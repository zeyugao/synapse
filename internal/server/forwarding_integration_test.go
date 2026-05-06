package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	synapseclient "github.com/zeyugao/synapse/internal/client"
)

func TestForwardingIntegrationMatchesOpenAIClient(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	backend := newFakeOpenAIBackend(t)
	defer backend.Close()

	serverAPIKey := "synapse-server-api-key"
	srv := NewServer(serverAPIKey, "", "test-version", "")

	httpBaseURL, wsURL, shutdownServer := startSynapseHTTPServer(t, srv)
	defer shutdownServer()

	client := synapseclient.NewClient(backend.BaseURL()+"/v1", wsURL, "test-version")
	client.ApiKey = backend.apiKey
	if err := client.Connect(); err != nil {
		t.Fatalf("failed to start synapse client: %v", err)
	}
	defer func() {
		client.Shutdown()
		client.Close()
	}()

	if !waitForCondition(5*time.Second, func() bool {
		srv.mu.RLock()
		defer srv.mu.RUnlock()
		clients := srv.modelClients[defaultGroupName][backend.modelID]
		return len(clients) > 0
	}) {
		t.Fatalf("model %q was never registered with the server", backend.modelID)
	}

	scriptPath := writePythonClientScript(t)

	nonStreamPrompt := "ping-non-stream"
	streamPrompt := "ping-stream"
	expectedNonStream := backend.ExpectedForPrompt(nonStreamPrompt)
	expectedStream := backend.ExpectedForPrompt(streamPrompt)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(
		ctx,
		"python3",
		scriptPath,
		"--base-url", httpBaseURL+"/v1",
		"--api-key", serverAPIKey,
		"--model", backend.modelID,
		"--non-stream-prompt", nonStreamPrompt,
		"--stream-prompt", streamPrompt,
	)
	cmd.Env = append(os.Environ(),
		"PYTHONUNBUFFERED=1",
		"NO_PROXY=127.0.0.1,localhost",
		"no_proxy=127.0.0.1,localhost",
	)
	output, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("python client timed out: %v", ctx.Err())
	}
	if err != nil {
		t.Fatalf("python client failed: %v\nOutput:\n%s", err, string(output))
	}

	var results []pythonResult
	if err := json.Unmarshal(bytes.TrimSpace(output), &results); err != nil {
		t.Fatalf("failed to parse python output %q: %v", string(output), err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 results from python client, got %d (%v)", len(results), results)
	}

	var gotNonStream, gotStream string
	for _, res := range results {
		switch res.Type {
		case "non_stream":
			gotNonStream = res.Content
		case "stream":
			gotStream = res.Content
		default:
			t.Fatalf("unexpected result type %q from python client", res.Type)
		}
	}

	if gotNonStream != expectedNonStream {
		t.Fatalf("non-stream mismatch: got %q want %q", gotNonStream, expectedNonStream)
	}
	if gotStream != expectedStream {
		t.Fatalf("stream mismatch: got %q want %q", gotStream, expectedStream)
	}
}

func TestForwardingIntegrationPreservesRawSSEBytes(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	backend := newFakeOpenAIBackend(t)
	defer backend.Close()

	httpBaseURL, serverAPIKey, shutdown := startConnectedSynapseClient(t, backend)
	defer shutdown()

	resp, body := readStreamingBody(t, httpBaseURL, serverAPIKey, backend.modelID, rawSSEPrompt)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status code: got %d want %d", resp.StatusCode, http.StatusOK)
	}
	if contentType := resp.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("unexpected content type: got %q", contentType)
	}

	expected := backend.specialStreamBody(rawSSEPrompt)
	if !bytes.Equal(body, expected) {
		t.Fatalf("raw stream mismatch:\n got: %q\nwant: %q", string(body), string(expected))
	}
}

