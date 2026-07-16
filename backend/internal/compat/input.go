package compat

import (
	"encoding/json"
	"fmt"
	"strings"
)

// SanitizeResponsesInput rewrites Codex/OpenAI Responses input arrays into
// shapes accepted by Grok's ModelInput enum.
//
// Grok rejects native OpenAI built-ins (422 ModelInput). We never silently
// delete tool-loop semantics when a faithful mapping exists:
//
//   - message / function_call / function_call_output / reasoning → keep
//   - local_shell_* / custom_tool_* / web_search_* / computer_* / mcp_* /
//     file_search_* / code_interpreter_* / image_generation_* / apply_patch_*
//     → function_call(+_output) with structured arguments
//   - item_reference / bare references → assistant note (id preserved in text)
//   - unknown types → assistant note with type + compact payload
//   - null message content → ""
//   - role developer → system (Grok ModelInput rejects developer)
//   - OpenAI compaction blobs / foreign encrypted_content → dropped or note
//
// call_id pairing: missing ids on calls/outputs are paired in order within one
// request, not independently random.
func SanitizeResponsesInput(raw any) any {
	return SanitizeResponsesInputWithCompat(raw, newToolCompatibility())
}

// SanitizeResponsesInputWithCompat is SanitizeResponsesInput using a shared
// ToolCompatibility (so apply_patch/shell aliases match the tools[] pass).
func SanitizeResponsesInputWithCompat(raw any, state *ToolCompatibility) any {
	if state == nil {
		state = newToolCompatibility()
	}
	switch typed := raw.(type) {
	case string:
		return typed
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			if sanitized := sanitizeInputItem(item, state); sanitized != nil {
				out = append(out, sanitized)
			}
		}
		if len(out) == 0 {
			// Never send empty array after filtering — keep a noop user turn.
			return []any{map[string]any{"role": "user", "content": ""}}
		}
		return out
	default:
		return raw
	}
}

