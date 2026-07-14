package compat

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ResponsesToChat converts a non-stream Responses JSON body into Chat Completions JSON.
func ResponsesToChat(payload []byte) ([]byte, error) {
	var response map[string]any
	if err := json.Unmarshal(payload, &response); err != nil {
		return nil, fmt.Errorf("decode responses body: %w", err)
	}
	if nested, ok := response["response"].(map[string]any); ok {
		response = nested
	}

	model := stringValue(response["model"])
	if model == "" {
		model = "grok"
	}
	text, toolCalls := extractResponsesContent(response)
	finish := "stop"
	if status := stringValue(response["status"]); status == "incomplete" {
		finish = "length"
	}
	if len(toolCalls) > 0 {
		finish = "tool_calls"
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
		if total := intValue(rawUsage["total_tokens"]); total > 0 {
			usage["total_tokens"] = total
		} else {
			usage["total_tokens"] = inputTokens + outputTokens
		}
		// Pass through Grok prompt-cache metrics for chat clients / NewAPI.
		if details, ok := rawUsage["input_tokens_details"].(map[string]any); ok {
			usage["prompt_tokens_details"] = details
			if cached := intValue(details["cached_tokens"]); cached > 0 {
				// Common alias some dashboards look for.
				usage["cached_tokens"] = cached
			}
		} else if cached := intValue(rawUsage["cached_tokens"]); cached > 0 {
			usage["cached_tokens"] = cached
			usage["prompt_tokens_details"] = map[string]any{"cached_tokens": cached}
		}
	}

	id := stringValue(response["id"])
	if id == "" {
		id = "chatcmpl_" + randomID(20)
	}
	message := map[string]any{
		"role":    "assistant",
		"content": text,
	}
	// OpenAI allows null content when tool_calls are present; keep empty string
	// for broader client compatibility, but still attach tool_calls.
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
		if text == "" {
			message["content"] = nil
		}
	}
	output := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": intValue(response["created_at"]),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
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

func extractResponsesContent(response map[string]any) (text string, toolCalls []any) {
	if direct := strings.TrimSpace(stringValue(response["output_text"])); direct != "" {
		text = direct
	}
	parts := make([]string, 0)
	rawOutput, _ := response["output"].([]any)
	for _, item := range rawOutput {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(stringValue(object["type"]))) {
		case "message", "":
			content, _ := object["content"].([]any)
			for _, rawPart := range content {
				part, ok := rawPart.(map[string]any)
				if !ok {
					continue
				}
				switch stringValue(part["type"]) {
				case "output_text", "text", "":
					if value := stringValue(part["text"]); value != "" {
						parts = append(parts, value)
					}
				}
			}
		case "function_call", "tool_call", "custom_tool_call":
			if call := responsesItemToChatToolCall(object); call != nil {
				toolCalls = append(toolCalls, call)
			}
		}
	}
	if text == "" && len(parts) > 0 {
		text = strings.Join(parts, "")
	}
	return text, toolCalls
}

func responsesItemToChatToolCall(item map[string]any) map[string]any {
	callID, name, arguments := functionCallFields(item)
	if name == "" {
		return nil
	}
	if callID == "" {
		callID = "call_" + randomID(12)
	}
	if arguments == "" {
		if params := item["parameters"]; params != nil {
			arguments = jsonString(params)
		}
	}
	if arguments == "" {
		arguments = "{}"
	}
	return map[string]any{
		"id":   callID,
		"type": "function",
		"function": map[string]any{
			"name":      name,
			"arguments": arguments,
		},
	}
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