func TestForwardingIntegrationSupportsLargeStreamingChunks(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	backend := newFakeOpenAIBackend(t)
	defer backend.Close()

	httpBaseURL, serverAPIKey, shutdown := startConnectedSynapseClient(t, backend)
	defer shutdown()

	resp, body := readStreamingBody(t, httpBaseURL, serverAPIKey, backend.modelID, largeStreamPrompt)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status code: got %d want %d", resp.StatusCode, http.StatusOK)
	}

	expected := backend.specialStreamBody(largeStreamPrompt)
	if !bytes.Equal(body, expected) {
		t.Fatalf("large stream mismatch: got %d bytes want %d bytes", len(body), len(expected))
	}
}

func TestForwardingIntegrationHandlesEmptyStream(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	backend := newFakeOpenAIBackend(t)
	defer backend.Close()

	httpBaseURL, serverAPIKey, shutdown := startConnectedSynapseClient(t, backend)
	defer shutdown()

	resp, body := readStreamingBody(t, httpBaseURL, serverAPIKey, backend.modelID, emptyStreamPrompt)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status code: got %d want %d", resp.StatusCode, http.StatusOK)
	}
	if len(body) != 0 {
		t.Fatalf("expected empty stream body, got %q", string(body))
	}
}

func TestForwardingIntegrationClosesStreamWhenEOFArrivesAfterFinalChunk(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	backend := newFakeOpenAIBackend(t)
	defer backend.Close()

	httpBaseURL, serverAPIKey, shutdown := startConnectedSynapseClient(t, backend)
	defer shutdown()

	payload := map[string]any{
		"model":    backend.modelID,
		"messages": []map[string]string{{"role": "user", "content": delayedEOFPrompt}},
		"stream":   true,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("failed to marshal request: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, httpBaseURL+"/v1/chat/completions", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+serverAPIKey)
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("stream request failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read delayed-EOF streaming body: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status code: got %d want %d", resp.StatusCode, http.StatusOK)
	}

	expected := backend.specialStreamBody(delayedEOFPrompt)
	if !bytes.Equal(body, expected) {
		t.Fatalf("delayed EOF stream mismatch:\n got: %q\nwant: %q", string(body), string(expected))
	}
}

func TestForwardingIntegrationIsolatesGroupsAndSupportsXAPIKey(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	const sharedModelID = "shared-model"

	alphaBackend := newFakeOpenAIBackendWithOptions(t, sharedModelID, "upstream-alpha", "alpha", "group-alpha")
	defer alphaBackend.Close()

	betaBackend := newFakeOpenAIBackendWithOptions(t, sharedModelID, "upstream-beta", "beta", "group-beta")
	defer betaBackend.Close()

	srv, err := NewServerWithConfig(&Config{
		Groups: []ConfigGroup{
			{
				Name:       "alpha",
				WSAuthKeys: []string{"ws-alpha"},
				APIKeys:    []string{"api-alpha"},
			},
			{
				Name:       "beta",
				WSAuthKeys: []string{"ws-beta"},
				APIKeys:    []string{"api-beta"},
			},
		},
	}, "test-version", "")
	if err != nil {
		t.Fatalf("failed to initialize grouped server: %v", err)
	}

	httpBaseURL, wsURL, shutdownServer := startSynapseHTTPServer(t, srv)
	defer shutdownServer()

	alphaClient := connectSynapseClient(t, alphaBackend, wsURL, "ws-alpha")
	defer func() {
		alphaClient.Shutdown()
		alphaClient.Close()
	}()

	betaClient := connectSynapseClient(t, betaBackend, wsURL, "ws-beta")
	defer func() {
		betaClient.Shutdown()
		betaClient.Close()
	}()

	waitForRegisteredModel(t, srv, "alpha", sharedModelID)
	waitForRegisteredModel(t, srv, "beta", sharedModelID)

	alphaModels := fetchModelsResponse(t, httpBaseURL, map[string]string{
		"Authorization": "Bearer api-alpha",
	})
	if alphaModels.Data[0].OwnedBy != "group-alpha" {
		t.Fatalf("expected alpha models view to stay in group alpha, got %#v", alphaModels.Data)
	}

	betaModels := fetchModelsResponse(t, httpBaseURL, map[string]string{
		"X-API-Key": "api-beta",
	})
	if betaModels.Data[0].OwnedBy != "group-beta" {
		t.Fatalf("expected beta models view to stay in group beta, got %#v", betaModels.Data)
	}

	alphaContent := fetchCompletionContent(t, httpBaseURL, sharedModelID, "prompt-alpha", map[string]string{
		"Authorization": "Bearer api-alpha",
	})
	if alphaContent != alphaBackend.ExpectedForPrompt("prompt-alpha") {
		t.Fatalf("alpha group routed to wrong backend: got %q want %q", alphaContent, alphaBackend.ExpectedForPrompt("prompt-alpha"))
	}

	betaContent := fetchCompletionContent(t, httpBaseURL, sharedModelID, "prompt-beta", map[string]string{
		"X-API-Key": "api-beta",
	})
	if betaContent != betaBackend.ExpectedForPrompt("prompt-beta") {
		t.Fatalf("beta group routed to wrong backend: got %q want %q", betaContent, betaBackend.ExpectedForPrompt("prompt-beta"))
	}

	resp, body := doJSONRequest(t, http.MethodGet, httpBaseURL+"/v1/models", nil, map[string]string{
		"Authorization": "Bearer api-alpha",
		"X-API-Key":     "api-beta",
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected conflicting credentials to be rejected, got %d body=%q", resp.StatusCode, string(body))
	}
}

func startSynapseHTTPServer(t *testing.T, srv *Server) (httpBaseURL, wsURL string, shutdown func()) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", srv.handleWebSocket)
	mux.HandleFunc("/v1/", srv.handleAPIRequest)

	server := &http.Server{
		Handler: mux,
	}

	go func() {
		if err := server.Serve(ln); err != nil && err != http.ErrServerClosed {
			t.Logf("synapse server exited: %v", err)
		}
	}()

	addr := ln.Addr().String()
	return "http://" + addr, "ws://" + addr + "/ws", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(ctx)
	}
}

