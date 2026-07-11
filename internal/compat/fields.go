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
	// Structured output (Anthropic output_config.format → text.format).
	"text": {},
	// Kept for session continuity when upstream accepts it (Claude Code).
	"prompt_cache_key": {},
}

// codexRejectedFields are known client extras that Grok rejects with
// "Argument not supported: …" (Codex / OpenAI Responses clients).
var codexRejectedFields = []string{
	"external_web_access",
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
//
// Also re-sanitizes tools[] so nested Codex extras (web_search.external_web_access,
// search_context_size, local_shell, …) never reach Grok even if prepare was skipped.
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

	// Nested tool sanitize (Codex puts external_web_access on tools, not only root).
	if rawTools, ok := input["tools"]; ok {
		result := NormalizeResponsesToolsDetailed(rawTools, MaxUpstreamTools)
		if result.BackendSearch != nil {
			if _, exists := input["backend_search"]; !exists {
				input["backend_search"] = *result.BackendSearch
				changed = true
			}
		}
		if len(result.Tools) > 0 {
			input["tools"] = result.Tools
			changed = true
		} else if rawTools != nil {
			delete(input, "tools")
			changed = true
		}
	}

	// Codex multi-turn items (local_shell_call, item_reference, …) → Grok ModelInput.
	if rawInput, ok := input["input"]; ok {
		sanitized := SanitizeResponsesInput(rawInput)
		before, _ := json.Marshal(rawInput)
		after, _ := json.Marshal(sanitized)
		if string(before) != string(after) {
			input["input"] = sanitized
			changed = true
		}
	}

	// grok-4.5 rejects reasoning_effort=none ("This model does not support … none").
	// Drop none so the request is valid; models that support disable get default.
	if stripped := stripUnsupportedReasoningNone(input); stripped {
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

// stripUnsupportedReasoningNone removes reasoning_effort/reasoning.effort when
// the value is "none" (unsupported on grok-4.5 and most production models).
func stripUnsupportedReasoningNone(input map[string]any) bool {
	changed := false
	if effort, ok := input["reasoning_effort"].(string); ok && strings.EqualFold(strings.TrimSpace(effort), "none") {
		delete(input, "reasoning_effort")
		changed = true
	}
	if reasoning, ok := input["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok && strings.EqualFold(strings.TrimSpace(effort), "none") {
			delete(reasoning, "effort")
			changed = true
			if len(reasoning) == 0 {
				delete(input, "reasoning")
			} else {
				input["reasoning"] = reasoning
			}
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

// defaultSearchToolTypes are Grok built-in search tools injected when the model
// supports backend search and the client has not disabled it.
var defaultSearchToolTypes = []string{"web_search", "x_search"}

// EnsureDefaultSearchTools prepends bare web_search / x_search tools when the
// model supports native search and search is not explicitly disabled.
//
// Policy:
//   - enabled=false → no-op
//   - backend_search / web_search explicitly false → no-op
//   - existing tools of the same type are left as-is (no duplicates)
//   - function tools named web_search/x_search are collapsed to bare builtins
//   - client function tools are preserved after the search tools
func EnsureDefaultSearchTools(payload []byte, enabled bool) ([]byte, error) {
	if !enabled {
		return payload, nil
	}
	var input map[string]any
	if err := json.Unmarshal(payload, &input); err != nil {
		return nil, fmt.Errorf("decode responses request: %w", err)
	}
	if raw, ok := input["backend_search"]; ok && !truthy(raw) {
		return payload, nil
	}
	if raw, ok := input["web_search"]; ok && !truthy(raw) {
		return payload, nil
	}

	existing, _ := input["tools"].([]any)
	have := map[string]bool{}
	for _, item := range existing {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		typeName := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
		have[typeName] = true
		// function:web_search already covers the search capability name.
		if typeName == "function" || typeName == "" {
			if name := strings.ToLower(strings.TrimSpace(firstNonEmptyString(tool["name"]))); name != "" {
				have[name] = true
			}
		}
	}

	prefix := make([]any, 0, len(defaultSearchToolTypes))
	for _, typeName := range defaultSearchToolTypes {
		if have[typeName] {
			continue
		}
		prefix = append(prefix, map[string]any{"type": typeName})
	}

	merged := make([]any, 0, len(prefix)+len(existing))
	merged = append(merged, prefix...)
	merged = append(merged, existing...)
	// Always collapse function:web_search vs bare web_search after inject.
	merged = CollapseSearchToolNameCollisions(merged)
	if MaxUpstreamTools > 0 && len(merged) > MaxUpstreamTools {
		merged = merged[:MaxUpstreamTools]
	}
	// No change if already clean and nothing injected.
	if len(prefix) == 0 {
		before, _ := json.Marshal(existing)
		after, _ := json.Marshal(merged)
		if string(before) == string(after) {
			return payload, nil
		}
	}
	input["tools"] = merged
	// Keep native search flag aligned when we inject search tools.
	if _, exists := input["backend_search"]; !exists {
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
