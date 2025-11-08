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
	"net/http"
	"os"
	"sort"
	"strconv"
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
	clients                      map[string]*Client
	modelClients                 map[string][]string // model -> []clientID
	mu                           sync.RWMutex
	upgrader                     websocket.Upgrader
	pendingRequests              map[string]chan *types.ForwardResponse
	reqMu                        sync.RWMutex
	apiAuthKey                   string
	wsAuthKey                    string
	clientRequests               map[string]map[string]struct{} // clientID -> set of requestIDs
	version                      string
	semver                       string
	semverMajor                  int
	clientBinaryPath             string
	abortOnClientVersionMismatch bool
	clientMsgPool                sync.Pool
	serverMsgPool                sync.Pool
	pongFrame                    []byte
	modelsCache                  []byte
	modelsCacheDirty             bool
}

type Client struct {
	conn           *websocket.Conn
	models         []*types.ModelInfo
	mu             sync.Mutex
	activeRequests int64
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

func NewServer(apiAuthKey, wsAuthKey string, version string, semver string, clientBinaryPath string, abortOnClientVersionMismatch bool) *Server {
	semverMajor, err := parseSemverMajor(semver)
	if err != nil {
		log.Fatalf("invalid server semantic version %q: %v", semver, err)
	}

	server := &Server{
		clients:         make(map[string]*Client),
		modelClients:    make(map[string][]string),
		pendingRequests: make(map[string]chan *types.ForwardResponse),
		clientRequests:  make(map[string]map[string]struct{}),
		apiAuthKey:      apiAuthKey,
		wsAuthKey:       wsAuthKey,
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
		version:                      version,
		semver:                       semver,
		semverMajor:                  semverMajor,
		clientBinaryPath:             clientBinaryPath,
		abortOnClientVersionMismatch: abortOnClientVersionMismatch,
		modelsCacheDirty:             true,
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

func generateRequestID() string {
	b := make([]byte, 16)
	cryptoRand.Read(b)
	return hex.EncodeToString(b)
}

func parseSemverMajor(ver string) (int, error) {
	trimmed := strings.TrimSpace(ver)
	if trimmed == "" {
		return 0, fmt.Errorf("empty semantic version")
	}
	if trimmed[0] == 'v' || trimmed[0] == 'V' {
		trimmed = trimmed[1:]
	}
	parts := strings.Split(trimmed, ".")
	if len(parts) < 2 {
		return 0, fmt.Errorf("semantic version %q must include major and minor components", ver)
	}
	majorComponent := strings.TrimSpace(parts[0])
	if majorComponent == "" {
		return 0, fmt.Errorf("semantic version %q has empty major component", ver)
	}
	major, err := strconv.Atoi(majorComponent)
	if err != nil {
		return 0, fmt.Errorf("invalid major component in semantic version %q: %w", ver, err)
	}
	if major < 0 {
		return 0, fmt.Errorf("semantic version %q has negative major component", ver)
	}
	return major, nil
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

	// Check WebSocket authentication
	if s.wsAuthKey != "" {
		authKey := r.URL.Query().Get("ws_auth_key")
		if authKey != s.wsAuthKey {
			log.Printf("WebSocket authentication failed from %s", clientIP)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
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
		if s.abortOnClientVersionMismatch {
			log.Printf("Aborting server because client version mismatch from %s", clientIP)
			conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(4001, fmt.Sprintf("Client version %s does not match server version %s", version, s.version)))
			conn.Close()
			s.putClientMessage(initialMsg)
			return
		}
	}

	clientSemver := registration.GetSemver()
	if clientSemver == "" {
		log.Printf("Client %s from %s did not provide a semantic version, connection refused", clientID, clientIP)
		conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(4002, "Semantic version required"))
		conn.Close()
		s.putClientMessage(initialMsg)
		return
	}

	clientSemverMajor, err := parseSemverMajor(clientSemver)
	if err != nil {
		log.Printf("Client %s from %s provided invalid semantic version %q: %v", clientID, clientIP, clientSemver, err)
		conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(4003, "Invalid semantic version"))
		conn.Close()
		s.putClientMessage(initialMsg)
		return
	}

	if clientSemverMajor != s.semverMajor {
		log.Printf("Client semantic version mismatch (client: %s, server: %s) from %s", clientSemver, s.semver, clientIP)
		conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(4004, fmt.Sprintf("Client major version %d does not match server major version %d", clientSemverMajor, s.semverMajor)))
		conn.Close()
		s.putClientMessage(initialMsg)
		return
	}

	models := cloneModelInfos(registration.GetModels())
	log.Printf("New client connected: %s, registered models: %d", clientID, len(models))

	s.mu.Lock()
	client := &Client{
		conn:   conn,
		models: models,
	}
	s.clients[clientID] = client

	// Update model to client mapping
	for _, model := range models {
		if model == nil {
			continue
		}
		s.modelClients[model.GetId()] = append(s.modelClients[model.GetId()], clientID)
	}
	s.modelsCacheDirty = true
	s.mu.Unlock()

	s.putClientMessage(initialMsg)

	// Add goroutine for handling responses
	go s.handleClientResponses(clientID, client)
}

