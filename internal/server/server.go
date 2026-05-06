package server

import (
	"bytes"
	cryptoRand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"
	"github.com/zeyugao/synapse/internal/types"
	pb "github.com/zeyugao/synapse/internal/types/proto"
	"google.golang.org/protobuf/proto"
)

const responseChannelBuffer = 8

type Server struct {
	clients          map[string]*Client
	modelClients     map[string]map[string][]string // group -> model -> []clientKey
	mu               sync.RWMutex
	upgrader         websocket.Upgrader
	pendingRequests  map[string]chan *types.ForwardResponse
	reqMu            sync.RWMutex
	authConfig       atomic.Value
	clientRequests   map[string]map[string]struct{} // clientKey -> set of requestIDs
	version          string
	clientBinaryPath string
	clientMsgPool    sync.Pool
	serverMsgPool    sync.Pool
	pongFrame        []byte
	modelsCache      map[string][]byte
	modelsCacheDirty map[string]bool
}

type Client struct {
	id             string
	groupName      string
	conn           *websocket.Conn
	models         []*types.ModelInfo
	mu             sync.Mutex
	activeRequests int64
	processingMu   sync.RWMutex
	modelAverages  map[string]float64
}

func (c *Client) writeProto(msg proto.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	data, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	return c.conn.WriteMessage(websocket.BinaryMessage, data)
}

func (c *Client) writeRaw(frame []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn.WriteMessage(websocket.BinaryMessage, frame)
}

func (c *Client) incrementActive() {
	atomic.AddInt64(&c.activeRequests, 1)
}

func (c *Client) decrementActive() {
	atomic.AddInt64(&c.activeRequests, -1)
}

func (c *Client) loadActive() int64 {
	return atomic.LoadInt64(&c.activeRequests)
}

func (c *Client) updateAverageProcessing(stats []*types.ModelProcessingStats) {
	if len(stats) == 0 {
		return
	}
	c.processingMu.Lock()
	if c.modelAverages == nil {
		c.modelAverages = make(map[string]float64)
	}
	for _, stat := range stats {
		if stat == nil {
			continue
		}
		modelID := stat.GetModelId()
		if modelID == "" {
			continue
		}
		avg := stat.GetAverageProcessingMs()
		if avg <= 0 {
			delete(c.modelAverages, modelID)
			continue
		}
		c.modelAverages[modelID] = avg
	}
	c.processingMu.Unlock()
}

func (c *Client) processingForModel(modelID string) float64 {
	if modelID == "" {
		return 0
	}
	c.processingMu.RLock()
	defer c.processingMu.RUnlock()
	return c.modelAverages[modelID]
}

func NewServer(apiAuthKey, wsAuthKey string, version string, clientBinaryPath string) *Server {
	return newServer(newLegacyAuthConfig(apiAuthKey, wsAuthKey), version, clientBinaryPath)
}

func NewServerWithConfig(cfg *Config, version string, clientBinaryPath string) (*Server, error) {
	authConfig, err := newAuthConfigFromConfig(cfg)
	if err != nil {
		return nil, err
	}
	return newServer(authConfig, version, clientBinaryPath), nil
}

func newServer(authConfig *authConfig, version string, clientBinaryPath string) *Server {
	server := &Server{
		clients:         make(map[string]*Client),
		modelClients:    make(map[string]map[string][]string),
		pendingRequests: make(map[string]chan *types.ForwardResponse),
		clientRequests:  make(map[string]map[string]struct{}),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
		version:          version,
		clientBinaryPath: clientBinaryPath,
		modelsCache:      make(map[string][]byte),
		modelsCacheDirty: make(map[string]bool),
	}
	server.authConfig.Store(authConfig)

	for _, groupName := range authConfig.groupNames {
		server.modelClients[groupName] = make(map[string][]string)
		server.modelsCacheDirty[groupName] = true
	}

	server.clientMsgPool = sync.Pool{
		New: func() any {
			return &types.ClientMessage{}
		},
	}

	server.serverMsgPool = sync.Pool{
		New: func() any {
			return &types.ServerMessage{}
		},
	}

	frame, err := proto.Marshal(&types.ServerMessage{
		Message: &pb.ServerMessage_Pong{Pong: &types.Pong{}},
	})
	if err != nil {
		log.Fatalf("failed to precompute pong frame: %v", err)
	}
	server.pongFrame = frame

	return server
}

