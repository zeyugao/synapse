package client

import (
	"encoding/json"
	"fmt"
	"log"
	"reflect"
	"strconv"
	"strings"

	"github.com/zeyugao/synapse/internal/types"
	"google.golang.org/protobuf/proto"
)

// metadataChanged compares stable leaves against the last advertised snapshot.
// The immediately previous observation advances even when no update is sent.
// This deliberately delays a one-off metadata change until its next stable poll.
func (c *Client) metadataChanged(current []*types.ModelInfo) bool {
	previous := c.previousModels
	if previous == nil {
		previous = c.models
	}
	defer func() { c.previousModels = cloneModelInfos(current) }()
	notified := modelIndex(c.models)
	now := modelIndex(current)
	before := modelIndex(previous)
	if !sameModelIDs(now, notified) || !sameModelIDs(now, before) {
		c.suppressedMetadata = nil
		return true
	}
	if c.suppressedMetadata == nil {
		c.suppressedMetadata = make(map[string]bool)
	}
	changed := false
	for id, model := range now {
		a, errA := metadataLeaves(before[id])
		b, errB := metadataLeaves(model)
		last, errLast := metadataLeaves(notified[id])
		if errA != nil || errB != nil || errLast != nil {
			// Malformed raw JSON must never silently hide a metadata update.
			if !proto.Equal(notified[id], model) {
				changed = true
			}
			continue
		}
		paths := make(map[string]bool)
		for path := range a {
			paths[path] = true
		}
		for path := range b {
			paths[path] = true
		}
		for path := range last {
			paths[path] = true
		}
		for path := range paths {
			key := id + "\x00" + path
			if !sameLeaf(a, b, path) {
				if !c.suppressedMetadata[key] {
					log.Printf("Suppressing unstable model metadata path: model=%q path=%q", id, path)
					c.suppressedMetadata[key] = true
				}
				continue
			}
			delete(c.suppressedMetadata, key)
			if !sameLeaf(b, last, path) {
				changed = true
			}
		}
	}
	return changed
}

func modelIndex(models []*types.ModelInfo) map[string]*types.ModelInfo {
	result := make(map[string]*types.ModelInfo)
	for _, m := range models {
		if m != nil {
			result[m.GetId()] = m
		}
	}
	return result
}

func sameModelIDs(a, b map[string]*types.ModelInfo) bool {
	if len(a) != len(b) {
		return false
	}
	for id := range a {
		if _, ok := b[id]; !ok {
			return false
		}
	}
	return true
}

func sameLeaf(a, b map[string]any, path string) bool {
	x, xok := a[path]
	y, yok := b[path]
	return xok == yok && reflect.DeepEqual(x, y)
}

// Keep typed protobuf fields and raw JSON in separate projections: ignoring a
// volatile raw /created alone would still compare its typed duplicate. Numbers
// retain their exact decimal representation, including integers above 2^53.
func metadataLeaves(model *types.ModelInfo) (map[string]any, error) {
	typed := proto.Clone(model).(*types.ModelInfo)
	raw := typed.RawJson
	typed.RawJson = nil
	encoded, err := json.Marshal(typed)
	if err != nil {
		return nil, err
	}
	payload, err := decodeJSONObject(encoded)
	if err != nil {
		return nil, err
	}
	result := make(map[string]any)
	flattenMetadata(result, "/typed", payload)
	if len(raw) > 0 {
		payload, err = decodeJSONObject(raw)
		if err != nil {
			return nil, err
		}
		flattenMetadata(result, "/raw", payload)
	}
	return result, nil
}

// Container markers distinguish missing, null, empty objects/arrays, type changes
// and array lengths. Elements are compared by index, including nested arrays.
func flattenMetadata(out map[string]any, path string, value any) {
	switch v := value.(type) {
	case map[string]any:
		out[path] = "object"
		for key, child := range v {
			key = strings.ReplaceAll(strings.ReplaceAll(key, "~", "~0"), "/", "~1")
			flattenMetadata(out, path+"/"+key, child)
		}
	case []any:
		out[path] = fmt.Sprintf("array:%d", len(v))
		for i, child := range v {
			flattenMetadata(out, path+"/"+strconv.Itoa(i), child)
		}
	default:
		// Wrap scalar leaves to distinguish strings from container markers.
		out[path] = struct{ Value any }{value}
	}
}
