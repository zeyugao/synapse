package client

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/zeyugao/synapse/internal/types"
	pb "github.com/zeyugao/synapse/internal/types/proto"
	"google.golang.org/protobuf/proto"
)

type Client struct {
	BaseUrl         string
	ServerURL       string
	ClientID        string
	WSAuthKey       string
	ApiKey          string
	models          []*types.ModelInfo
	conn            *websocket.Conn
	mu              sync.Mutex
	reconnMu        sync.Mutex
	reconnecting    bool
	closing         bool
	heartbeatTicker *time.Ticker
	cancelMap       map[string]context.CancelFunc
	syncTicker      *time.Ticker
	shutdownSignal  chan struct{}
	shutdownOnce    sync.Once
	activeRequests  int64 // Atomic counter
	Version         string
	Semver          string
	connClosed      chan struct{} // Channel to signal connection closed
	msgPool         sync.Pool
}

func NewClient(baseUrl, serverURL string, version string, semver string) *Client {
	client := &Client{
		BaseUrl:         baseUrl,
		ServerURL:       serverURL,
		ClientID:        generateClientID(),
		Version:         version,
		Semver:          semver,
		cancelMap:       make(map[string]context.CancelFunc),
		heartbeatTicker: time.NewTicker(15 * time.Second),
		syncTicker:      time.NewTicker(30 * time.Second),
		shutdownSignal:  make(chan struct{}),
		connClosed:      make(chan struct{}),
	}
	client.msgPool = sync.Pool{
		New: func() any {
			return &types.ClientMessage{}
		},
	}

	// Start the background goroutines only once
	go client.heartbeatLoop()
	go client.modelSyncLoop()
	go client.WaitForShutdown()

	return client
}

func (c *Client) fetchModels(silent bool) ([]*types.ModelInfo, error) {
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/models", c.BaseUrl), nil)
	if err != nil {
		if !silent {
			log.Printf("Failed to create model request: %v", err)
		}
		return nil, err
	}

	if c.ApiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.ApiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if !silent {
			log.Printf("Failed to get model list: %v", err)
		}
		return nil, err
	}
	defer resp.Body.Close()

	var modelsResp types.ModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&modelsResp); err != nil {
		if !silent {
			log.Printf("Failed to parse model response: %v", err)
		}
		return nil, err
	}

	cloned := cloneModelInfos(modelsResp.GetData())

	if !silent {
		log.Printf("Got %d models", len(cloned))
		for _, model := range cloned {
			if model == nil {
				continue
			}
			log.Printf("- %s", model.GetId())
		}
	}

	return cloned, nil
}

func (c *Client) Connect() error {
	models, err := c.fetchModels(false)
	if err != nil {
		log.Printf("Failed to fetch models during initial connection, using cached model list: %v", err)
	}
	if models == nil {
		models = []*types.ModelInfo{}
	}
	c.models = models

	// Use the configured server URL directly
	wsURL := c.ServerURL
	if c.WSAuthKey != "" {
		wsURL += "?ws_auth_key=" + c.WSAuthKey
	}

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		log.Printf("Failed to connect to server: %v", err)
		return err
	}

	// Reset connClosed channel if it was closed
	select {
	case <-c.connClosed:
		c.connClosed = make(chan struct{})
	default:
	}

	c.conn = conn
	serverToPrint := wsURL
	if c.WSAuthKey != "" {
		serverToPrint = strings.Replace(wsURL, c.WSAuthKey, "...", -1)
	}
	log.Printf("Successfully connected to server %s", serverToPrint)

	registration := c.getClientMessage()
	registration.Message = &pb.ClientMessage_Registration{
		Registration: &types.ClientRegistration{
			ClientId: c.ClientID,
			Models:   cloneModelInfos(c.models),
			Version:  c.Version,
			Semver:   c.Semver,
		},
	}
	if err := c.writeProto(registration); err != nil {
		log.Printf("Failed to send registration information: %v", err)
		conn.Close()
		c.putClientMessage(registration)
		return err
	}
	c.putClientMessage(registration)
	log.Printf("Sent client registration information, ID: %s", c.ClientID)

	// Start the connection handler in a separate goroutine
	go c.handleRequests()

	return nil
}

// heartbeatLoop replaces startHeartbeat and runs continuously
func (c *Client) heartbeatLoop() {
	for range c.heartbeatTicker.C {
		if c.closing {
			return
		}

		if c.isReconnecting() || c.conn == nil {
			time.Sleep(1 * time.Second)
			continue
		}

		heartbeat := c.getClientMessage()
		heartbeat.Message = &pb.ClientMessage_Heartbeat{
			Heartbeat: &types.Heartbeat{
				Timestamp: time.Now().Unix(),
			},
		}
		if err := c.writeProto(heartbeat); err != nil {
			log.Printf("Failed to send heartbeat: %v", err)
			c.signalConnectionClosed()
			c.putClientMessage(heartbeat)
			continue
		}
		c.putClientMessage(heartbeat)
	}
}