func (s *Server) ReloadConfig(cfg *Config) error {
	authConfig, err := newAuthConfigFromConfig(cfg)
	if err != nil {
		return err
	}

	s.authConfig.Store(authConfig)

	s.mu.Lock()
	for _, groupName := range authConfig.groupNames {
		if s.modelClients[groupName] == nil {
			s.modelClients[groupName] = make(map[string][]string)
		}
		s.modelsCacheDirty[groupName] = true
	}
	s.mu.Unlock()

	return nil
}

func (s *Server) currentAuthConfig() *authConfig {
	return s.authConfig.Load().(*authConfig)
}

func makeClientKey(groupName, clientID string) string {
	return groupName + "\x00" + clientID
}

func generateRequestID() string {
	b := make([]byte, 16)
	cryptoRand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) readClientMessage(conn *websocket.Conn) (*types.ClientMessage, error) {
	_, data, err := conn.ReadMessage()
	if err != nil {
		return nil, err
	}

	msg := s.getClientMessage()
	if err := proto.Unmarshal(data, msg); err != nil {
		s.putClientMessage(msg)
		return nil, err
	}
	return msg, nil
}

func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	clientIP := r.RemoteAddr
	if forwardedFor := r.Header.Get("X-Forwarded-For"); forwardedFor != "" {
		clientIP = forwardedFor
	} else if realIP := r.Header.Get("X-Real-IP"); realIP != "" {
		clientIP = realIP
	}

	groupName, authorized := s.currentAuthConfig().matchWebSocketGroup(r.URL.Query().Get("ws_auth_key"))
	if !authorized {
		log.Printf("WebSocket authentication failed from %s", clientIP)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed from %s: %v", clientIP, err)
		http.Error(w, "Could not upgrade connection", http.StatusInternalServerError)
		return
	}

	initialMsg, err := s.readClientMessage(conn)
	if err != nil {
		log.Printf("Failed to read registration information from %s: %v", clientIP, err)
		conn.Close()
		return
	}

	registration := initialMsg.GetRegistration()
	if registration == nil {
		log.Printf("Client from %s did not send registration as first message, connection refused", clientIP)
		conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(4000, "Registration required"))
		conn.Close()
		s.putClientMessage(initialMsg)
		return
	}

	// Version check logic
	clientID := registration.GetClientId()
	version := registration.GetVersion()
	if version == "" {
		log.Printf("Client %s from %s did not provide a version number, connection refused", clientID, clientIP)
		conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(4000, "Version required"))
		conn.Close()
		s.putClientMessage(initialMsg)
		return
	}

	if version != s.version {
		log.Printf("Client version mismatch (client: %s, server: %s) from %s", version, s.version, clientIP)
		s.notifyClientUpdate(conn, r)
		conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(4001, fmt.Sprintf("Client version %s does not match server version %s", version, s.version)))
		conn.Close()
		s.putClientMessage(initialMsg)
		return
	}

	models := cloneModelInfos(registration.GetModels())
	clientKey := makeClientKey(groupName, clientID)
	log.Printf("New client connected: %s (group=%s), registered models: %d", clientID, groupName, len(models))

	s.mu.Lock()
	client := &Client{
		id:            clientID,
		groupName:     groupName,
		conn:          conn,
		models:        models,
		modelAverages: make(map[string]float64),
	}
	s.clients[clientKey] = client

	// Update model to client mapping
	if s.modelClients[groupName] == nil {
		s.modelClients[groupName] = make(map[string][]string)
	}
	for _, model := range models {
		if model == nil {
			continue
		}
		modelID := model.GetId()
		s.modelClients[groupName][modelID] = append(s.modelClients[groupName][modelID], clientKey)
	}
	s.modelsCacheDirty[groupName] = true
	s.mu.Unlock()

	s.putClientMessage(initialMsg)

	// Add goroutine for handling responses
	go s.handleClientResponses(clientKey, client)
}