type pythonResult struct {
	Type    string `json:"type"`
	Content string `json:"content"`
}

func writePythonClientScript(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "openai_client.py")
	if err := os.WriteFile(path, []byte(pythonClientScript), 0o600); err != nil {
		t.Fatalf("failed to write python client: %v", err)
	}
	return path
}

const pythonClientScript = `
import argparse
import concurrent.futures
import json
import sys
import urllib.error
import urllib.request


def build_url(base, path):
    if base.endswith("/"):
        base = base[:-1]
    if not path.startswith("/"):
        path = "/" + path
    return base + path


def create_request(url, payload, api_key, accept=None):
    headers = {
        "Content-Type": "application/json",
        "Authorization": f"Bearer {api_key}",
    }
    if accept:
        headers["Accept"] = accept
    data = json.dumps(payload).encode("utf-8")
    return urllib.request.Request(url, data=data, headers=headers, method="POST")


def read_streaming_response(response):
    parts = []
    for raw_line in response:
        line = raw_line.decode("utf-8").rstrip("\r\n")
        if not line or not line.startswith("data:"):
            continue
        payload = line[5:].lstrip()
        if not payload or payload == "[DONE]":
            if payload == "[DONE]":
                break
            continue
        try:
            data = json.loads(payload)
        except json.JSONDecodeError:
            continue
        for choice in data.get("choices", []):
            delta = choice.get("delta") or {}
            content = delta.get("content")
            if isinstance(content, str):
                parts.append(content)
            elif isinstance(content, list):
                for item in content:
                    text = item.get("text")
                    if text:
                        parts.append(text)
    return "".join(parts)


def run_non_stream(base_url, api_key, model, prompt):
    url = build_url(base_url, "/chat/completions")
    payload = {
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
    }
    request = create_request(url, payload, api_key)
    with urllib.request.urlopen(request) as response:
        body = response.read().decode("utf-8")
    data = json.loads(body)
    choices = data.get("choices") or []
    if not choices:
        return ""
    message = choices[0].get("message") or {}
    return message.get("content", "")


def run_stream(base_url, api_key, model, prompt):
    url = build_url(base_url, "/chat/completions")
    payload = {
        "model": model,
        "messages": [{"role": "user", "content": prompt}],
        "stream": True,
    }
    request = create_request(url, payload, api_key, accept="text/event-stream")
    with urllib.request.urlopen(request) as response:
        return read_streaming_response(response)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--base-url", required=True)
    parser.add_argument("--api-key", required=True)
    parser.add_argument("--model", required=True)
    parser.add_argument("--non-stream-prompt", required=True)
    parser.add_argument("--stream-prompt", required=True)
    args = parser.parse_args()

    with concurrent.futures.ThreadPoolExecutor(max_workers=2) as executor:
        futures = [
            executor.submit(run_non_stream, args.base_url, args.api_key, args.model, args.non_stream_prompt),
            executor.submit(run_stream, args.base_url, args.api_key, args.model, args.stream_prompt),
        ]
        results = [
            {"type": "non_stream", "content": futures[0].result()},
            {"type": "stream", "content": futures[1].result()},
        ]

    print(json.dumps(results), flush=True)


if __name__ == "__main__":
    try:
        main()
    except urllib.error.HTTPError as exc:
        payload = exc.read().decode("utf-8", "ignore")
        sys.stderr.write(f"HTTP error {exc.code}: {payload}\n")
        raise
`

