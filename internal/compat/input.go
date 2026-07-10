package compat

import (
	"fmt"
	"strings"
)

// ChatMessagesToResponsesInput converts OpenAI Chat Completions messages into
// Responses API input items.
//
// Mapping (aligned with Continue.dev / OpenAI Responses agent clients):
//   - system → accumulated into instructions (not duplicated in input)
//   - user/assistant text → {role, content}
//   - assistant tool_calls → {type:function_call, call_id, name, arguments}
//   - tool results → {type:function_call_output, call_id, output}
func ChatMessagesToResponsesInput(messages []any) (input []any, instructions string) {
	input = make([]any, 0, len(messages))
	var systemParts []string

	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(stringValue(msg["role"])))
		content := normalizeMessageContent(msg["content"])

		switch role {
		case "system", "developer":
			if text := strings.TrimSpace(content); text != "" {
				systemParts = append(systemParts, text)
			}
		case "assistant":
			if content != "" {
				input = append(input, map[string]any{
					"role":    "assistant",
					"content": content,
				})
			}
			if calls, ok := msg["tool_calls"].([]any); ok {
				for _, rawCall := range calls {
					if item := chatToolCallToFunctionCall(rawCall); item != nil {
						input = append(input, item)
					}
				}
			} else if call, ok := msg["function_call"].(map[string]any); ok {
				// Legacy OpenAI function_call on the message.
				name := stringValue(call["name"])
				args := stringValue(call["arguments"])
				if name != "" {
					input = append(input, map[string]any{
						"type":      "function_call",
						"call_id":   "call_" + randomID(12),
						"name":      name,
						"arguments": firstNonEmpty(args, "{}"),
					})
				}
			} else if content == "" {
				// Empty assistant with no tools — still keep a placeholder turn.
				input = append(input, map[string]any{
					"role":    "assistant",
					"content": "",
				})
			}
		case "tool":
			callID := firstNonEmptyString(msg["tool_call_id"], msg["id"])
			if callID == "" {
				callID = "call_" + randomID(12)
			}
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": callID,
				"output":  content,
			})
		default:
			// user and any other roles
			item := map[string]any{
				"role":    firstNonEmpty(role, "user"),
				"content": content,
			}
			if name := strings.TrimSpace(stringValue(msg["name"])); name != "" {
				item["name"] = name
			}
			input = append(input, item)
		}
	}

	if len(systemParts) > 0 {
		instructions = strings.Join(systemParts, "\n\n")
	}
	return input, instructions
}

func chatToolCallToFunctionCall(raw any) map[string]any {
	call, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	function, _ := call["function"].(map[string]any)
	name := firstNonEmptyString(call["name"], function["name"])
	if name == "" {
		return nil
	}
	callID := firstNonEmptyString(call["id"], call["call_id"], function["id"])
	if callID == "" {
		callID = "call_" + randomID(12)
	}
	arguments := jsonString(firstNonNil(call["arguments"], function["arguments"]))
	if arguments == "" {
		arguments = "{}"
	}
	return map[string]any{
		"type":      "function_call",
		"call_id":   callID,
		"name":      name,
		"arguments": arguments,
	}
}

func normalizeMessageContent(content any) string {
	switch typed := content.(type) {
	case string:
		return typed
	case []any:
		return flattenContentParts(typed)
	case nil:
		return ""
	default:
		return fmt.Sprint(typed)
	}
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
			// Images are not forwarded on the Responses text path.
		default:
			if text, _ := part["text"].(string); text != "" {
				b.WriteString(text)
			}
		}
	}
	return b.String()
}