func (s *Server) notifyClientUpdate(conn *websocket.Conn, r *http.Request) {
	downloadURL := s.buildClientDownloadURL(r)
	msg := &types.ServerMessage{
		Message: &pb.ServerMessage_UpdateRequired{
			UpdateRequired: &types.UpdateRequired{
				Version:     s.version,
				DownloadUrl: downloadURL,
			},
		},
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		log.Printf("Failed to marshal update notification: %v", err)
		return
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		log.Printf("Failed to send update notification: %v", err)
	}
}

func (s *Server) buildClientDownloadURL(r *http.Request) string {
	if s.clientBinaryPath == "" {
		return ""
	}
	if _, err := os.Stat(s.clientBinaryPath); err != nil {
		if os.IsNotExist(err) {
			log.Printf("Client binary not found at %s", s.clientBinaryPath)
		} else {
			log.Printf("Failed to stat client binary: %v", err)
		}
		return ""
	}

	protoScheme := "http"
	if r.TLS != nil {
		protoScheme = "https"
	}
	if forwardedProto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); forwardedProto != "" {
		protoScheme = forwardedProto
	}

	host := strings.TrimSpace(r.Host)
	if forwardedHost := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); forwardedHost != "" {
		host = strings.TrimSpace(strings.Split(forwardedHost, ",")[0])
	}
	if host == "" && r.URL != nil {
		host = strings.TrimSpace(r.URL.Host)
	}
	if host == "" {
		host = "localhost"
	}

	if forwardedPort := strings.TrimSpace(r.Header.Get("X-Forwarded-Port")); forwardedPort != "" {
		port := strings.TrimSpace(strings.Split(forwardedPort, ",")[0])
		if port != "" && !hostHasExplicitPort(host) {
			host = net.JoinHostPort(stripIPv6Brackets(host), port)
		}
	}

	return fmt.Sprintf("%s://%s/getclient", protoScheme, host)
}

func (s *Server) handleClientResponses(clientKey string, client *Client) {
	defer func() {
		client.conn.Close()
		s.unregisterClient(clientKey)
	}()

	for {
		msg, err := s.readClientMessage(client.conn)
		if err != nil {
			log.Printf("Failed to read client %s message: %v", client.id, err)
			return
		}

		switch payload := msg.Message.(type) {
		case *pb.ClientMessage_ForwardResponse:
			if payload.ForwardResponse != nil {
				s.handleForwardResponse(cloneForwardResponse(payload.ForwardResponse))
			}
		case *pb.ClientMessage_Heartbeat:
			if payload.Heartbeat != nil {
				client.updateAverageProcessing(payload.Heartbeat.GetProcessing())
			}
			if err := client.writeRaw(s.pongFrame); err != nil {
				log.Printf("Failed to send pong response: %v", err)
			}
		case *pb.ClientMessage_ModelUpdate:
			if payload.ModelUpdate != nil {
				s.handleModelUpdate(clientKey, payload.ModelUpdate)
			}
		case *pb.ClientMessage_Unregister:
			if payload.Unregister != nil {
				log.Printf("Received unregister request from client %s", payload.Unregister.GetClientId())
				s.unregisterClient(clientKey)
				s.putClientMessage(msg)
				return
			}
		case *pb.ClientMessage_ForceShutdown:
			if payload.ForceShutdown != nil {
				s.handleForceShutdown(clientKey, payload.ForceShutdown)
			}
		default:
			log.Printf("Unknown client message type: %T", payload)
		}

		s.putClientMessage(msg)
	}
}

func (s *Server) handleForwardResponse(resp *types.ForwardResponse) {
	requestID := resp.GetRequestId()

	s.reqMu.RLock()
	respChan, exists := s.pendingRequests[requestID]
	s.reqMu.RUnlock()

	if !exists || respChan == nil {
		return
	}

	func() {
		defer func() {
			if r := recover(); r != nil {
				// Channel may have been closed concurrently; ignore.
			}
		}()
		respChan <- resp
	}()

	kind := resp.GetKind()
	if (kind == types.ResponseKindStream && resp.GetDone()) || kind == types.ResponseKindNormal {
		s.reqMu.Lock()
		if ch, ok := s.pendingRequests[requestID]; ok && ch == respChan {
			delete(s.pendingRequests, requestID)
			close(respChan)
		}
		s.reqMu.Unlock()
	}
}

