package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/zeyugao/synapse/internal/types"
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

func TestModelsEqualOnlyChecksIDs(t *testing.T) {
	t.Parallel()

	c := &Client{
		models: []*types.ModelInfo{
			{Id: "model-a", RawJson: []byte(`{"id":"model-a","max_model_len":1}`)},
			{Id: "model-b", RawJson: []byte(`{"id":"model-b","max_model_len":2}`)},
		},
	}

	if !c.modelsEqual([]*types.ModelInfo{
		{Id: "model-b", RawJson: []byte(`{"id":"model-b","max_model_len":999}`)},
		{Id: "model-a", RawJson: []byte(`{"id":"model-a","max_model_len":123}`)},
	}) {
		t.Fatal("expected same ID set to be equal despite metadata differences")
	}

	if c.modelsEqual([]*types.ModelInfo{
		{Id: "model-a", RawJson: []byte(`{"id":"model-a"}`)},
	}) {
		t.Fatal("expected different ID set to be unequal")
	}
}
