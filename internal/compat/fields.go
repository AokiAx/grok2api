package compat

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaxUpstreamTools soft-caps pathological agent payloads after namespace expansion.
const MaxUpstreamTools = 512

// responsesAllowedFields is the whitelist of fields accepted by the Grok
// /responses endpoint. Anything not in this set is silently dropped to avoid
// 422/400 rejections from client-added extras (external_web_access, metadata,
// user, tool_resources, …).
var responsesAllowedFields = map[string]struct{}{
	"model":                   {},
	"stream":                  {},
	"input":                   {},
	"max_output_tokens":       {},
	"temperature":             {},
	"top_p":                   {},
	"tools":                   {},
	"tool_choice":             {},
	"backend_search":          {},
	"supports_backend_search": {},
	"web_search":              {},
	"include":                 {},
	"instructions":            {},
	"reasoning_effort":        {},
	"reasoning":               {},
}

// StripUnknownResponsesFields removes fields not in the Responses whitelist.
func StripUnknownResponsesFields(payload []byte) ([]byte, error) {
	var input map[string]any
	if err := json.Unmarshal(payload, &input); err != nil {
		return nil, fmt.Errorf("decode responses request: %w", err)
	}
	changed := false
	for key := range input {
		if _, ok := responsesAllowedFields[key]; !ok {
			delete(input, key)
			changed = true
		}
	}
	if !changed {
		return payload, nil
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encode responses request: %w", err)
	}
	return encoded, nil
}

// EnsureBackendSearch sets backend_search when the model supports native search
// and the client has not already provided an explicit value.
//
// Respects client-provided backend_search / web_search (including false).
func EnsureBackendSearch(payload []byte, enabled bool) ([]byte, error) {
	if !enabled {
		return payload, nil
	}
	var input map[string]any
	if err := json.Unmarshal(payload, &input); err != nil {
		return nil, fmt.Errorf("decode responses request: %w", err)
	}
	if _, exists := input["backend_search"]; exists {
		return payload, nil
	}
	if _, exists := input["web_search"]; exists {
		input["backend_search"] = truthy(input["web_search"])
	} else {
		input["backend_search"] = true
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encode responses request: %w", err)
	}
	return encoded, nil
}

// SetJSONStreamFlag forces the stream field on a JSON object payload.
func SetJSONStreamFlag(payload []byte, stream bool) ([]byte, error) {
	var input map[string]any
	if err := json.Unmarshal(payload, &input); err != nil {
		return nil, fmt.Errorf("decode json body: %w", err)
	}
	if existing, ok := input["stream"].(bool); ok && existing == stream {
		return payload, nil
	}
	input["stream"] = stream
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encode json body: %w", err)
	}
	return encoded, nil
}

func truthy(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "1", "true", "yes", "on":
			return true
		default:
			return false
		}
	case float64:
		return typed != 0
	case int:
		return typed != 0
	default:
		return value != nil
	}
}