func (s *Server) handleModels(w http.ResponseWriter, groupName string) {
	cache := s.getModelsCache(groupName)
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(cache); err != nil {
		log.Printf("Failed to write models response: %v", err)
	}
}

func (s *Server) handleAPIRequest(w http.ResponseWriter, r *http.Request) {
	groupName, err := s.currentAuthConfig().matchAPIGroup(r.Header)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	path := r.URL.Path
	if strings.HasPrefix(path, "/v1/") {
		// Remove the /v1 prefix from the path
		path = strings.TrimPrefix(path, "/v1")
	}

	if path == "/models" {
		s.handleModels(w, groupName)
		return
	}

	// Read request body
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Error reading request body", http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewBuffer(body))

	var modelReq struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &modelReq); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	// Generate request ID
	requestID := generateRequestID()

	// Create response channel
	respChan := make(chan *types.ForwardResponse, responseChannelBuffer)
	s.reqMu.Lock()
	s.pendingRequests[requestID] = respChan
	s.reqMu.Unlock()

	// Build forward request
	req := &types.ForwardRequest{
		RequestId: requestID,
		Model:     modelReq.Model,
		Method:    r.Method,
		Path:      path,
		Query:     r.URL.RawQuery,
		Header:    types.HTTPHeaderToProto(r.Header.Clone()),
		Body:      body,
	}

	// Select client based on model and load balancing
	s.mu.RLock()
	clientIDsForModel, modelSupported := s.modelClients[groupName][req.Model]

	var candidateClients []*Client
	var candidateClientKeys []string

	if modelSupported && len(clientIDsForModel) > 0 {
		for _, clientKey := range clientIDsForModel {
			if c, exists := s.clients[clientKey]; exists {
				candidateClients = append(candidateClients, c)
				candidateClientKeys = append(candidateClientKeys, clientKey)
			}
		}
	}
	s.mu.RUnlock()

	if len(candidateClients) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]interface{}{
				"code":    "bad_request",
				"message": fmt.Sprintf("Unsupported model or no available clients: %s", req.Model),
				"type":    "invalid_request_error",
				"param":   nil,
			},
		})
		return
	}

	var tiedIndexes []int
	minLoad := int64(-1)

	for i, currentClient := range candidateClients {
		load := currentClient.loadActive()

		if minLoad == -1 || load < minLoad {
			minLoad = load
			tiedIndexes = tiedIndexes[:0]
			tiedIndexes = append(tiedIndexes, i)
		} else if load == minLoad {
			tiedIndexes = append(tiedIndexes, i)
		}
	}

	bestClient, bestClientKey := selectClientByMetrics(candidateClients, candidateClientKeys, tiedIndexes, req.GetModel())
	if bestClient == nil {
		http.Error(w, "No available clients", http.StatusServiceUnavailable)
		return
	}

	client := bestClient // Use this client object to send the request
	clientKey := bestClientKey

	// Add request to client's active requests map
	s.reqMu.Lock()
	if s.clientRequests[clientKey] == nil {
		s.clientRequests[clientKey] = make(map[string]struct{})
	}
	s.clientRequests[clientKey][requestID] = struct{}{}
	s.reqMu.Unlock()
	client.incrementActive()

	// Wait for response
	defer func() {
		client.decrementActive()
		s.reqMu.Lock()
		// Remove request from the client's active request set
		if reqs, ok := s.clientRequests[clientKey]; ok {
			delete(reqs, requestID)
			if len(reqs) == 0 {
				delete(s.clientRequests, clientKey)
			}
		}
		// Clean up pendingRequests
		if ch, exists := s.pendingRequests[requestID]; exists {
			delete(s.pendingRequests, requestID)
			close(ch)
		}
		s.reqMu.Unlock()
	}()

	forwardMsg := s.getServerMessage()
	forwardMsg.Message = &pb.ServerMessage_ForwardRequest{ForwardRequest: req}
	if err := client.writeProto(forwardMsg); err != nil {
		s.putServerMessage(forwardMsg)
		http.Error(w, "Failed to forward request", http.StatusInternalServerError)
		return
	}
	s.putServerMessage(forwardMsg)

	select {
	case resp, ok := <-respChan:
		if !ok || resp == nil {
			return
		}
		for k, values := range types.ProtoToHTTPHeader(resp.GetHeader()) {
			for _, v := range values {
				w.Header().Add(k, v)
			}
		}
		if resp.GetKind() == types.ResponseKindNormal {
			if resp.GetStatusCode() != 0 {
				w.WriteHeader(int(resp.GetStatusCode()))
			}
			if len(resp.GetBody()) > 0 {
				w.Write(resp.GetBody())
			}
		} else {
			if w.Header().Get("Content-Type") == "" {
				w.Header().Set("Content-Type", "text/event-stream")
			}
			w.Header().Del("Content-Length")
			if resp.GetStatusCode() != 0 {
				w.WriteHeader(int(resp.GetStatusCode()))
			}
			flusher, _ := w.(http.Flusher)

			if len(resp.GetBody()) > 0 {
				if _, err := w.Write(resp.GetBody()); err != nil {
					log.Printf("Failed to write stream chunk: %v", err)
					return
				}
			}
			if flusher != nil {
				flusher.Flush()
			}
			if resp.GetDone() {
				return
			}

			// Continue processing subsequent chunks
			for {
				select {
				case chunk, ok := <-respChan:
					if !ok || chunk == nil {
						return
					}
					if len(chunk.GetBody()) > 0 {
						if _, err := w.Write(chunk.GetBody()); err != nil {
							log.Printf("Failed to write stream chunk: %v", err)
							return
						}
					}
					if flusher != nil {
						flusher.Flush()
					}
					if chunk.GetDone() {
						return
					}
				case <-r.Context().Done():
					// Send client close request
					closeReq := s.getServerMessage()
					closeReq.Message = &pb.ServerMessage_ClientClose{ClientClose: &types.ClientClose{RequestId: requestID}}
					if err := client.writeProto(closeReq); err != nil {
						log.Printf("Failed to send close request: %v", err)
					}
					s.putServerMessage(closeReq)
					return
				}
			}
		}
	case <-r.Context().Done():
		// Send client close request
		closeReq := s.getServerMessage()
		closeReq.Message = &pb.ServerMessage_ClientClose{ClientClose: &types.ClientClose{RequestId: requestID}}
		if err := client.writeProto(closeReq); err != nil {
			log.Printf("Failed to send close request: %v", err)
		}
		s.putServerMessage(closeReq)
		return
	}
}

