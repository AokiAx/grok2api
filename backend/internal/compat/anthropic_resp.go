package compat

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// ResponsesToAnthropic converts a non-stream Grok/OpenAI Responses JSON body
// into an Anthropic Messages response. Reasoning items become thinking blocks
// with opaque thinking signatures when thinking is enabled.
func ResponsesToAnthropic(payload []byte, requestModel string, thinkingEnabled bool, thinkingDisplay string) ([]byte, error) {
	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil, fmt.Errorf("decode Responses body: %w", err)
	}
	// Unwrap response.completed / response.incomplete envelopes.
	if typ := stringValue(root["type"]); typ == "response.completed" || typ == "response.incomplete" {
		if nested, ok := root["response"].(map[string]any); ok {
			root = nested
		}
	}

	id := stringValue(root["id"])
	if id == "" {
		id = "msg_" + randomID(24)
	}
	if strings.HasPrefix(id, "resp_") {
		id = "msg_" + strings.TrimPrefix(id, "resp_")
	}

	model := strings.TrimSpace(requestModel)
	if model == "" {
		model = stringValue(root["model"])
	}
	if model == "" {
		model = "grok"
	}

	content := make([]any, 0)
	hasTool := false

	if output, ok := root["output"].([]any); ok {
		for _, raw := range output {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(stringValue(item["type"]))) {
			case "message":
				content = append(content, extractAnthropicTextBlocks(item["content"])...)
			case "function_call", "tool_call":
				hasTool = true
				callID := firstNonEmptyString(item["call_id"], item["id"])
				if callID == "" {
					callID = "toolu_" + randomID(12)
				}
				args := stringValue(item["arguments"])
				if args == "" {
					if fn, ok := item["function"].(map[string]any); ok {
						args = stringValue(fn["arguments"])
					}
				}
				if args == "" {
					args = "{}"
				}
				if !json.Valid([]byte(args)) {
					args = "{}"
				}
				var input any
				if err := json.Unmarshal([]byte(args), &input); err != nil {
					input = map[string]any{}
				}
				name := stringValue(item["name"])
				if name == "" {
					if fn, ok := item["function"].(map[string]any); ok {
						name = stringValue(fn["name"])
					}
				}
				content = append(content, map[string]any{
					"type":  "tool_use",
					"id":    callID,
					"name":  name,
					"input": input,
				})
			case "reasoning":
				if !thinkingEnabled {
					continue
				}
				summary := extractReasoningSummaryText(item["summary"])
				if thinkingDisplay == "omitted" {
					summary = ""
				}
				signature := stringValue(item["encrypted_content"])
				if summary == "" && signature == "" {
					continue
				}
				content = append(content, map[string]any{
					"type":      "thinking",
					"thinking":  summary,
					"signature": signature,
				})
			}
		}
	}

	if len(content) == 0 {
		if text := stringValue(root["output_text"]); text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
	}
	if content == nil {
		content = []any{}
	}

	inputTokens, outputTokens := 0, 0
	if usage, ok := root["usage"].(map[string]any); ok {
		inputTokens = intValue(firstNonNil(usage["input_tokens"], usage["prompt_tokens"]))
		outputTokens = intValue(firstNonNil(usage["output_tokens"], usage["completion_tokens"]))
	}

	stopReason := "end_turn"
	if hasTool {
		stopReason = "tool_use"
	} else if strings.EqualFold(stringValue(root["status"]), "incomplete") {
		stopReason = "max_tokens"
	}

	out := map[string]any{
		"id":            id,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
		},
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("encode Anthropic response: %w", err)
	}
	return encoded, nil
}

// AggregateResponsesToAnthropic reads a Responses SSE stream and returns a
// complete Anthropic Messages JSON body.
func AggregateResponsesToAnthropic(stream io.ReadCloser, requestModel string, thinkingEnabled bool, thinkingDisplay string) ([]byte, error) {
	defer stream.Close()
	raw, err := io.ReadAll(stream)
	if err != nil {
		return nil, fmt.Errorf("read responses stream: %w", err)
	}
	if len(bytesTrimSpace(raw)) > 0 && raw[0] == '{' && json.Valid(raw) {
		return ResponsesToAnthropic(raw, requestModel, thinkingEnabled, thinkingDisplay)
	}
	if completed := ExtractCompletedResponse(raw); len(completed) > 0 {
		return ResponsesToAnthropic(completed, requestModel, thinkingEnabled, thinkingDisplay)
	}
	// Fallback: synthesize from deltas when completed envelope is missing.
	var textBuilder strings.Builder
	var finalBody []byte
	_ = IterateSSEBytes(raw, func(event SSEEvent) error {
		switch event.Type {
		case "response.output_text.delta":
			textBuilder.WriteString(stringValue(event.Payload["delta"]))
		case "response.completed":
			if response, ok := event.Payload["response"].(map[string]any); ok {
				if encoded, err := json.Marshal(response); err == nil {
					finalBody = encoded
				}
			}
		}
		return nil
	})
	if len(finalBody) > 0 {
		return ResponsesToAnthropic(finalBody, requestModel, thinkingEnabled, thinkingDisplay)
	}
	synthetic := map[string]any{
		"id":          "resp_" + randomID(12),
		"model":       requestModel,
		"status":      "completed",
		"output_text": textBuilder.String(),
		"output": []any{
			map[string]any{
				"type":    "message",
				"role":    "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": textBuilder.String()}},
			},
		},
	}
	encoded, _ := json.Marshal(synthetic)
	return ResponsesToAnthropic(encoded, requestModel, thinkingEnabled, thinkingDisplay)
}

func extractAnthropicTextBlocks(content any) []any {
	out := make([]any, 0)
	switch typed := content.(type) {
	case string:
		if typed != "" {
			out = append(out, map[string]any{"type": "text", "text": typed})
		}
	case []any:
		for _, raw := range typed {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(stringValue(part["type"]))) {
			case "output_text", "text", "input_text", "":
				if text := stringValue(part["text"]); text != "" {
					out = append(out, map[string]any{"type": "text", "text": text})
				}
			}
		}
	}
	return out
}

func extractReasoningSummaryText(raw any) string {
	switch typed := raw.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			part, ok := item.(map[string]any)
			if !ok {
				if s, ok := item.(string); ok && s != "" {
					parts = append(parts, s)
				}
				continue
			}
			text := firstNonEmptyString(part["text"], part["content"])
			if text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func bytesTrimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}
