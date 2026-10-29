package client

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/zeyugao/synapse/internal/types"
	pb "github.com/zeyugao/synapse/internal/types/proto"
	"google.golang.org/protobuf/proto"
)

func TestFetchModelsPreservesRawJSON(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"object": "list",
			"data": [
				{
					"id": "model-a",
					"object": "model",
					"created": 1774275193,
					"owned_by": "vllm",
					"max_model_len": 262144,
					"permission": [
						{
							"id": "perm-1",
							"allow_sampling": true
						}
					],
					"meta": {
						"family": "nemotron"
					}
				}
			]
		}`))
	}))
	defer server.Close()

	c := &Client{BaseUrl: server.URL}
	models, err := c.fetchModels(false)
	if err != nil {
		t.Fatalf("fetchModels returned error: %v", err)
	}

	if len(models) != 1 {
		t.Fatalf("expected 1 model, got %d", len(models))
	}

	model := models[0]
	if model.GetId() != "model-a" {
		t.Fatalf("expected id model-a, got %q", model.GetId())
	}
	if model.GetObject() != "model" {
		t.Fatalf("expected object model, got %q", model.GetObject())
	}
	if model.GetCreated() != 1774275193 {
		t.Fatalf("expected created 1774275193, got %d", model.GetCreated())
	}
	if model.GetOwnedBy() != "vllm" {
		t.Fatalf("expected owned_by vllm, got %q", model.GetOwnedBy())
	}
	if len(model.GetRawJson()) == 0 {
		t.Fatal("expected raw_json to be populated")
	}

	payload, err := decodeJSONObject(model.GetRawJson())
	if err != nil {
		t.Fatalf("failed to decode raw_json: %v", err)
	}

	expected := map[string]any{
		"id":            "model-a",
		"object":        "model",
		"created":       json.Number("1774275193"),
		"owned_by":      "vllm",
		"max_model_len": json.Number("262144"),
		"permission": []any{
			map[string]any{
				"id":             "perm-1",
				"allow_sampling": true,
			},
		},
		"meta": map[string]any{
			"family": "nemotron",
		},
	}
	if !reflect.DeepEqual(expected, payload) {
		t.Fatalf("raw_json mismatch:\nexpected: %#v\ngot: %#v", expected, payload)
	}
}

func TestModelsEqualChecksMetadata(t *testing.T) {
	t.Parallel()

	c := &Client{
		models: []*types.ModelInfo{
			{Id: "model-a", RawJson: []byte(`{"id":"model-a","max_model_len":1}`)},
			{Id: "model-b", RawJson: []byte(`{"id":"model-b","max_model_len":2}`)},
		},
	}

	if !c.modelsEqual([]*types.ModelInfo{
		{Id: "model-b", RawJson: []byte(`{"id":"model-b","max_model_len":2}`)},
		{Id: "model-a", RawJson: []byte(`{"id":"model-a","max_model_len":1}`)},
	}) {
		t.Fatal("unchanged metadata should compare equal regardless of model order")
	}

	if c.modelsEqual([]*types.ModelInfo{
		{Id: "model-b", RawJson: []byte(`{"id":"model-b","max_model_len":999}`)},
		{Id: "model-a", RawJson: []byte(`{"id":"model-a","max_model_len":123}`)},
	}) {
		t.Fatal("expected changed metadata to be unequal")
	}

	if c.modelsEqual([]*types.ModelInfo{
		{Id: "model-a", RawJson: []byte(`{"id":"model-a"}`)},
	}) {
		t.Fatal("expected different ID set to be unequal")
	}
}

func TestSyncModelsClearsModelsAfterConsecutiveUpstreamFailures(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "loading", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	messages := make(chan *types.ClientMessage, 4)
	wsURL, closeWS := startClientMessageCaptureServer(t, messages)
	defer closeWS()

	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("failed to dial websocket test server: %v", err)
	}
	defer conn.Close()

	c := &Client{
		BaseUrl: upstream.URL,
		models: []*types.ModelInfo{
			{Id: "model-a"},
		},
		conn: conn,
	}

	c.syncModels()
	c.syncModels()

	if len(c.models) != 1 || c.models[0].GetId() != "model-a" {
		t.Fatalf("expected existing model list to be kept before threshold, got %#v", c.models)
	}

	select {
	case msg := <-messages:
		t.Fatalf("expected no model update before threshold, got %T", msg.Message)
	default:
	}

	c.syncModels()

	select {
	case msg := <-messages:
		payload, ok := msg.Message.(*pb.ClientMessage_ModelUpdate)
		if !ok || payload.ModelUpdate == nil {
			t.Fatalf("expected model update message, got %T", msg.Message)
		}
		if len(payload.ModelUpdate.GetModels()) != 0 {
			t.Fatalf("expected empty model update, got %#v", payload.ModelUpdate.GetModels())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for empty model update")
	}

	if len(c.models) != 0 {
		t.Fatalf("expected local model list to be cleared after threshold, got %#v", c.models)
	}
}

func TestConnectRegistersEmptyModelsWhenUpstreamModelsFail(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "loading", http.StatusServiceUnavailable)
	}))
	defer upstream.Close()

	registrationCh := make(chan *types.ClientRegistration, 1)
	wsURL, closeWS := startRegistrationCaptureServer(t, registrationCh)
	defer closeWS()

	c := NewClient(upstream.URL, wsURL, "test-version")
	defer c.Close()
	c.models = []*types.ModelInfo{{Id: "cached-model"}}

	if err := c.Connect(); err != nil {
		t.Fatalf("Connect returned error: %v", err)
	}

	select {
	case registration := <-registrationCh:
		if len(registration.GetModels()) != 0 {
			t.Fatalf("expected empty registration models, got %#v", registration.GetModels())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for registration")
	}
}

func startClientMessageCaptureServer(t *testing.T, messages chan<- *types.ClientMessage) (string, func()) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	upgrader := websocket.Upgrader{}
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("failed to upgrade websocket: %v", err)
				return
			}
			defer conn.Close()

			for {
				_, data, err := conn.ReadMessage()
				if err != nil {
					return
				}

				var msg types.ClientMessage
				if err := proto.Unmarshal(data, &msg); err != nil {
					t.Errorf("failed to unmarshal client message: %v", err)
					return
				}
				messages <- proto.Clone(&msg).(*types.ClientMessage)
			}
		}),
	}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			t.Errorf("websocket test server failed: %v", err)
		}
	}()

	return "ws://" + listener.Addr().String(), func() {
		_ = server.Close()
	}
}

func startRegistrationCaptureServer(t *testing.T, registrationCh chan<- *types.ClientRegistration) (string, func()) {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	upgrader := websocket.Upgrader{}
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("failed to upgrade websocket: %v", err)
				return
			}
			defer conn.Close()

			_, data, err := conn.ReadMessage()
			if err != nil {
				t.Errorf("failed to read registration: %v", err)
				return
			}

			var msg types.ClientMessage
			if err := proto.Unmarshal(data, &msg); err != nil {
				t.Errorf("failed to unmarshal registration: %v", err)
				return
			}

			payload, ok := msg.Message.(*pb.ClientMessage_Registration)
			if !ok || payload.Registration == nil {
				t.Errorf("expected registration message, got %T", msg.Message)
				return
			}
			registrationCh <- payload.Registration
		}),
	}
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			t.Errorf("websocket test server failed: %v", err)
		}
	}()

	return "ws://" + listener.Addr().String(), func() {
		_ = server.Close()
	}
}
