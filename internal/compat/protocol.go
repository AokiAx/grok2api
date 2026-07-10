package compat

import (
	"encoding/json"
	"fmt"
	"strings"
)

// MaxUpstreamTools soft-caps pathological agent payloads after namespace expansion.
const MaxUpstreamTools = 512

// ChatToResponses converts an OpenAI Chat Completions body into a Responses body.
//
// Tools are kept. Codex-style {type:"namespace"} groups are expanded into nested
// function tools so strict Responses backends do not 422 on unknown variant
// "namespace". Tool capability is preserved; only the grouping shell is flattened.
//
// Native Grok WebSearch is server-side "backend search". Clients can enable it via:
//   - backend_search / web_search / supports_backend_search
//   - web_search_options
//   - tools: [{ "type": "web_search" }] (OpenAI-style)
//
// Call EnsureBackendSearch after conversion to default-on for catalog models that
// advertise supports_backend_search.
func ChatToResponses(payload []byte) ([]byte, bool, error) {
	var input map[string]any
	if err := json.Unmarshal(payload, &input); err != nil {
		return nil, false, fmt.Errorf("decode chat request: %w", err)
	}

	stream, _ := input["stream"].(bool)
	model, _ := input["model"].(string)

	output := map[string]any{
		"model":  model,
		"stream": stream,
	}
	if rawMessages, ok := input["messages"].([]any); ok {
		output["input"] = sanitizeMessages(rawMessages)
	}
	if maxTokens := firstNumber(input, "max_tokens", "max_completion_tokens"); maxTokens != nil {
		output["max_output_tokens"] = maxTokens
	}
	// Core sampling fields.
	copyFields(output, input, "temperature", "top_p")
	// Backend search / tool flags used by CLI-backed models.
	for _, key := range []string{"backend_search", "supports_backend_search", "web_search", "include", "instructions"} {
		if value, ok := input[key]; ok {
			output[key] = value
		}
	}
	// Keep tools for agent clients; expand Codex namespaces for strict backends.
	if tools := NormalizeResponsesTools(input["tools"], MaxUpstreamTools); len(tools) > 0 {
		output["tools"] = tools
	}
	if choice, ok := input["tool_choice"]; ok && choice != nil {
		output["tool_choice"] = choice
	}
	// OpenAI-style web_search_options -> backend_search enablement signal.
	if _, ok := input["web_search_options"]; ok {
		if _, exists := output["backend_search"]; !exists {
			output["backend_search"] = true
		}
	}
	// OpenAI Responses-style built-in web_search tool.
	if hasWebSearchTool(input["tools"]) {
		if _, exists := output["backend_search"]; !exists {
			output["backend_search"] = true
		}
	}

	if effort := extractReasoningEffort(input); effort != "" {
		output["reasoning_effort"] = effort
	}

	encoded, err := json.Marshal(output)
	if err != nil {
		return nil, false, fmt.Errorf("encode responses request: %w", err)
	}
	return encoded, stream, nil
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
		// Mirror explicit web_search into backend_search for CLI-compatible backends.
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

func hasWebSearchTool(raw any) bool {
	tools, ok := raw.([]any)
	if !ok {
		return false
	}
	for _, item := range tools {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(stringValue(tool["type"]))) {
		case "web_search", "web_search_preview", "websearch":
			return true
		}
	}
	return false
}