// modelSyncLoop replaces startModelSync and runs continuously
func (c *Client) modelSyncLoop() {
	for range c.syncTicker.C {
		if c.closing {
			return
		}

		if c.isReconnecting() || c.conn == nil {
			time.Sleep(1 * time.Second)
			continue
		}

		c.syncModels()
	}
}

func (c *Client) syncModels() {
	if c.closing || c.isReconnecting() || c.conn == nil {
		return
	}

	cleared := false
	newModels, err := c.fetchModels(true)
	if err != nil {
		if c.modelsEqual(nil) {
			return
		}
		log.Printf("Reporting empty model list due to upstream failure")
		newModels = nil
		cleared = true
	} else if c.modelsEqual(newModels) {
		return
	}

	if cleared {
		log.Printf("Cleared model list due to upstream failure")
	} else {
		log.Printf("Detected model changes, triggering update")
		log.Printf("Updated model list (%d models):", len(newModels))
		for _, model := range newModels {
			if model == nil {
				continue
			}
			log.Printf("- %s", model.GetId())
		}
	}
	c.models = newModels
	c.notifyModelUpdate(newModels)
}

func (c *Client) triggerModelSync() {
	go c.syncModels()
}

func (c *Client) handleRequests() {
	defer c.signalConnectionClosed()

	for {
		msg, err := readServerMessage(c.conn)
		if err != nil {
			if c.closing {
				return
			}

			// Add version error handling
			if closeErr, ok := err.(*websocket.CloseError); ok {
				switch closeErr.Code {
				case 4000:
					log.Fatalf("Version error: %s", closeErr.Text)
				case 4001:
					log.Fatalf("Version mismatch: %s", closeErr.Text)
				}
			}

			log.Printf("Connection error: %v, attempting to reconnect...", err)
			return
		}

		switch payload := msg.Message.(type) {
		case *pb.ServerMessage_ForwardRequest:
			if payload.ForwardRequest == nil {
				continue
			}
			go c.forwardRequest(payload.ForwardRequest)
		case *pb.ServerMessage_ClientClose:
			if payload.ClientClose == nil {
				continue
			}
			requestID := payload.ClientClose.GetRequestId()
			c.mu.Lock()
			cancel, exists := c.cancelMap[requestID]
			c.mu.Unlock()
			if exists {
				cancel()
			}
		case *pb.ServerMessage_Pong:
			// Ignore pong
		default:
			log.Printf("Unknown server message type: %T", payload)
		}
	}
}

// signalConnectionClosed notifies that the connection has been closed
func (c *Client) signalConnectionClosed() {
	select {
	case <-c.connClosed:
		// Channel already closed
	default:
		close(c.connClosed)
		go c.reconnect() // Trigger reconnection
	}
}

func (c *Client) writeProto(msg proto.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		return fmt.Errorf("connection is nil")
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	return c.conn.WriteMessage(websocket.BinaryMessage, data)
}

func readServerMessage(conn *websocket.Conn) (*types.ServerMessage, error) {
	_, data, err := conn.ReadMessage()
	if err != nil {
		return nil, err
	}

	msg := &types.ServerMessage{}
	if err := proto.Unmarshal(data, msg); err != nil {
		return nil, err
	}
	return msg, nil
}

