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
		clients := srv.modelClients[backend.modelID]
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
	cmd.Env = append(os.Environ(), "PYTHONUNBUFFERED=1")
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
	t       *testing.T
	server  *httptest.Server
	apiKey  string
	modelID string
}

func newFakeOpenAIBackend(t *testing.T) *fakeOpenAIBackend {
	t.Helper()

	backend := &fakeOpenAIBackend{
		t:       t,
		apiKey:  "upstream-test-api-key",
		modelID: "test-model",
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
	return fmt.Sprintf("echo:%s", prompt)
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
				"owned_by": "integration-test",
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