// NormalizeResponsesTools adapts client tool lists for strict Responses backends.
//
// Policy:
//  1. Expand Codex {type:"namespace", tools:[function...]} into flat function tools
//  2. Keep function/web_search/mcp/shell and other known types
//  3. Soft-cap count when maxTools > 0
//
// This preserves tool capability while avoiding 422 "unknown variant namespace".
func NormalizeResponsesTools(raw any, maxTools int) []any {
	tools, ok := raw.([]any)
	if !ok || len(tools) == 0 {
		return nil
	}
	out := make([]any, 0, len(tools))
	seen := map[string]struct{}{}
	appendTool := func(tool map[string]any) {
		if maxTools > 0 && len(out) >= maxTools {
			return
		}
		typeName := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
		name := strings.TrimSpace(stringValue(tool["name"]))
		if name == "" {
			if fn, ok := tool["function"].(map[string]any); ok {
				name = strings.TrimSpace(stringValue(fn["name"]))
			}
		}
		if name != "" {
			key := typeName + "\x00" + name
			if _, exists := seen[key]; exists {
				return
			}
			seen[key] = struct{}{}
		}
		// Strict Responses backends require function.parameters.
		if typeName == "function" {
			if fn, ok := tool["function"].(map[string]any); ok {
				if _, has := fn["parameters"]; !has {
					fn["parameters"] = map[string]any{
						"type":       "object",
						"properties": map[string]any{},
					}
				}
			}
		}
		out = append(out, tool)
	}

	for _, item := range tools {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		typeName := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
		switch typeName {
		case "namespace":
			nested, _ := tool["tools"].([]any)
			for _, child := range nested {
				fnTool, ok := child.(map[string]any)
				if !ok {
					continue
				}
				childType := strings.ToLower(strings.TrimSpace(stringValue(fnTool["type"])))
				if childType == "" {
					childType = "function"
				}
				if childType != "function" {
					continue
				}
				flat := map[string]any{}
				for k, v := range fnTool {
					flat[k] = v
				}
				flat["type"] = "function"
				if strings.TrimSpace(stringValue(flat["name"])) == "" {
					if fn, ok := flat["function"].(map[string]any); ok {
						if n := strings.TrimSpace(stringValue(fn["name"])); n != "" {
							flat["name"] = n
						}
					}
				}
				appendTool(flat)
			}
		case "", "function":
			cloned := map[string]any{}
			for k, v := range tool {
				cloned[k] = v
			}
			if typeName == "" {
				if _, hasFn := cloned["function"]; hasFn || strings.TrimSpace(stringValue(cloned["name"])) != "" {
					cloned["type"] = "function"
				} else {
					continue
				}
			}
			// Ensure top-level name for strict Responses backends.
			if strings.TrimSpace(stringValue(cloned["name"])) == "" {
				if fn, ok := cloned["function"].(map[string]any); ok {
					if n := strings.TrimSpace(stringValue(fn["name"])); n != "" {
						cloned["name"] = n
					}
				}
			}
			appendTool(cloned)
		default:
			// Keep known and unknown non-namespace tools as-is.
			appendTool(tool)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
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

// sanitizeMessages normalizes messages for the Responses input array.
// Multimodal content arrays are flattened to plain text.
// Tool-loop fields (tool_calls, tool_call_id, name) are preserved so agent
// clients like Cursor can continue multi-turn tool use.
func sanitizeMessages(messages []any) []any {
	out := make([]any, 0, len(messages))
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		clean := map[string]any{
			"role": msg["role"],
		}
		// Flatten content: array of content parts → concatenated text string.
		switch content := msg["content"].(type) {
		case string:
			clean["content"] = content
		case []any:
			clean["content"] = flattenContentParts(content)
		case nil:
			// assistant messages with tool_calls may have nil content
		default:
			clean["content"] = fmt.Sprint(content)
		}
		// Preserve tool-calling fields for agent multi-turn.
		for _, key := range []string{"tool_calls", "tool_call_id", "name", "function_call"} {
			if value, ok := msg[key]; ok && value != nil {
				clean[key] = value
			}
		}
		out = append(out, clean)
	}
	return out
}

// flattenContentParts extracts text from OpenAI multimodal content arrays.
func flattenContentParts(parts []any) string {
	var b strings.Builder
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch part["type"] {
		case "text":
			if text, _ := part["text"].(string); text != "" {
				b.WriteString(text)
			}
		case "image_url":
			// skip images — Grok Responses API handles images differently
		default:
			// unknown part type — try to extract text field
			if text, _ := part["text"].(string); text != "" {
				b.WriteString(text)
			}
		}
	}
	return b.String()
}

// NormalizeChatRequest fills defaults and normalizes reasoning_effort aliases.
func NormalizeChatRequest(payload []byte, defaultModel string) ([]byte, string, bool, error) {
	var input map[string]any
	if err := json.Unmarshal(payload, &input); err != nil {
		return nil, "", false, fmt.Errorf("decode chat request: %w", err)
	}
	model, _ := input["model"].(string)
	if strings.TrimSpace(model) == "" {
		model = defaultModel
		input["model"] = model
	}
	stream, _ := input["stream"].(bool)
	if effort := extractReasoningEffort(input); effort != "" {
		input["reasoning_effort"] = effort
		// Keep nested form for clients that send OpenAI-style reasoning objects.
		if _, ok := input["reasoning"].(map[string]any); !ok {
			input["reasoning"] = map[string]any{"effort": effort}
		}
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, "", false, fmt.Errorf("encode chat request: %w", err)
	}
	return encoded, model, stream, nil
}

// ResponsesToChat converts a non-stream Responses JSON body into Chat Completions JSON.
func ResponsesToChat(payload []byte) ([]byte, error) {
	var response map[string]any
	if err := json.Unmarshal(payload, &response); err != nil {
		return nil, fmt.Errorf("decode responses body: %w", err)
	}
	// Some proxies wrap the object under "response".
	if nested, ok := response["response"].(map[string]any); ok {
		response = nested
	}

	model := stringValue(response["model"])
	if model == "" {
		model = "grok"
	}
	text := extractResponsesText(response)
	finish := "stop"
	if status := stringValue(response["status"]); status == "incomplete" {
		finish = "length"
	}
	usage := map[string]any{
		"prompt_tokens":     0,
		"completion_tokens": 0,
		"total_tokens":      0,
	}
	if rawUsage, ok := response["usage"].(map[string]any); ok {
		inputTokens := intValue(rawUsage["input_tokens"])
		outputTokens := intValue(rawUsage["output_tokens"])
		usage["prompt_tokens"] = inputTokens
		usage["completion_tokens"] = outputTokens
		usage["total_tokens"] = inputTokens + outputTokens
	}

	id := stringValue(response["id"])
	if id == "" {
		id = "chatcmpl_" + randomID(20)
	}
	output := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": intValue(response["created_at"]),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": text,
				},
				"finish_reason": finish,
			},
		},
		"usage": usage,
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return nil, fmt.Errorf("encode chat completion: %w", err)
	}
	return encoded, nil
}