func (s *Server) unregisterClient(clientKey string) {
	s.mu.Lock()
	client, exists := s.clients[clientKey]
	if !exists {
		s.mu.Unlock()
		return
	}

	// Clean up model to client mapping
	groupModels := s.modelClients[client.groupName]
	for _, model := range client.models {
		if model == nil {
			continue
		}
		modelID := model.GetId()
		if clients, ok := groupModels[modelID]; ok {
			newClients := make([]string, 0, len(clients))
			for _, existingClientKey := range clients {
				if existingClientKey != clientKey {
					newClients = append(newClients, existingClientKey)
				}
			}

			if len(newClients) > 0 {
				groupModels[modelID] = newClients
			} else {
				delete(groupModels, modelID)
			}
		}
	}

	delete(s.clients, clientKey)
	currentClientCount := len(s.clients)
	s.modelsCacheDirty[client.groupName] = true
	s.mu.Unlock()

	// Clean up clientRequests associated with this client
	s.reqMu.Lock()
	delete(s.clientRequests, clientKey)
	s.reqMu.Unlock()

	log.Printf("Client %s (group=%s) unregistered, remaining clients: %d", client.id, client.groupName, currentClientCount)
}

func (s *Server) handleModelUpdate(clientKey string, update *types.ModelUpdateRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()

	client := s.clients[clientKey]
	if client == nil {
		log.Printf("Received model update for unknown client %s", update.GetClientId())
		return
	}
	groupModels := s.modelClients[client.groupName]
	if groupModels == nil {
		groupModels = make(map[string][]string)
		s.modelClients[client.groupName] = groupModels
	}
	for _, model := range client.models {
		if model == nil {
			continue
		}
		modelID := model.GetId()
		if clients, ok := groupModels[modelID]; ok {
			newClients := make([]string, 0, len(clients))
			for _, existingClientKey := range clients {
				if existingClientKey != clientKey {
					newClients = append(newClients, existingClientKey)
				}
			}
			if len(newClients) > 0 {
				groupModels[modelID] = newClients
			} else {
				delete(groupModels, modelID)
			}
		}
	}

	client.models = cloneModelInfos(update.GetModels())
	for _, model := range client.models {
		if model == nil {
			continue
		}
		modelID := model.GetId()
		groupModels[modelID] = append(groupModels[modelID], clientKey)
	}

	log.Printf("Updated model list for client %s (group=%s), current number of models: %d", client.id, client.groupName, len(client.models))
	s.modelsCacheDirty[client.groupName] = true
}

