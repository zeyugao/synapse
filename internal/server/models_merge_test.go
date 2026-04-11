package server

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/zeyugao/synapse/internal/types"
)

func TestMergedModelPayloadKeepsOnlySharedFields(t *testing.T) {
	t.Parallel()

	models := []*types.ModelInfo{
		rawModelInfo(t, "model-a", `{
			"id": "model-a",
			"object": "model",
			"owned_by": "vllm",
			"max_model_len": 262144,
			"aliases": ["a", "b"],
			"permission": [
				{
					"id": "perm-1",
					"allow_sampling": true,
					"allow_logprobs": true
				},
				{
					"id": "perm-2",
					"allow_sampling": true
				}
			],
			"meta": {
				"format": "bf16",
				"quant": "none"
			},
			"settings": {
				"a": 1
			}
		}`),
		rawModelInfo(t, "model-a", `{
			"id": "model-a",
			"object": "model",
			"owned_by": "vllm",
			"max_model_len": 131072,
			"aliases": ["c", "d"],
			"permission": [
				{
					"id": "perm-1",
					"allow_sampling": true,
					"allow_logprobs": false
				},
				{
					"id": "perm-3",
					"allow_sampling": true
				}
			],
			"meta": {
				"format": "bf16",
				"quant": "awq"
			},
			"settings": {
				"b": 2
			}
		}`),
	}

	got := mergedModelPayload(models, "model-a")
	expected := map[string]any{
		"id":       "model-a",
		"object":   "model",
		"owned_by": "vllm",
		"permission": []any{
			map[string]any{
				"id":             "perm-1",
				"allow_sampling": true,
			},
		},
		"meta": map[string]any{
			"format": "bf16",
		},
	}

	if !reflect.DeepEqual(expected, got) {
		t.Fatalf("merged payload mismatch:\nexpected: %#v\ngot: %#v", expected, got)
	}
}

func TestBuildModelsCacheLockedMergesModelsAndAddsClientCount(t *testing.T) {
	t.Parallel()

	srv := &Server{
		clients: map[string]*Client{
			"client-b": {
				groupName: defaultGroupName,
				models: []*types.ModelInfo{
					rawModelInfo(t, "model-b", `{
						"id": "model-b",
						"object": "model",
						"owned_by": "vllm",
						"max_model_len": 1024
					}`),
				},
			},
			"client-a": {
				groupName: defaultGroupName,
				models: []*types.ModelInfo{
					rawModelInfo(t, "model-a", `{
						"id": "model-a",
						"object": "model",
						"owned_by": "vllm"
					}`),
					rawModelInfo(t, "model-b", `{
						"id": "model-b",
						"object": "model",
						"owned_by": "vllm",
						"max_model_len": 2048
					}`),
				},
			},
		},
	}

	cache := srv.buildModelsCacheLocked(defaultGroupName)

	var response struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
	}
	if err := decodeJSONWithNumbers(cache, &response); err != nil {
		t.Fatalf("failed to decode cache: %v", err)
	}

	if response.Object != "list" {
		t.Fatalf("expected object=list, got %q", response.Object)
	}
	if len(response.Data) != 2 {
		t.Fatalf("expected 2 models, got %d", len(response.Data))
	}

	if id, _ := response.Data[0]["id"].(string); id != "model-a" {
		t.Fatalf("expected first model to be model-a, got %q", id)
	}
	if id, _ := response.Data[1]["id"].(string); id != "model-b" {
		t.Fatalf("expected second model to be model-b, got %q", id)
	}

	expectedA := map[string]any{
		"id":           "model-a",
		"object":       "model",
		"owned_by":     "vllm",
		"client_count": json.Number("1"),
	}
	if !reflect.DeepEqual(expectedA, response.Data[0]) {
		t.Fatalf("model-a mismatch:\nexpected: %#v\ngot: %#v", expectedA, response.Data[0])
	}

	expectedB := map[string]any{
		"id":           "model-b",
		"object":       "model",
		"owned_by":     "vllm",
		"client_count": json.Number("2"),
	}
	if !reflect.DeepEqual(expectedB, response.Data[1]) {
		t.Fatalf("model-b mismatch:\nexpected: %#v\ngot: %#v", expectedB, response.Data[1])
	}
}

func rawModelInfo(t *testing.T, id, raw string) *types.ModelInfo {
	t.Helper()

	trimmed := bytes.TrimSpace([]byte(raw))
	return &types.ModelInfo{
		Id:      id,
		RawJson: trimmed,
	}
}

func decodeJSONWithNumbers(data []byte, dest any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return decoder.Decode(dest)
}