func extractReasoningEffort(input map[string]any) string {
	if effort := strings.TrimSpace(stringValue(input["reasoning_effort"])); effort != "" {
		return normalizeEffort(effort)
	}
	if reasoning, ok := input["reasoning"].(map[string]any); ok {
		if effort := strings.TrimSpace(stringValue(reasoning["effort"])); effort != "" {
			return normalizeEffort(effort)
		}
	}
	// Anthropic-style thinking budget is not mapped here.
	return ""
}

func normalizeEffort(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "none", "minimal", "low", "medium", "high", "xhigh", "max":
		if strings.EqualFold(value, "max") {
			return "xhigh"
		}
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return strings.TrimSpace(value)
	}
}

func extractResponsesText(response map[string]any) string {
	if text := strings.TrimSpace(stringValue(response["output_text"])); text != "" {
		return text
	}
	parts := make([]string, 0)
	rawOutput, _ := response["output"].([]any)
	for _, item := range rawOutput {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch stringValue(object["type"]) {
		case "message", "":
			content, _ := object["content"].([]any)
			for _, rawPart := range content {
				part, ok := rawPart.(map[string]any)
				if !ok {
					continue
				}
				switch stringValue(part["type"]) {
				case "output_text", "text", "":
					if text := stringValue(part["text"]); text != "" {
						parts = append(parts, text)
					}
				}
			}
		}
	}
	return strings.Join(parts, "")
}

func firstNumber(input map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := input[key]; ok && value != nil {
			return value
		}
	}
	return nil
}

func intValue(value any) int {
	switch typed := value.(type) {
	case float64:
		return int(typed)
	case int:
		return typed
	case int64:
		return int(typed)
	case json.Number:
		parsed, _ := typed.Int64()
		return int(parsed)
	default:
		return 0
	}
}