func (s *Server) handleForceShutdown(clientKey string, req *types.ForceShutdownRequest) {
	s.reqMu.Lock()
	defer s.reqMu.Unlock()

	var requestsToCancel []string
	if clientActiveRequests, clientExists := s.clientRequests[clientKey]; clientExists {
		for requestID := range clientActiveRequests {
			requestsToCancel = append(requestsToCancel, requestID)
		}
	}

	if len(requestsToCancel) == 0 {
		log.Printf("No pending requests found to forcibly close for client %s", req.GetClientId())
		return
	}

	// Send cancel response and clean up
	for _, requestID := range requestsToCancel {
		if ch, exists := s.pendingRequests[requestID]; exists {
			ch <- &types.ForwardResponse{
				RequestId:  requestID,
				StatusCode: int32(http.StatusGatewayTimeout),
				Body:       []byte("Upstream client force shutdown"),
				Kind:       types.ResponseKindNormal,
				Done:       true,
			}
			close(ch)
			delete(s.pendingRequests, requestID)
		}
		if reqsForClient, ok := s.clientRequests[clientKey]; ok {
			delete(reqsForClient, requestID)
			if len(reqsForClient) == 0 {
				delete(s.clientRequests, clientKey)
			}
		}
	}
	log.Printf("Forcibly closed %d pending requests for client %s", len(requestsToCancel), req.GetClientId())
}

func (s *Server) getClientMessage() *types.ClientMessage {
	if msg := s.clientMsgPool.Get(); msg != nil {
		m := msg.(*types.ClientMessage)
		proto.Reset(m)
		return m
	}
	return &types.ClientMessage{}
}

func (s *Server) putClientMessage(msg *types.ClientMessage) {
	if msg == nil {
		return
	}
	proto.Reset(msg)
	s.clientMsgPool.Put(msg)
}

func (s *Server) getServerMessage() *types.ServerMessage {
	if msg := s.serverMsgPool.Get(); msg != nil {
		m := msg.(*types.ServerMessage)
		proto.Reset(m)
		return m
	}
	return &types.ServerMessage{}
}

func (s *Server) putServerMessage(msg *types.ServerMessage) {
	if msg == nil {
		return
	}
	proto.Reset(msg)
	s.serverMsgPool.Put(msg)
}