func (c *Client) forwardRequest(req *types.ForwardRequest) {
	c.trackRequest(true)
	defer c.trackRequest(false)

	// Create a cancellable context
	ctx, cancel := context.WithCancel(context.Background())
	c.mu.Lock()
	c.cancelMap[req.GetRequestId()] = cancel
	c.mu.Unlock()

	// Clean up on function return
	defer func() {
		c.mu.Lock()
		delete(c.cancelMap, req.GetRequestId())
		c.mu.Unlock()
	}()

	var upstreamURL string
	if req.GetQuery() == "" {
		upstreamURL = fmt.Sprintf("%s%s", c.BaseUrl, req.GetPath())
	} else {
		upstreamURL = fmt.Sprintf("%s%s?%s", c.BaseUrl, req.GetPath(), req.GetQuery())
	}

	httpReq, err := http.NewRequestWithContext(ctx, req.GetMethod(), upstreamURL, bytes.NewReader(req.GetBody()))
	if err != nil {
		log.Printf("Failed to create upstream request: %v", err)
		errResp := &types.ForwardResponse{
			RequestId:  req.GetRequestId(),
			StatusCode: int32(http.StatusInternalServerError),
			Body:       []byte(fmt.Sprintf("Error: %v", err)),
			Kind:       types.ResponseKindNormal,
		}
		if err := c.sendForwardResponse(errResp); err != nil {
			log.Printf("Failed to send error response: %v", err)
		}
		return
	}

	// Set request headers
	for k, values := range types.ProtoToHTTPHeader(req.GetHeader()) {
		for _, v := range values {
			httpReq.Header.Add(k, v)
		}
	}

	if c.ApiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.ApiKey)
	}

	// Ensure Content-Type is set
	if httpReq.Header.Get("Content-Type") == "" {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		log.Printf("Upstream request execution failed: %v", err)
		if isConnectionRefused(err) {
			c.triggerModelSync()
		}
		errResp := &types.ForwardResponse{
			RequestId:  req.GetRequestId(),
			StatusCode: int32(http.StatusInternalServerError),
			Body:       []byte(fmt.Sprintf("Error: %v", err)),
			Kind:       types.ResponseKindNormal,
		}
		if err := c.sendForwardResponse(errResp); err != nil {
			log.Printf("Failed to send error response: %v", err)
		}
		return
	}
	defer resp.Body.Close()

	if mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err == nil && mediaType == "text/event-stream" {
		c.handleStreamResponse(resp.Body, req.GetRequestId(), ctx)
	} else {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("Failed to read response body: %v", err)
			errResp := &types.ForwardResponse{
				RequestId:  req.GetRequestId(),
				StatusCode: int32(http.StatusInternalServerError),
				Body:       []byte(fmt.Sprintf("Error: %v", err)),
				Kind:       types.ResponseKindNormal,
			}
			if err := c.sendForwardResponse(errResp); err != nil {
				log.Printf("Failed to send error response: %v", err)
			}
			return
		}

		// Create forward response structure
		forwardResp := &types.ForwardResponse{
			RequestId:  req.GetRequestId(),
			StatusCode: int32(resp.StatusCode),
			Header:     types.HTTPHeaderToProto(resp.Header.Clone()),
			Body:       body,
			Kind:       types.ResponseKindNormal,
		}

		if err := c.sendForwardResponse(forwardResp); err != nil {
			log.Printf("Failed to send response: %v", err)
			return
		}
	}
}

func (c *Client) handleStreamResponse(reader io.Reader, requestID string, ctx context.Context) {
	scanner := bufio.NewScanner(reader)
	var buffer bytes.Buffer

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			log.Printf("Request %s has been cancelled", requestID)
			return
		default:
			line := scanner.Bytes()
			if bytes.HasPrefix(line, []byte("data: ")) {
				content := bytes.TrimSpace(line[6:])
				if bytes.Equal(content, []byte("[DONE]")) {
					if err := c.sendForwardResponse(&types.ForwardResponse{
						RequestId:  requestID,
						Kind:       types.ResponseKindStream,
						Done:       true,
						StatusCode: int32(http.StatusOK),
					}); err != nil {
						log.Printf("Failed to send stream end marker: %v", err)
					}
					return
				}

				buffer.Write(content)
			}
			// Send a chunk of data when an empty line is encountered
			if len(line) == 0 {
				if buffer.Len() > 0 {
					chunkData := append([]byte(nil), buffer.Bytes()...)
					if err := c.sendForwardResponse(&types.ForwardResponse{
						RequestId:  requestID,
						Kind:       types.ResponseKindStream,
						StatusCode: int32(http.StatusOK),
						Body:       chunkData,
					}); err != nil {
						log.Printf("Failed to send stream data chunk: %v", err)
						return
					}
					buffer.Reset()
				}
			}
		}
	}
}

func generateClientID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		log.Printf("Failed to generate client ID: %v", err)
		return "fallback-id"
	}
	return hex.EncodeToString(b)
}

func (c *Client) reconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closing {
		return
	}

	c.setReconnecting(true)
	defer func() {
		c.setReconnecting(false)
	}()

	// Close the current connection if it exists
	if c.conn != nil {
		c.conn.Close()
		c.conn = nil
	}

	retryWait := 1 * time.Second
	maxRetryWait := 30 * time.Second

	for {
		log.Printf("Attempting to reconnect...")
		if err := c.Connect(); err == nil {
			log.Printf("Reconnected successfully")
			return
		}

		if retryWait < maxRetryWait {
			retryWait *= 2
		}
		log.Printf("Reconnection failed, retrying in %v...", retryWait)
		time.Sleep(retryWait)
	}
}

