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

// codexRejectedFields are known client extras that Grok rejects with
// "Argument not supported: …" (Codex / OpenAI Responses clients).
var codexRejectedFields = []string{
	"external_web_access",
	"prompt_cache_key",
	"safety_identifier",
	"service_tier",
	"store",
	"background",
	"parallel_tool_calls",
	"previous_response_id",
	"truncation",
	"user",
	"metadata",
	"tool_resources",
	"max_tool_calls",
	"prompt_cache_retention",
}

// StripUnknownResponsesFields removes fields not in the Responses whitelist.
func StripUnknownResponsesFields(payload []byte) ([]byte, error) {
	var input map[string]any
	if err := json.Unmarshal(payload, &input); err != nil {
		return nil, fmt.Errorf("decode responses request: %w", err)
	}
	changed := sanitizeResponsesMap(input)
	if !changed {
		return payload, nil
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encode responses request: %w", err)
	}
	return encoded, nil
}

// sanitizeResponsesMap mutates input in place: maps client web-access flags onto
// backend_search, then drops every non-whitelisted field.
func sanitizeResponsesMap(input map[string]any) bool {
	changed := false

	// Codex / OpenAI clients send external_web_access; Grok uses backend_search.
	if raw, ok := input["external_web_access"]; ok {
		if _, exists := input["backend_search"]; !exists {
			input["backend_search"] = truthy(raw)
			changed = true
		}
		delete(input, "external_web_access")
		changed = true
	}

	for _, key := range codexRejectedFields {
		if _, ok := input[key]; ok {
			delete(input, key)
			changed = true
		}
	}

	for key := range input {
		if _, ok := responsesAllowedFields[key]; !ok {
			delete(input, key)
			changed = true
		}
	}
	return changed
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