func sanitizeInputItem(raw any, state *ToolCompatibility) any {
	item, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	typeName := strings.ToLower(strings.TrimSpace(stringValue(item["type"])))
	switch typeName {
	case "", "message":
		return sanitizeMessageItem(item)
	case "function_call", "tool_call":
		return sanitizeFunctionCallItem(item, state)
	case "function_call_output", "tool_result", "tool_call_output":
		return sanitizeFunctionCallOutputItem(item, state)
	case "tool_search_call":
		return toolSearchCallToFunctionCall(item, state)
	case "tool_search_output":
		return toolSearchOutputToFunctionCallOutput(item, state)
	case "additional_tools":
		return additionalToolsToHistoryMessage(item, state)
	case "agent_message":
		return agentMessageToHistory(item, state)
	case "mcp_tool_call_output":
		return mcpToolCallOutputToHistory(item)
	case "compaction_trigger":
		return compactionTriggerToHistory(state)
	case "reasoning":
		return sanitizeReasoningItem(item)
	case "compaction", "compact_result", "compaction_result":
		// OpenAI compact session blobs are not portable to Grok
		// ("Could not decode the compaction blob").
		return historyNote("assistant", "compaction", item)
	case "local_shell_call", "shell_call":
		return localShellCallToNativeOrFunction(item, state)
	case "local_shell_call_output", "shell_call_output":
		// When tools upgraded to native shell, keep shell_call_output shape.
		if state != nil && (state.legacyLocalShell || state.nativeShell) {
			return map[string]any{
				"type":    "shell_call_output",
				"call_id": state.takeCallID(firstNonEmptyString(item["call_id"], item["id"])),
				"output":  firstNonNil(item["output"], item["result"]),
			}
		}
		return map[string]any{
			"type":    "function_call_output",
			"call_id": state.takeCallID(firstNonEmptyString(item["call_id"], item["id"])),
			"output":  stringifyOutput(firstNonNil(item["output"], item["result"])),
		}
	case "custom_tool_call":
		return customToolCallToFunctionCall(item, state)
	case "custom_tool_call_output":
		return map[string]any{
			"type":    "function_call_output",
			"call_id": state.takeCallID(firstNonEmptyString(item["call_id"], item["id"])),
			"output":  stringifyOutput(firstNonNil(item["output"], item["result"])),
		}
	case "web_search_call":
		return builtinCallToFunctionCall(item, "web_search", map[string]any{
			"query":  firstNonEmptyString(item["query"], digString(item, "action", "query")),
			"status": stringValue(item["status"]),
		}, state)
	case "web_search_call_output":
		return builtinOutputToFunctionCallOutput(item, state)
	case "file_search_call":
		return builtinCallToFunctionCall(item, "file_search", map[string]any{
			"queries": firstNonNil(item["queries"], digAny(item, "action", "queries")),
			"status":  stringValue(item["status"]),
		}, state)
	case "file_search_call_output":
		return builtinOutputToFunctionCallOutput(item, state)
	case "code_interpreter_call":
		return builtinCallToFunctionCall(item, "code_interpreter", map[string]any{
			"code":   firstNonEmptyString(item["code"], digString(item, "action", "code")),
			"status": stringValue(item["status"]),
		}, state)
	case "code_interpreter_call_output":
		return builtinOutputToFunctionCallOutput(item, state)
	case "image_generation_call":
		return builtinCallToFunctionCall(item, "image_generation", map[string]any{
			"prompt": firstNonEmptyString(item["prompt"], digString(item, "action", "prompt")),
			"status": stringValue(item["status"]),
		}, state)
	case "image_generation_call_output":
		return builtinOutputToFunctionCallOutput(item, state)
	case "computer_call":
		return builtinCallToFunctionCall(item, "computer", map[string]any{
			"action": firstNonNil(item["action"], item["pending_safety_checks"]),
			"status": stringValue(item["status"]),
		}, state)
	case "computer_call_output":
		return builtinOutputToFunctionCallOutput(item, state)
	case "mcp_call":
		return builtinCallToFunctionCall(item, firstNonEmptyString(item["name"], "mcp_call"), map[string]any{
			"server":    firstNonEmptyString(item["server_label"], item["server"]),
			"arguments": firstNonNil(item["arguments"], item["input"]),
			"status":    stringValue(item["status"]),
			"tool":      stringValue(item["name"]),
			"error":     item["error"],
			"output":    item["output"],
		}, state)
	case "mcp_list_tools":
		return builtinCallToFunctionCall(item, "mcp_list_tools", map[string]any{
			"server": stringValue(firstNonEmptyString(item["server_label"], item["server"])),
			"tools":  firstNonNil(item["tools"], item["output"]),
		}, state)
	case "mcp_approval_request", "mcp_approval_response":
		return historyNote("assistant", typeName, item)
	case "apply_patch_call":
		return applyPatchCallToFunctionCall(item, state)
	case "apply_patch_call_output":
		return applyPatchOutputToFunctionCallOutput(item, state)
	case "item_reference":
		// Cannot resolve without a response store; keep a visible breadcrumb.
		id := firstNonEmptyString(item["id"], item["item_id"], digString(item, "item", "id"))
		return map[string]any{
			"role":    "assistant",
			"content": "[prior item_reference id=" + id + " — expanded content unavailable on this backend]",
		}
	default:
		if typeName != "" {
			// Unknown typed item: preserve as a short history note, never 422.
			return historyNote("assistant", typeName, item)
		}
		return sanitizeMessageItem(item)
	}
}