func (s *Server) getModelsCache(groupName string) []byte {
	s.mu.RLock()
	if !s.modelsCacheDirty[groupName] && s.modelsCache[groupName] != nil {
		cacheCopy := append([]byte(nil), s.modelsCache[groupName]...)
		s.mu.RUnlock()
		return cacheCopy
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.modelsCacheDirty[groupName] && s.modelsCache[groupName] != nil {
		return append([]byte(nil), s.modelsCache[groupName]...)
	}
	cache := s.buildModelsCacheLocked(groupName)
	s.modelsCache[groupName] = cache
	s.modelsCacheDirty[groupName] = false
	return append([]byte(nil), cache...)
}

func (s *Server) buildModelsCacheLocked(groupName string) []byte {
	type modelGroup struct {
		models    []*types.ModelInfo
		clientIDs map[string]struct{}
	}

	modelGroups := make(map[string]*modelGroup)
	for clientKey, client := range s.clients {
		if client.groupName != groupName {
			continue
		}
		for _, model := range client.models {
			if model == nil {
				continue
			}

			modelID := model.GetId()
			group := modelGroups[modelID]
			if group == nil {
				group = &modelGroup{
					clientIDs: make(map[string]struct{}),
				}
				modelGroups[modelID] = group
			}

			group.models = append(group.models, model)
			group.clientIDs[clientKey] = struct{}{}
		}
	}

	entries := make([]map[string]any, 0, len(modelGroups))
	for modelID, group := range modelGroups {
		entry := mergedModelPayload(group.models, modelID)
		entry["client_count"] = len(group.clientIDs)
		entries = append(entries, entry)
	}

	sort.Slice(entries, func(i, j int) bool {
		left, _ := entries[i]["id"].(string)
		right, _ := entries[j]["id"].(string)
		return left < right
	})

	response := struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}{
		Object: "list",
		Data:   entries,
	}

	data, err := json.Marshal(response)
	if err != nil {
		log.Printf("Failed to marshal models cache: %v", err)
		return []byte(`{"object":"list","data":[]}`)
	}
	return data
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
		out = append(out, cloneModelInfo(model))
	}
	return out
}

func cloneModelInfo(src *types.ModelInfo) *types.ModelInfo {
	if src == nil {
		return nil
	}
	cloned, ok := proto.Clone(src).(*types.ModelInfo)
	if !ok {
		return nil
	}
	return cloned
}

func selectClientByMetrics(clients []*Client, ids []string, indexes []int, model string) (*Client, string) {
	if len(indexes) == 0 {
		return nil, ""
	}

	if len(indexes) == 1 {
		idx := indexes[0]
		return clients[idx], ids[idx]
	}

	var sumAvg float64
	var countAvg int
	for _, idx := range indexes {
		if avg := clients[idx].processingForModel(model); avg > 0 {
			sumAvg += avg
			countAvg++
		}
	}

	if countAvg == 0 {
		idx := indexes[rand.Intn(len(indexes))]
		return clients[idx], ids[idx]
	}

	groupAvg := sumAvg / float64(countAvg)

	weights := make([]float64, len(indexes))
	var total float64

	for i, idx := range indexes {
		client := clients[idx]
		loadWeight := float64(client.loadActive()) + 1
		avg := client.processingForModel(model)
		if avg <= 0 {
			avg = groupAvg
		}
		weight := 1 / (loadWeight * avg)
		weights[i] = weight
		total += weight
	}

	if total <= 0 {
		idx := indexes[rand.Intn(len(indexes))]
		return clients[idx], ids[idx]
	}

	pick := rand.Float64() * total
	for i, weight := range weights {
		pick -= weight
		if pick <= 0 {
			idx := indexes[i]
			return clients[idx], ids[idx]
		}
	}

	idx := indexes[len(indexes)-1]
	return clients[idx], ids[idx]
}

func cloneForwardResponse(src *types.ForwardResponse) *types.ForwardResponse {
	if src == nil {
		return nil
	}
	clone := &types.ForwardResponse{
		RequestId:  src.GetRequestId(),
		StatusCode: src.GetStatusCode(),
		Body:       append([]byte(nil), src.GetBody()...),
		Kind:       src.GetKind(),
		Done:       src.GetDone(),
		Timestamp:  src.GetTimestamp(),
	}
	clone.Header = cloneHeaderEntries(src.GetHeader())
	return clone
}

func cloneHeaderEntries(entries []*types.HeaderEntry) []*types.HeaderEntry {
	if len(entries) == 0 {
		return nil
	}
	out := make([]*types.HeaderEntry, len(entries))
	for i, entry := range entries {
		if entry == nil {
			continue
		}
		out[i] = &types.HeaderEntry{
			Key:    entry.GetKey(),
			Values: append([]string(nil), entry.GetValues()...),
		}
	}
	return out
}