type fakeOpenAIBackend struct {
	t              *testing.T
	server         *httptest.Server
	apiKey         string
	modelID        string
	responsePrefix string
	ownedBy        string
}

func newFakeOpenAIBackend(t *testing.T) *fakeOpenAIBackend {
	return newFakeOpenAIBackendWithOptions(t, "test-model", "upstream-test-api-key", "echo", "integration-test")
}

func newFakeOpenAIBackendWithOptions(t *testing.T, modelID, apiKey, responsePrefix, ownedBy string) *fakeOpenAIBackend {
	t.Helper()

	backend := &fakeOpenAIBackend{
		t:              t,
		apiKey:         apiKey,
		modelID:        modelID,
		responsePrefix: responsePrefix,
		ownedBy:        ownedBy,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", backend.handleModels)
	mux.HandleFunc("/v1/chat/completions", backend.handleChatCompletion)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen for fake backend: %v", err)
	}

	srv := httptest.NewUnstartedServer(mux)
	srv.Listener = ln
	srv.Start()

	backend.server = srv
	return backend
}

func (f *fakeOpenAIBackend) Close() {
	f.server.Close()
}

func (f *fakeOpenAIBackend) BaseURL() string {
	return f.server.URL
}

func (f *fakeOpenAIBackend) ExpectedForPrompt(prompt string) string {
	return fmt.Sprintf("%s:%s", f.responsePrefix, prompt)
}

func (f *fakeOpenAIBackend) handleModels(w http.ResponseWriter, r *http.Request) {
	if !f.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	resp := map[string]any{
		"object": "list",
		"data": []map[string]any{
			{
				"id":       f.modelID,
				"object":   "model",
				"owned_by": f.ownedBy,
				"created":  time.Now().Unix(),
			},
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

type completionMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatCompletionRequest struct {
	Model    string              `json:"model"`
	Messages []completionMessage `json:"messages"`
	Stream   bool                `json:"stream"`
}

func (f *fakeOpenAIBackend) handleChatCompletion(w http.ResponseWriter, r *http.Request) {
	if !f.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}
	defer r.Body.Close()

	var req chatCompletionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}

	if req.Model != f.modelID {
		http.Error(w, "unknown model", http.StatusNotFound)
		return
	}

	prompt := lastUserMessage(req.Messages)
	if prompt == "" {
		http.Error(w, "missing user prompt", http.StatusBadRequest)
		return
	}

	responseText := f.ExpectedForPrompt(prompt)

	if req.Stream {
		if prompt == delayedEOFPrompt {
			f.streamRawResponseWithDelayedClose(w, f.specialStreamBody(prompt), 150*time.Millisecond)
			return
		}
		if rawBody, ok := f.specialStreamBodyForPrompt(prompt); ok {
			f.streamRawResponse(w, rawBody)
			return
		}
		f.streamResponse(w, req.Model, responseText)
		return
	}

	resp := map[string]any{
		"id":      "chatcmpl-test",
		"object":  "chat.completion",
		"model":   req.Model,
		"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": responseText}, "finish_reason": "stop"}},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (f *fakeOpenAIBackend) authorized(r *http.Request) bool {
	return r.Header.Get("Authorization") == "Bearer "+f.apiKey
}

