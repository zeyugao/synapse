package server

import (
	"bytes"
	"encoding/json"
	"log"
	"reflect"

	"github.com/zeyugao/synapse/internal/types"
)

func mergedModelPayload(models []*types.ModelInfo, modelID string) map[string]any {
	values := make([]any, 0, len(models))
	for _, model := range models {
		payload := modelPayload(model)
		if payload == nil {
			continue
		}
		values = append(values, payload)
	}

	var merged map[string]any
	if len(values) > 0 {
		if value, ok := mergeJSONValues(values); ok {
			if object, ok := value.(map[string]any); ok {
				merged = object
			}
		}
	}
	if merged == nil {
		merged = make(map[string]any)
	}

	if modelID != "" {
		merged["id"] = modelID
	}
	return merged
}

func modelPayload(model *types.ModelInfo) map[string]any {
	if model == nil {
		return nil
	}

	if raw := model.GetRawJson(); len(raw) > 0 {
		payload, err := decodeRawJSONObject(raw)
		if err == nil {
			return payload
		}
		log.Printf("Failed to decode raw model JSON for %q: %v", model.GetId(), err)
	}

	payload := make(map[string]any)
	if model.GetId() != "" {
		payload["id"] = model.GetId()
	}
	if model.GetObject() != "" {
		payload["object"] = model.GetObject()
	}
	if model.GetCreated() != 0 {
		payload["created"] = model.GetCreated()
	}
	if model.GetOwnedBy() != "" {
		payload["owned_by"] = model.GetOwnedBy()
	}
	if len(payload) == 0 {
		return nil
	}
	return payload
}

func decodeRawJSONObject(raw []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func mergeJSONValues(values []any) (any, bool) {
	if len(values) == 0 {
		return nil, false
	}
	if allJSONValuesEqual(values) {
		return cloneJSONValue(values[0]), true
	}

	switch values[0].(type) {
	case map[string]any:
		objects := make([]map[string]any, 0, len(values))
		for _, value := range values {
			object, ok := value.(map[string]any)
			if !ok {
				return nil, false
			}
			objects = append(objects, object)
		}
		return mergeJSONObjects(objects)
	case []any:
		arrays := make([][]any, 0, len(values))
		for _, value := range values {
			array, ok := value.([]any)
			if !ok {
				return nil, false
			}
			arrays = append(arrays, array)
		}
		return mergeJSONArrays(arrays)
	case nil, bool, string, json.Number, float64:
		return nil, false
	default:
		return nil, false
	}
}

func mergeJSONObjects(objects []map[string]any) (any, bool) {
	if len(objects) == 0 {
		return nil, false
	}

	result := make(map[string]any)
	for key, firstValue := range objects[0] {
		values := make([]any, 0, len(objects))
		values = append(values, firstValue)

		presentInAll := true
		for i := 1; i < len(objects); i++ {
			value, ok := objects[i][key]
			if !ok {
				presentInAll = false
				break
			}
			values = append(values, value)
		}
		if !presentInAll {
			continue
		}

		if merged, ok := mergeJSONValues(values); ok {
			result[key] = merged
		}
	}

	if len(result) == 0 {
		return nil, false
	}
	return result, true
}

func mergeJSONArrays(arrays [][]any) (any, bool) {
	if len(arrays) == 0 {
		return nil, false
	}

	used := make([]map[int]struct{}, len(arrays))
	for i := range used {
		used[i] = make(map[int]struct{})
	}

	result := make([]any, 0)
	for _, candidate := range arrays[0] {
		merged, ok := matchArrayElement(candidate, arrays, used)
		if !ok {
			continue
		}
		result = append(result, merged)
	}

	if len(result) == 0 {
		return nil, false
	}
	return result, true
}

func matchArrayElement(candidate any, arrays [][]any, used []map[int]struct{}) (any, bool) {
	if id, ok := jsonObjectID(candidate); ok {
		values := []any{candidate}
		matches := make([]int, len(arrays))
		for arrayIndex := 1; arrayIndex < len(arrays); arrayIndex++ {
			matchIndex := -1
			for elementIndex, element := range arrays[arrayIndex] {
				if _, taken := used[arrayIndex][elementIndex]; taken {
					continue
				}
				otherID, ok := jsonObjectID(element)
				if ok && otherID == id {
					matchIndex = elementIndex
					values = append(values, element)
					break
				}
			}
			if matchIndex == -1 {
				return nil, false
			}
			matches[arrayIndex] = matchIndex
		}

		merged, ok := mergeJSONValues(values)
		if !ok {
			return nil, false
		}
		for arrayIndex := 1; arrayIndex < len(arrays); arrayIndex++ {
			used[arrayIndex][matches[arrayIndex]] = struct{}{}
		}
		return merged, true
	}

	values := []any{candidate}
	matches := make([]int, len(arrays))
	for arrayIndex := 1; arrayIndex < len(arrays); arrayIndex++ {
		matchIndex := -1
		for elementIndex, element := range arrays[arrayIndex] {
			if _, taken := used[arrayIndex][elementIndex]; taken {
				continue
			}
			if reflect.DeepEqual(candidate, element) {
				matchIndex = elementIndex
				values = append(values, element)
				break
			}
		}
		if matchIndex == -1 {
			return nil, false
		}
		matches[arrayIndex] = matchIndex
	}

	for arrayIndex := 1; arrayIndex < len(arrays); arrayIndex++ {
		used[arrayIndex][matches[arrayIndex]] = struct{}{}
	}

	return cloneJSONValue(values[0]), true
}

func jsonObjectID(value any) (string, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return "", false
	}
	id, ok := object["id"].(string)
	if !ok || id == "" {
		return "", false
	}
	return id, true
}

func allJSONValuesEqual(values []any) bool {
	for i := 1; i < len(values); i++ {
		if !reflect.DeepEqual(values[0], values[i]) {
			return false
		}
	}
	return true
}

func cloneJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		cloned := make(map[string]any, len(typed))
		for key, nested := range typed {
			cloned[key] = cloneJSONValue(nested)
		}
		return cloned
	case []any:
		cloned := make([]any, len(typed))
		for i, nested := range typed {
			cloned[i] = cloneJSONValue(nested)
		}
		return cloned
	case json.Number:
		return typed
	default:
		return typed
	}
}