// sanitizeReasoningItem keeps multi-turn reasoning structure but drops
// encrypted_content that Grok cannot decrypt (OpenAI/Codex foreign blobs).
// Summary text is preserved so the model still sees prior plan signal.
func sanitizeReasoningItem(item map[string]any) map[string]any {
	out := map[string]any{"type": "reasoning"}
	if id := strings.TrimSpace(stringValue(item["id"])); id != "" {
		out["id"] = id
	}
	if status := strings.TrimSpace(stringValue(item["status"])); status != "" {
		out["status"] = status
	}
	if summary, ok := item["summary"]; ok && summary != nil {
		out["summary"] = summary
	} else {
		out["summary"] = []any{}
	}
	// Do not forward encrypted_content: foreign values 422 with
	// "Could not decrypt the provided encrypted_content".
	// Anthropic thinking signatures that were mapped into this field also
	// cannot be decrypted by Grok — summary/note is the portable fallback.
	return out
}

// builtinCallToFunctionCall maps OpenAI built-in call items onto function_call.
func builtinCallToFunctionCall(item map[string]any, name string, args map[string]any, state *ToolCompatibility) map[string]any {
	callID := state.ensureCallID(firstNonEmptyString(item["call_id"], item["id"]))
	// Drop empty string values for cleaner arguments.
	clean := map[string]any{}
	for k, v := range args {
		if v == nil {
			continue
		}
		if s, ok := v.(string); ok && strings.TrimSpace(s) == "" {
			continue
		}
		clean[k] = v
	}
	if len(clean) == 0 {
		// Still emit a call so multi-turn pairing with outputs stays coherent.
		clean["status"] = firstNonEmptyString(item["status"], "completed")
	}
	encoded, err := json.Marshal(clean)
	if err != nil {
		encoded = []byte("{}")
	}
	out := map[string]any{
		"type":      "function_call",
		"call_id":   callID,
		"name":      name,
		"arguments": string(encoded),
	}
	if status := strings.TrimSpace(stringValue(item["status"])); status != "" {
		out["status"] = status
	}
	return out
}

func builtinOutputToFunctionCallOutput(item map[string]any, state *ToolCompatibility) map[string]any {
	callID := state.takeCallID(firstNonEmptyString(item["call_id"], item["id"]))
	output := firstNonNil(item["output"], item["result"], item["content"], item["text"])
	return map[string]any{
		"type":    "function_call_output",
		"call_id": callID,
		"output":  stringifyOutput(output),
	}
}

func historyNote(role, typeName string, item map[string]any) map[string]any {
	// Compact JSON of a few useful fields so the model retains signal.
	// Never include large opaque blobs (compaction / encrypted_content).
	snippet := map[string]any{}
	for _, key := range []string{"id", "call_id", "name", "status", "query", "server_label", "action", "output", "error"} {
		if v, ok := item[key]; ok && v != nil {
			snippet[key] = v
		}
	}
	body := stringifyOutput(snippet)
	if len(body) > 1500 {
		body = body[:1500] + "…"
	}
	return map[string]any{
		"role":    role,
		"content": "[converted " + typeName + "] " + body,
	}
}

func digString(item map[string]any, keys ...string) string {
	cur := any(item)
	for _, key := range keys {
		obj, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = obj[key]
	}
	return stringValue(cur)
}

func digAny(item map[string]any, keys ...string) any {
	cur := any(item)
	for _, key := range keys {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = obj[key]
	}
	return cur
}