func (f *fakeOpenAIBackend) streamResponse(w http.ResponseWriter, model, content string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, ok := w.(http.Flusher)
	if !ok {
		f.t.Fatal("response writer does not support flushing")
	}

	chunks := chunkContent(content, 4)
	for idx, chunk := range chunks {
		choices := []map[string]any{
			{
				"index": 0,
				"delta": map[string]any{
					"content": chunk,
				},
				"finish_reason": nil,
			},
		}
		payload := map[string]any{
			"id":      fmt.Sprintf("chunk-%d", idx),
			"object":  "chat.completion.chunk",
			"model":   model,
			"choices": choices,
		}
		if idx == 0 {
			choices[0]["delta"] = map[string]any{
				"role":    "assistant",
				"content": chunk,
			}
		}
		data, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}

	finalChoices := []map[string]any{
		{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "stop",
		},
	}
	finalPayload := map[string]any{
		"id":      fmt.Sprintf("chunk-%d", len(chunks)),
		"object":  "chat.completion.chunk",
		"model":   model,
		"choices": finalChoices,
	}
	data, _ := json.Marshal(finalPayload)
	fmt.Fprintf(w, "data: %s\n\n", data)
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (f *fakeOpenAIBackend) streamRawResponse(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	flusher, ok := w.(http.Flusher)
	if !ok {
		f.t.Fatal("response writer does not support flushing")
	}

	if len(body) > 0 {
		if _, err := w.Write(body); err != nil {
			f.t.Fatalf("failed to write raw stream body: %v", err)
		}
	}
	flusher.Flush()
}

func (f *fakeOpenAIBackend) streamRawResponseWithDelayedClose(w http.ResponseWriter, body []byte, delay time.Duration) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, ok := w.(http.Flusher)
	if !ok {
		f.t.Fatal("response writer does not support flushing")
	}

	if len(body) > 0 {
		if _, err := w.Write(body); err != nil {
			f.t.Fatalf("failed to write delayed raw stream body: %v", err)
		}
	}
	flusher.Flush()
	time.Sleep(delay)
}

func (f *fakeOpenAIBackend) specialStreamBody(prompt string) []byte {
	f.t.Helper()

	body, ok := f.specialStreamBodyForPrompt(prompt)
	if !ok {
		f.t.Fatalf("no special stream configured for prompt %q", prompt)
	}
	return append([]byte(nil), body...)
}

func (f *fakeOpenAIBackend) specialStreamBodyForPrompt(prompt string) ([]byte, bool) {
	switch prompt {
	case rawSSEPrompt:
		return []byte(": keep-alive\nid: evt-1\nevent: update\ndata: first line\ndata: second line\n\nid: evt-2\ndata: [DONE]\n\n"), true
	case largeStreamPrompt:
		return []byte("data: " + strings.Repeat("x", 70*1024) + "\n\n"), true
	case emptyStreamPrompt:
		return nil, true
	case delayedEOFPrompt:
		return []byte("data: {\"id\":\"delayed-eof\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"), true
	default:
		return nil, false
	}
}

func chunkContent(text string, size int) []string {
	if size <= 0 {
		size = 4
	}
	var chunks []string
	remaining := text
	for len(remaining) > 0 {
		if len(remaining) <= size {
			chunks = append(chunks, remaining)
			break
		}
		chunks = append(chunks, remaining[:size])
		remaining = remaining[size:]
	}
	if len(chunks) == 0 {
		chunks = append(chunks, "")
	}
	return chunks
}

