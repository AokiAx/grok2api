package compat

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ChatToResponses converts an OpenAI Chat Completions body into a Responses body.
//
// Only fields accepted by the Grok /responses API are forwarded. OpenAI-specific
// fields like tools, tool_choice, metadata, user, tool_resources are stripped
// because the upstream Rust backend rejects unknown fields (serde untagged enum).
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
	// Only forward fields the Grok Responses API actually accepts.
	copyFields(output, input, "temperature", "top_p")
	// Backend search / tool flags used by CLI-backed models.
	for _, key := range []string{"backend_search", "supports_backend_search", "web_search", "include", "instructions"} {
		if value, ok := input[key]; ok {
			output[key] = value
		}
	}
	// OpenAI-style web_search_options -> backend_search enablement signal.
	if _, ok := input["web_search_options"]; ok {
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

// sanitizeMessages strips OpenAI-specific fields from messages that the Grok
// Responses API does not understand (tool_calls, tool_call_id, name, function).
// Multimodal content arrays are flattened to plain text.
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
