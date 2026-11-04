package grok

import "encoding/json"

// CloneJSONObject detaches mutable tool declarations.
func CloneJSONObject(value map[string]any) map[string]any {
	cloned := make(map[string]any, len(value))
	for key, item := range value {
		cloned[key] = CloneJSONValue(item)
	}
	return cloned
}

// CloneJSONValue preserves the previous JSON round-trip numeric semantics.
func CloneJSONValue(value any) any {
	data, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var cloned any
	if json.Unmarshal(data, &cloned) != nil {
		return value
	}
	return cloned
}
