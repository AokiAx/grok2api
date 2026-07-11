package compat

import (
	"encoding/json"
	"fmt"
	"strings"
)

// SanitizeResponsesInput rewrites Codex/OpenAI Responses input arrays into
// shapes accepted by Grok's ModelInput enum.
//
// Grok rejects (422 "data did not match any variant of untagged enum ModelInput"):
//   - item_reference, local_shell_call, web_search_call, computer_call,
//     custom_tool_call, null content, …
//
// Policy:
//   - Keep message / function_call / function_call_output / reasoning
//   - Map local_shell_* and custom_tool_* onto function_call(_output)
//   - Drop untranslatable built-in call items (web_search_call, item_reference, …)
//   - Coerce null message content to empty string
func SanitizeResponsesInput(raw any) any {
	switch typed := raw.(type) {
	case string:
		return typed
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			if sanitized := sanitizeInputItem(item); sanitized != nil {
				out = append(out, sanitized)
			}
		}
		if len(out) == 0 {
			// Never send empty array after dropping everything — keep a noop user turn.
			return []any{map[string]any{"role": "user", "content": ""}}
		}
		return out
	default:
		return raw
	}
}

func sanitizeInputItem(raw any) any {
	item, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	typeName := strings.ToLower(strings.TrimSpace(stringValue(item["type"])))
	switch typeName {
	case "", "message":
		return sanitizeMessageItem(item)
	case "function_call", "tool_call":
		return sanitizeFunctionCallItem(item)
	case "function_call_output", "tool_result", "tool_call_output":
		return sanitizeFunctionCallOutputItem(item)
	case "reasoning":
		// Grok accepts reasoning items in multi-turn history.
		return cloneMap(item)
	case "local_shell_call", "shell_call":
		return localShellCallToFunctionCall(item)
	case "local_shell_call_output", "shell_call_output":
		return map[string]any{
			"type":    "function_call_output",
			"call_id": firstNonEmptyString(item["call_id"], item["id"], "call_"+randomID(10)),
			"output":  stringifyOutput(item["output"]),
		}
	case "custom_tool_call":
		return customToolCallToFunctionCall(item)
	case "custom_tool_call_output":
		return map[string]any{
			"type":    "function_call_output",
			"call_id": firstNonEmptyString(item["call_id"], item["id"], "call_"+randomID(10)),
			"output":  stringifyOutput(item["output"]),
		}
	case "item_reference", "web_search_call", "web_search_call_output",
		"file_search_call", "file_search_call_output",
		"code_interpreter_call", "code_interpreter_call_output",
		"image_generation_call", "image_generation_call_output",
		"computer_call", "computer_call_output",
		"mcp_call", "mcp_list_tools", "mcp_approval_request", "mcp_approval_response",
		"apply_patch_call", "apply_patch_call_output":
		// Not representable on Grok ModelInput — drop to avoid 422.
		return nil
	default:
		// Unknown typed item: drop rather than 422 the whole request.
		if typeName != "" {
			return nil
		}
		return sanitizeMessageItem(item)
	}
}

func sanitizeMessageItem(item map[string]any) map[string]any {
	out := map[string]any{}
	if typ := strings.TrimSpace(stringValue(item["type"])); typ != "" {
		out["type"] = typ
	}
	role := strings.TrimSpace(stringValue(item["role"]))
	if role == "" {
		role = "user"
	}
	out["role"] = role
	if id := strings.TrimSpace(stringValue(item["id"])); id != "" {
		out["id"] = id
	}
	if status := strings.TrimSpace(stringValue(item["status"])); status != "" {
		out["status"] = status
	}
	if name := strings.TrimSpace(stringValue(item["name"])); name != "" {
		out["name"] = name
	}
	content := item["content"]
	if content == nil {
		out["content"] = ""
		return out
	}
	// Keep structured content arrays; only fix nulls inside.
	if parts, ok := content.([]any); ok {
		cleaned := make([]any, 0, len(parts))
		for _, raw := range parts {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			p := cloneMap(part)
			if _, hasText := p["text"]; hasText && p["text"] == nil {
				p["text"] = ""
			}
			cleaned = append(cleaned, p)
		}
		out["content"] = cleaned
		return out
	}
	out["content"] = content
	return out
}