func (s *Server) handleClientResponses(clientID string, client *Client) {
	defer func() {
		client.conn.Close()
		s.unregisterClient(clientID)
	}()

	for {
		msg, err := s.readClientMessage(client.conn)
		if err != nil {
			log.Printf("Failed to read client %s message: %v", clientID, err)
			return
		}

		switch payload := msg.Message.(type) {
		case *pb.ClientMessage_ForwardResponse:
			if payload.ForwardResponse != nil {
				s.handleForwardResponse(cloneForwardResponse(payload.ForwardResponse))
			}
		case *pb.ClientMessage_Heartbeat:
			if err := client.writeRaw(s.pongFrame); err != nil {
				log.Printf("Failed to send pong response: %v", err)
			}
		case *pb.ClientMessage_ModelUpdate:
			if payload.ModelUpdate != nil {
				s.handleModelUpdate(payload.ModelUpdate)
			}
		case *pb.ClientMessage_Unregister:
			if payload.Unregister != nil {
				log.Printf("Received unregister request from client %s", payload.Unregister.GetClientId())
				s.unregisterClient(payload.Unregister.GetClientId())
				s.putClientMessage(msg)
				return
			}
		case *pb.ClientMessage_ForceShutdown:
			if payload.ForceShutdown != nil {
				s.handleForceShutdown(payload.ForceShutdown)
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

func (s *Server) handleModels(w http.ResponseWriter, _ *http.Request) {
	cache := s.getModelsCache()
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(cache); err != nil {
		log.Printf("Failed to write models response: %v", err)
	}
}

func (s *Server) handleAPIRequest(w http.ResponseWriter, r *http.Request) {
	// Check API authentication
	if s.apiAuthKey != "" {
		authHeader := r.Header.Get("Authorization")
		if authHeader != "Bearer "+s.apiAuthKey {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}

	path := r.URL.Path
	if strings.HasPrefix(path, "/v1/") {
		// Remove the /v1 prefix from the path
		path = strings.TrimPrefix(path, "/v1")
	}

	if path == "/models" {
		s.handleModels(w, r)
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
	clientIDsForModel, modelSupported := s.modelClients[req.Model]

	var candidateClients []*Client
	var candidateClientIDs []string

	if modelSupported && len(clientIDsForModel) > 0 {
		for _, cid := range clientIDsForModel {
			if c, exists := s.clients[cid]; exists {
				candidateClients = append(candidateClients, c)
				candidateClientIDs = append(candidateClientIDs, cid)
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

	var bestClient *Client
	var bestClientID string
	minLoad := -1
	var tiedClients []*Client
	var tiedClientIDs []string

	for i, currentClient := range candidateClients {
		currentClientID := candidateClientIDs[i]
		load := int(currentClient.loadActive())

		if bestClient == nil || load < minLoad {
			minLoad = load
			bestClient = currentClient
			bestClientID = currentClientID
			tiedClients = []*Client{currentClient}
			tiedClientIDs = []string{currentClientID}
		} else if load == minLoad {
			tiedClients = append(tiedClients, currentClient)
			tiedClientIDs = append(tiedClientIDs, currentClientID)
		}
	}

	if len(tiedClients) > 1 {
		randomIndex := rand.Intn(len(tiedClients))
		bestClient = tiedClients[randomIndex]
		bestClientID = tiedClientIDs[randomIndex]
	}

	client := bestClient     // Use this client object to send the request
	clientID := bestClientID // Use this clientID for tracking and logging

	// Add request to client's active requests map
	s.reqMu.Lock()
	if s.clientRequests[clientID] == nil {
		s.clientRequests[clientID] = make(map[string]struct{})
	}
	s.clientRequests[clientID][requestID] = struct{}{}
	s.reqMu.Unlock()
	client.incrementActive()

	// Wait for response
	// The clientID variable from the outer scope (bestClientID) is captured by this defer.
	defer func() {
		client.decrementActive()
		s.reqMu.Lock()
		// Remove request from the client's active request set
		if reqs, ok := s.clientRequests[clientID]; ok {
			delete(reqs, requestID)
			if len(reqs) == 0 {
				delete(s.clientRequests, clientID)
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
			// Streaming response
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			flusher, _ := w.(http.Flusher)

			// Write the first chunk
			if err := writeSSEChunk(w, resp.GetBody()); err != nil {
				log.Printf("Failed to write SSE chunk: %v", err)
				return
			}
			flusher.Flush()

			// Continue processing subsequent chunks
			for {
				select {
				case chunk, ok := <-respChan:
					if !ok || chunk == nil {
						return
					}
					if chunk.GetDone() {
						if err := writeSSEChunk(w, []byte("[DONE]")); err != nil {
							log.Printf("Failed to write SSE DONE chunk: %v", err)
						}
						flusher.Flush()
						return
					} else {
						if err := writeSSEChunk(w, chunk.GetBody()); err != nil {
							log.Printf("Failed to write SSE chunk: %v", err)
							return
						}
						flusher.Flush()
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

func (s *Server) unregisterClient(clientID string) {
	s.mu.Lock()
	client, exists := s.clients[clientID]
	if !exists {
		s.mu.Unlock()
		return
	}

	// Clean up model to client mapping
	for _, model := range client.models {
		if model == nil {
			continue
		}
		modelID := model.GetId()
		if clients, ok := s.modelClients[modelID]; ok {
			newClients := make([]string, 0, len(clients))
			for _, cid := range clients {
				if cid != clientID {
					newClients = append(newClients, cid)
				}
			}

			if len(newClients) > 0 {
				s.modelClients[modelID] = newClients
			} else {
				delete(s.modelClients, modelID)
			}
		}
	}

	delete(s.clients, clientID)
	currentClientCount := len(s.clients)
	s.modelsCacheDirty = true
	s.mu.Unlock()

	// Clean up clientRequests associated with this client
	s.reqMu.Lock()
	delete(s.clientRequests, clientID)
	s.reqMu.Unlock()

	log.Printf("Client %s unregistered, remaining clients: %d", clientID, currentClientCount)
}

func (s *Server) handleModelUpdate(update *types.ModelUpdateRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()

	client := s.clients[update.GetClientId()]
	if client == nil {
		log.Printf("Received model update for unknown client %s", update.GetClientId())
		return
	}
	for _, model := range client.models {
		if model == nil {
			continue
		}
		modelID := model.GetId()
		if clients, ok := s.modelClients[modelID]; ok {
			newClients := make([]string, 0, len(clients))
			for _, cid := range clients {
				if cid != update.GetClientId() {
					newClients = append(newClients, cid)
				}
			}
			if len(newClients) > 0 {
				s.modelClients[modelID] = newClients
			} else {
				delete(s.modelClients, modelID)
			}
		}
	}

	client.models = cloneModelInfos(update.GetModels())
	for _, model := range client.models {
		if model == nil {
			continue
		}
		modelID := model.GetId()
		s.modelClients[modelID] = append(s.modelClients[modelID], update.GetClientId())
	}

	log.Printf("Updated model list for client %s, current number of models: %d", update.GetClientId(), len(client.models))
	s.modelsCacheDirty = true
}

func (s *Server) handleForceShutdown(req *types.ForceShutdownRequest) {
	s.reqMu.Lock()
	defer s.reqMu.Unlock()

	var requestsToCancel []string
	if clientActiveRequests, clientExists := s.clientRequests[req.GetClientId()]; clientExists {
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
		// Remove from clientRequests for this specific client (req.ClientId) and this requestID
		if reqsForClient, ok := s.clientRequests[req.GetClientId()]; ok {
			delete(reqsForClient, requestID)
			if len(reqsForClient) == 0 {
				delete(s.clientRequests, req.GetClientId())
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

func (s *Server) getModelsCache() []byte {
	s.mu.RLock()
	if !s.modelsCacheDirty && s.modelsCache != nil {
		cacheCopy := append([]byte(nil), s.modelsCache...)
		s.mu.RUnlock()
		return cacheCopy
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.modelsCacheDirty && s.modelsCache != nil {
		return append([]byte(nil), s.modelsCache...)
	}
	cache := s.buildModelsCacheLocked()
	s.modelsCache = cache
	s.modelsCacheDirty = false
	return append([]byte(nil), cache...)
}

func (s *Server) buildModelsCacheLocked() []byte {
	type modelEntry struct {
		*types.ModelInfo
		ClientCount int `json:"client_count"`
	}

	modelMap := make(map[string]*modelEntry)
	for _, client := range s.clients {
		for _, model := range client.models {
			if model == nil {
				continue
			}
			modelID := model.GetId()
			entry, exists := modelMap[modelID]
			if !exists {
				entry = &modelEntry{
					ModelInfo:   cloneModelInfo(model),
					ClientCount: len(s.modelClients[modelID]),
				}
				modelMap[modelID] = entry
			} else {
				entry.ClientCount = len(s.modelClients[modelID])
			}
		}
	}

	entries := make([]modelEntry, 0, len(modelMap))
	for _, entry := range modelMap {
		entries = append(entries, *entry)
	}

	sort.Slice(entries, func(i, j int) bool {
		left := ""
		right := ""
		if entries[i].ModelInfo != nil {
			left = entries[i].ModelInfo.GetId()
		}
		if entries[j].ModelInfo != nil {
			right = entries[j].ModelInfo.GetId()
		}
		return left < right
	})

	response := struct {
		Object string       `json:"object"`
		Data   []modelEntry `json:"data"`
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

func writeSSEChunk(w http.ResponseWriter, payload []byte) error {
	if _, err := w.Write([]byte("data: ")); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := w.Write(payload); err != nil {
			return err
		}
	}
	_, err := w.Write([]byte("\n\n"))
	return err
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

func (s *Server) Start(host string, port string) error {
	http.HandleFunc("/ws", s.handleWebSocket)
	http.HandleFunc("/v1/", s.handleAPIRequest)
	http.HandleFunc("/getclient", s.handleGetClient)
	http.HandleFunc("/", s.handleIndex)
	http.HandleFunc("/version", s.handleGetServerVersion)
	http.HandleFunc("/run", s.oneKeyScript)
	return http.ListenAndServe(host+":"+port, nil)
}