func sanitizeMessageItem(item map[string]any) map[string]any {
	out := map[string]any{}
	if typ := strings.TrimSpace(stringValue(item["type"])); typ != "" {
		out["type"] = typ
	}
	role := strings.ToLower(strings.TrimSpace(stringValue(item["role"])))
	if role == "" {
		role = "user"
	}
	// Grok ModelInput accepts system/user/assistant — not OpenAI "developer".
	// Codex posts developer system prompts; map to system (xAI docs use system).
	if role == "developer" {
		role = "system"
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

func sanitizeFunctionCallItem(item map[string]any, state *ToolCompatibility) map[string]any {
	name := firstNonEmptyString(item["name"], item["tool_name"])
	if name == "" {
		if fn, ok := item["function"].(map[string]any); ok {
			name = firstNonEmptyString(fn["name"])
		}
	}
	if name == "" {
		return nil
	}
	namespace := firstNonEmptyString(item["namespace"])
	if state != nil && namespace != "" {
		name = state.registerFunction(namespace, name)
	} else if state != nil {
		// If the client already used an upstream alias, keep it; otherwise leave bare name.
		if alias, ok := state.lookupAlias("", name); ok {
			name = alias
		}
	}
	callID := state.ensureCallID(firstNonEmptyString(item["call_id"], item["id"]))
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

func toolSearchCallToFunctionCall(item map[string]any, state *ToolCompatibility) map[string]any {
	callID := state.ensureCallID(firstNonEmptyString(item["call_id"], item["id"]))
	arguments, err := encodeFunctionArguments(firstNonNil(item["arguments"], item["input"]))
	if err != nil || arguments == "" {
		arguments = "{}"
	}
	return map[string]any{
		"type":      "function_call",
		"call_id":   callID,
		"name":      state.registerToolSearch("tool_search"),
		"arguments": arguments,
	}
}

func toolSearchOutputToFunctionCallOutput(item map[string]any, state *ToolCompatibility) map[string]any {
	execution := strings.ToLower(strings.TrimSpace(stringValue(item["execution"])))
	if execution != "" && execution != "client" {
		// Soft-degrade: still pair the turn so multi-turn continues.
		state.addWarning("tool_search_output_execution_forced_client")
	}
	callID := state.takeCallID(firstNonEmptyString(item["call_id"], item["id"]))
	tools, _ := item["tools"].([]any)
	if len(tools) > 0 {
		_ = state.loadToolsFromHistory(tools, "input.tool_search_output.tools")
		state.AppendVisibleTools(tools)
	}
	return map[string]any{
		"type":    "function_call_output",
		"call_id": callID,
		"output":  fmt.Sprintf("Tool search completed; %d selected tool definitions are now available.", len(tools)),
	}
}

func additionalToolsToHistoryMessage(item map[string]any, state *ToolCompatibility) map[string]any {
	tools, _ := item["tools"].([]any)
	if len(tools) > 0 {
		_ = state.loadToolsFromHistory(tools, "input.additional_tools.tools")
		state.AppendVisibleTools(tools)
		state.addWarning("additional_tools_position_approximated")
	}
	names := make([]string, 0, len(tools))
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name := firstNonEmptyString(tool["name"], tool["server_label"], tool["type"])
		if name != "" {
			names = append(names, name)
		}
	}
	message := "Additional tools become available at this point in the conversation."
	if len(names) > 0 {
		message += "\nTools: " + strings.Join(names, ", ")
	}
	return sanitizeMessageItem(map[string]any{
		"type": "message",
		"role": "developer",
		"content": []any{
			map[string]any{"type": "input_text", "text": message},
		},
	})
}

func sanitizeFunctionCallOutputItem(item map[string]any, state *ToolCompatibility) map[string]any {
	return map[string]any{
		"type":    "function_call_output",
		"call_id": state.takeCallID(firstNonEmptyString(item["call_id"], item["id"])),
		"output":  stringifyOutput(item["output"]),
	}
}

func localShellCallToFunctionCall(item map[string]any, state *ToolCompatibility) map[string]any {
	callID := state.ensureCallID(firstNonEmptyString(item["call_id"], item["id"]))
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

func customToolCallToFunctionCall(item map[string]any, state *ToolCompatibility) map[string]any {
	callID := state.ensureCallID(firstNonEmptyString(item["call_id"], item["id"]))
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
// Mapping:
//   - system → accumulated into instructions (not duplicated in input)
//   - user/assistant text or multimodal → {role, content} (images as input_image)
//   - assistant tool_calls → {type:function_call, call_id, name, arguments}
//   - tool results → {type:function_call_output, call_id, output}
//
// Missing tool_call_id is paired in order with prior assistant tool_calls.
func ChatMessagesToResponsesInput(messages []any) (input []any, instructions string) {
	input = make([]any, 0, len(messages))
	var systemParts []string
	state := newToolCompatibility()

	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(stringValue(msg["role"])))
		content := convertChatMessageContent(msg["content"])

		switch role {
		case "system", "developer":
			if text := strings.TrimSpace(contentAsPlainText(content)); text != "" {
				systemParts = append(systemParts, text)
			}
		case "assistant":
			hasContent := !isEmptyChatContent(content)
			if hasContent {
				input = append(input, map[string]any{
					"role":    "assistant",
					"content": content,
				})
			}
			if calls, ok := msg["tool_calls"].([]any); ok {
				for _, rawCall := range calls {
					if item := chatToolCallToFunctionCall(rawCall, state); item != nil {
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
						"call_id":   state.ensureCallID(""),
						"name":      name,
						"arguments": firstNonEmpty(args, "{}"),
					})
				}
			} else if !hasContent {
				// Empty assistant with no tools — still keep a placeholder turn.
				input = append(input, map[string]any{
					"role":    "assistant",
					"content": "",
				})
			}
		case "tool":
			callID := state.takeCallID(firstNonEmptyString(msg["tool_call_id"], msg["id"]))
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": callID,
				"output":  contentAsPlainText(content),
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

func chatToolCallToFunctionCall(raw any, state *ToolCompatibility) map[string]any {
	call, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	function, _ := call["function"].(map[string]any)
	name := firstNonEmptyString(call["name"], function["name"])
	if name == "" {
		return nil
	}
	callID := state.ensureCallID(firstNonEmptyString(call["id"], call["call_id"], function["id"]))
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

// convertChatMessageContent keeps multimodal structure (text + images).
// Plain strings stay strings; arrays become Responses content parts.
func convertChatMessageContent(content any) any {
	switch typed := content.(type) {
	case string:
		return typed
	case []any:
		parts := convertChatContentParts(typed)
		if len(parts) == 0 {
			return ""
		}
		// Text-only arrays collapse to a string (legacy chat clients / smaller body).
		// Images force structured content so vision is not dropped.
		hasImage := false
		for _, raw := range parts {
			if part, ok := raw.(map[string]any); ok &&
				strings.EqualFold(stringValue(part["type"]), "input_image") {
				hasImage = true
				break
			}
		}
		if !hasImage {
			return contentAsPlainText(parts)
		}
		return parts
	case nil:
		return ""
	default:
		return fmt.Sprint(typed)
	}
}

func convertChatContentParts(parts []any) []any {
	result := make([]any, 0, len(parts))
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(stringValue(part["type"]))) {
		case "text", "input_text", "output_text":
			if text := stringValue(part["text"]); text != "" {
				result = append(result, map[string]any{"type": "input_text", "text": text})
			}
		case "image_url", "input_image":
			if url := parseChatImageURL(part); url != "" {
				result = append(result, map[string]any{"type": "input_image", "image_url": url})
			}
		default:
			if text := stringValue(part["text"]); text != "" {
				result = append(result, map[string]any{"type": "input_text", "text": text})
			}
		}
	}
	return result
}

func parseChatImageURL(part map[string]any) string {
	switch typed := part["image_url"].(type) {
	case string:
		if url := strings.TrimSpace(typed); url != "" {
			return url
		}
	case map[string]any:
		if url := strings.TrimSpace(stringValue(typed["url"])); url != "" {
			return url
		}
	}
	if url := strings.TrimSpace(stringValue(part["url"])); url != "" {
		return url
	}
	return ""
}

func contentAsPlainText(content any) string {
	switch typed := content.(type) {
	case string:
		return typed
	case []any:
		var b strings.Builder
		for _, raw := range typed {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if text := stringValue(part["text"]); text != "" {
				b.WriteString(text)
			}
		}
		return b.String()
	case nil:
		return ""
	default:
		return fmt.Sprint(typed)
	}
}

func isEmptyChatContent(content any) bool {
	switch typed := content.(type) {
	case nil:
		return true
	case string:
		return typed == ""
	case []any:
		return len(typed) == 0
	default:
		return false
	}
}