func sanitizeFunctionCallItem(item map[string]any) map[string]any {
	callID := firstNonEmptyString(item["call_id"], item["id"])
	if callID == "" {
		callID = "call_" + randomID(12)
	}
	name := firstNonEmptyString(item["name"], item["tool_name"])
	if name == "" {
		if fn, ok := item["function"].(map[string]any); ok {
			name = firstNonEmptyString(fn["name"])
		}
	}
	if name == "" {
		return nil
	}
	arguments := jsonString(item["arguments"])
	if arguments == "" {
		arguments = jsonString(item["input"])
	}
	if arguments == "" {
		arguments = "{}"
	}
	out := map[string]any{
		"type":      "function_call",
		"call_id":   callID,
		"name":      name,
		"arguments": arguments,
	}
	if status := strings.TrimSpace(stringValue(item["status"])); status != "" {
		out["status"] = status
	}
	if id := strings.TrimSpace(stringValue(item["id"])); id != "" && id != callID {
		out["id"] = id
	}
	return out
}

func sanitizeFunctionCallOutputItem(item map[string]any) map[string]any {
	callID := firstNonEmptyString(item["call_id"], item["id"])
	if callID == "" {
		callID = "call_" + randomID(12)
	}
	return map[string]any{
		"type":    "function_call_output",
		"call_id": callID,
		"output":  stringifyOutput(item["output"]),
	}
}

func localShellCallToFunctionCall(item map[string]any) map[string]any {
	callID := firstNonEmptyString(item["call_id"], item["id"])
	if callID == "" {
		callID = "call_" + randomID(12)
	}
	action, _ := item["action"].(map[string]any)
	command := ""
	if action != nil {
		switch typed := action["command"].(type) {
		case string:
			command = typed
		case []any:
			parts := make([]string, 0, len(typed))
			for _, p := range typed {
				parts = append(parts, fmt.Sprint(p))
			}
			command = strings.Join(parts, " ")
		}
	}
	if command == "" {
		command = stringifyOutput(item["command"])
	}
	argsObj := map[string]any{"command": command}
	if action != nil {
		if wd := firstNonEmptyString(action["working_directory"], action["workdir"]); wd != "" {
			argsObj["workdir"] = wd
		}
	}
	encoded, err := json.Marshal(argsObj)
	if err != nil {
		encoded = []byte(`{"command":""}`)
	}
	return map[string]any{
		"type":      "function_call",
		"call_id":   callID,
		"name":      "shell_command",
		"arguments": string(encoded),
	}
}

func customToolCallToFunctionCall(item map[string]any) map[string]any {
	callID := firstNonEmptyString(item["call_id"], item["id"])
	if callID == "" {
		callID = "call_" + randomID(12)
	}
	name := firstNonEmptyString(item["name"], item["tool_name"])
	if name == "" {
		name = "custom_tool"
	}
	arguments := jsonString(item["input"])
	if arguments == "" {
		arguments = jsonString(item["arguments"])
	}
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

func stringifyOutput(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []byte:
		return string(typed)
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return fmt.Sprint(typed)
		}
		return string(encoded)
	}
}

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
		switch strings.ToLower(strings.TrimSpace(stringValue(part["type"]))) {
		case "text", "input_text", "output_text":
			if text, _ := part["text"].(string); text != "" {
				b.WriteString(text)
			}
		case "image_url", "input_image":
			// Images are not forwarded on the Responses text path.
		default:
			if text, _ := part["text"].(string); text != "" {
				b.WriteString(text)
			}
		}
	}
	return b.String()
}