func lastUserMessage(messages []completionMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.EqualFold(messages[i].Role, "user") {
			return messages[i].Content
		}
	}
	return ""
}

func waitForCondition(timeout time.Duration, fn func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

type modelsResponse struct {
	Data []struct {
		ID      string `json:"id"`
		OwnedBy string `json:"owned_by"`
	} `json:"data"`
}

func connectSynapseClient(t *testing.T, backend *fakeOpenAIBackend, wsURL, wsAuthKey string) *synapseclient.Client {
	t.Helper()

	client := synapseclient.NewClient(backend.BaseURL()+"/v1", wsURL, "test-version")
	client.ApiKey = backend.apiKey
	client.WSAuthKey = wsAuthKey
	if err := client.Connect(); err != nil {
		t.Fatalf("failed to start synapse client for group %q: %v", wsAuthKey, err)
	}
	return client
}

func waitForRegisteredModel(t *testing.T, srv *Server, groupName, modelID string) {
	t.Helper()

	if waitForCondition(5*time.Second, func() bool {
		srv.mu.RLock()
		defer srv.mu.RUnlock()
		return len(srv.modelClients[groupName][modelID]) > 0
	}) {
		return
	}

	t.Fatalf("model %q was never registered for group %q", modelID, groupName)
}

func startConnectedSynapseClient(t *testing.T, backend *fakeOpenAIBackend) (httpBaseURL, serverAPIKey string, shutdown func()) {
	t.Helper()

	serverAPIKey = "synapse-server-api-key"
	srv := NewServer(serverAPIKey, "", "test-version", "")

	httpBaseURL, wsURL, shutdownServer := startSynapseHTTPServer(t, srv)

	client := connectSynapseClient(t, backend, wsURL, "")
	waitForRegisteredModel(t, srv, defaultGroupName, backend.modelID)

	return httpBaseURL, serverAPIKey, func() {
		client.Shutdown()
		client.Close()
		shutdownServer()
	}
}

func fetchModelsResponse(t *testing.T, httpBaseURL string, headers map[string]string) modelsResponse {
	t.Helper()

	resp, body := doJSONRequest(t, http.MethodGet, httpBaseURL+"/v1/models", nil, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("models request failed: status=%d body=%q", resp.StatusCode, string(body))
	}

	var parsed modelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("failed to decode models response %q: %v", string(body), err)
	}
	if len(parsed.Data) == 0 {
		t.Fatalf("expected at least one model, got %q", string(body))
	}
	return parsed
}

func fetchCompletionContent(t *testing.T, httpBaseURL, model, prompt string, headers map[string]string) string {
	t.Helper()

	payload := map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("failed to marshal completion request: %v", err)
	}

	resp, body := doJSONRequest(t, http.MethodPost, httpBaseURL+"/v1/chat/completions", data, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("completion request failed: status=%d body=%q", resp.StatusCode, string(body))
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("failed to decode completion response %q: %v", string(body), err)
	}
	if len(parsed.Choices) == 0 {
		t.Fatalf("expected at least one choice, got %q", string(body))
	}
	return parsed.Choices[0].Message.Content
}

func doJSONRequest(t *testing.T, method, url string, body []byte, headers map[string]string) (*http.Response, []byte) {
	t.Helper()

	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	if len(body) > 0 && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}

	resp.Body = io.NopCloser(bytes.NewReader(respBody))
	return resp, respBody
}

func readStreamingBody(t *testing.T, httpBaseURL, apiKey, model, prompt string) (*http.Response, []byte) {
	t.Helper()

	payload := map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
		"stream":   true,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("failed to marshal request: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, httpBaseURL+"/v1/chat/completions", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("stream request failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read streaming body: %v", err)
	}

	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, body
}

const (
	rawSSEPrompt      = "raw-sse"
	largeStreamPrompt = "large-stream"
	emptyStreamPrompt = "empty-stream"
	delayedEOFPrompt  = "delayed-eof"
)