func (s *Server) handleGetClient(w http.ResponseWriter, r *http.Request) {
	// Check if client binary exists
	if _, err := os.Stat(s.clientBinaryPath); os.IsNotExist(err) {
		log.Printf("Client binary not found at %s", s.clientBinaryPath)
		http.Error(w, "Client binary not available", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", "synapse-client"))
	http.ServeFile(w, r, s.clientBinaryPath)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(`<html>
    <head>
        <title>Synapse Server</title>
    </head>
    <body>
        <h1>Synapse Server</h1>
        <p>Version: ` + s.version + `</p>
    </body>
</html>`))
}

func (s *Server) handleGetServerVersion(w http.ResponseWriter, r *http.Request) {
	w.Write([]byte(s.version))
}

func (s *Server) oneKeyScript(w http.ResponseWriter, r *http.Request) {
	// Check if client binary exists
	if _, err := os.Stat(s.clientBinaryPath); os.IsNotExist(err) {
		log.Printf("Client binary not found at %s", s.clientBinaryPath)
		http.Error(w, "Client binary not available, cannot generate installation script", http.StatusServiceUnavailable)
		return
	}

	// Get protocol and host information
	proto := "http"
	if r.TLS != nil {
		proto = "https"
	}
	// Handle reverse proxy cases
	if forwardedProto := r.Header.Get("X-Forwarded-Proto"); forwardedProto != "" {
		proto = forwardedProto
	}

	// Build WebSocket protocol
	wsProto := "ws"
	if proto == "https" {
		wsProto = "wss"
	}

	// Get full host address
	host := r.Host
	serverURL := fmt.Sprintf("%s://%s", proto, host)
	wsURL := fmt.Sprintf("%s://%s/ws", wsProto, host)

	// Generate Bash script template
	script := fmt.Sprintf(`#!/usr/bin/env bash
set -xeuo pipefail

wsUrl="%s"
serverUrl="%s"
serverVersion="%s"
synapseClientPath="$HOME/.local/bin/synapse-client"

function installClient() {
    # Automatically install client script
    echo "Downloading client..."
    mkdir -p "$(dirname "$synapseClientPath")"
    if command -v curl >/dev/null 2>&1; then
        curl -L -o "$synapseClientPath" "$serverUrl/getclient"
    elif command -v wget >/dev/null 2>&1; then
        wget -O "$synapseClientPath" "$serverUrl/getclient"
    else
        echo "Error: curl or wget is required to download the client"
        exit 1
    fi
    echo "Downloaded client to $synapseClientPath"
}

if [ -f "$synapseClientPath" ]; then
    clientVersion=$("$synapseClientPath" --version | awk -F': ' '{print $2}' | tr -d '\n')
    if [ "$clientVersion" != "$serverVersion" ]; then
        echo "Client version mismatch, reinstalling"
        installClient
    fi
else
    installClient
fi

chmod +x "$synapseClientPath"
echo "Starting client to connect to server: $wsUrl"
exec "$synapseClientPath" --server-url "$wsUrl" "$@"
`, wsURL, serverURL, s.version)

	// Set response headers
	w.Header().Set("Content-Type", "text/x-shellscript")
	w.Header().Set("Content-Disposition", "attachment; filename=install-client.sh")

	// Send script content
	if _, err := io.WriteString(w, script); err != nil {
		log.Printf("Failed to send one-key installation script: %v", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
	}
}

func hostHasExplicitPort(host string) bool {
	if host == "" {
		return false
	}
	if _, _, err := net.SplitHostPort(host); err == nil {
		return true
	}
	return false
}

func stripIPv6Brackets(host string) string {
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = strings.TrimPrefix(host, "[")
		host = strings.TrimSuffix(host, "]")
	}
	return host
}

func (s *Server) Start(host string, port string) error {
	http.HandleFunc("/ws", s.handleWebSocket)
	http.HandleFunc("/v1/", s.handleAPIRequest)
	http.HandleFunc("/getclient", s.handleGetClient)
	http.HandleFunc("/", s.handleIndex)
	http.HandleFunc("/version", s.handleGetServerVersion)
	http.HandleFunc("/run", s.oneKeyScript)
	return http.ListenAndServe(host+":"+port, nil)
}