func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closing = true
	if c.conn != nil {
		c.conn.Close()
	}
	c.heartbeatTicker.Stop()
	c.syncTicker.Stop()
}

func (c *Client) modelsEqual(newModels []*types.ModelInfo) bool {
	if len(c.models) != len(newModels) {
		return false
	}

	oldMap := make(map[string]struct{})
	for _, m := range c.models {
		if m == nil {
			continue
		}
		oldMap[m.GetId()] = struct{}{}
	}

	for _, m := range newModels {
		if m == nil {
			continue
		}
		if _, ok := oldMap[m.GetId()]; !ok {
			return false
		}
		delete(oldMap, m.GetId())
	}
	return len(oldMap) == 0
}

func (c *Client) notifyModelUpdate(models []*types.ModelInfo) {
	msg := c.getClientMessage()
	msg.Message = &pb.ClientMessage_ModelUpdate{
		ModelUpdate: &types.ModelUpdateRequest{
			ClientId: c.ClientID,
			Models:   cloneModelInfos(models),
		},
	}

	if err := c.writeProto(msg); err != nil {
		log.Printf("Failed to send model update notification: %v", err)
	}
	c.putClientMessage(msg)
}

func (c *Client) Shutdown() {
	c.shutdownOnce.Do(func() {
		close(c.shutdownSignal)
	})
}

func (c *Client) isReconnecting() bool {
	c.reconnMu.Lock()
	defer c.reconnMu.Unlock()
	return c.reconnecting
}

func (c *Client) setReconnecting(reconnecting bool) {
	c.reconnMu.Lock()
	defer c.reconnMu.Unlock()
	c.reconnecting = reconnecting
}

func (c *Client) WaitForShutdown() {
	sigChan := make(chan os.Signal, 2)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	select {
	case <-sigChan:
		if c.isReconnecting() || c.conn == nil {
			break
		}
		log.Println("Notifying server to update model list")
		c.notifyModelUpdate(nil)
	case <-c.shutdownSignal:
		return
	}

	// Add force shutdown handling
	select {
	case <-sigChan:
		if c.isReconnecting() || c.conn == nil {
			break
		}
		log.Println("Force shutdown, notifying server to interrupt requests")
		// Send force shutdown notification
		forceReq := c.getClientMessage()
		forceReq.Message = &pb.ClientMessage_ForceShutdown{
			ForceShutdown: &types.ForceShutdownRequest{
				ClientId: c.ClientID,
			},
		}
		if err := c.writeProto(forceReq); err != nil {
			log.Printf("Failed to send force shutdown notification: %v", err)
		}
		c.putClientMessage(forceReq)

		log.Println("Forcing shutdown...")
		os.Exit(1)
	case <-c.waitForRequests():
		log.Println("All requests processed, exiting safely")
	}
	os.Exit(0)
}

func (c *Client) trackRequest(start bool) {
	if start {
		atomic.AddInt64(&c.activeRequests, 1)
	} else {
		atomic.AddInt64(&c.activeRequests, -1)
	}
}

func (c *Client) waitForRequests() <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		for atomic.LoadInt64(&c.activeRequests) > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		close(ch)
	}()
	return ch
}

func (c *Client) getClientMessage() *types.ClientMessage {
	if msg := c.msgPool.Get(); msg != nil {
		m := msg.(*types.ClientMessage)
		proto.Reset(m)
		return m
	}
	return &types.ClientMessage{}
}

func (c *Client) putClientMessage(msg *types.ClientMessage) {
	if msg == nil {
		return
	}
	proto.Reset(msg)
	c.msgPool.Put(msg)
}

func (c *Client) sendForwardResponse(resp *types.ForwardResponse) error {
	msg := c.getClientMessage()
	msg.Message = &pb.ClientMessage_ForwardResponse{ForwardResponse: resp}
	err := c.writeProto(msg)
	c.putClientMessage(msg)
	return err
}

func isConnectionRefused(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if errors.Is(opErr.Err, syscall.ECONNREFUSED) {
			return true
		}
	}
	return strings.Contains(err.Error(), "connection refused")
}

func cloneModelInfos(src []*types.ModelInfo) []*types.ModelInfo {
	if len(src) == 0 {
		return nil
	}
	out := make([]*types.ModelInfo, 0, len(src))
	for _, model := range src {
		if model == nil {
			out = append(out, nil)
			continue
		}
		cloned, ok := proto.Clone(model).(*types.ModelInfo)
		if !ok {
			continue
		}
		out = append(out, cloned)
	}
	return out
}
