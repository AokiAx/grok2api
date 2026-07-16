package compat

import (
	"encoding/json"
	"fmt"
	"strings"
)

// AnthropicToResponsesOptions controls request translation.
type AnthropicToResponsesOptions struct {
	DefaultModel string
	// ConvID becomes prompt_cache_key for session sticky / cache continuity.
	ConvID string
}

// AnthropicToResponses converts Anthropic Messages JSON directly into a Grok
// Responses body. This path preserves thinking signatures, server web_search
// tools, and image blocks (no Chat Completions hop).
//
// Returns the Responses body, the model string (client model, defaulted if empty),
// stream flag, and error.
//
// Locally consumed (not forwarded): metadata, top_k, stop_sequences.
// Model id is passed through as-is (no Claude→Grok alias rewriting).
func AnthropicToResponses(payload []byte, defaultModel string) ([]byte, string, bool, error) {
	return AnthropicToResponsesWithOptions(payload, AnthropicToResponsesOptions{
		DefaultModel: defaultModel,
	})
}

// AnthropicToResponsesWithOptions is AnthropicToResponses with session options.
func AnthropicToResponsesWithOptions(payload []byte, opts AnthropicToResponsesOptions) ([]byte, string, bool, error) {
	var input map[string]any
	if err := json.Unmarshal(payload, &input); err != nil {
		return nil, "", false, fmt.Errorf("decode Anthropic request: %w", err)
	}

	model, _ := input["model"].(string)
	model = strings.TrimSpace(model)
	if model == "" {
		model = strings.TrimSpace(opts.DefaultModel)
	}
	stream, _ := input["stream"].(bool)

	out := map[string]any{
		"model":  model,
		"stream": stream,
		"input":  []any{},
	}
	if conv := strings.TrimSpace(opts.ConvID); conv != "" {
		out["prompt_cache_key"] = conv
	}

	if maxTokens := firstNumber(input, "max_tokens"); maxTokens != nil {
		out["max_output_tokens"] = maxTokens
	}
	// Grok reasoning models reject top_k / stop_sequences; Claude Code may send them.
	// metadata is attribution-only and also rejected upstream.
	copyFields(out, input, "temperature", "top_p")

	if system := anthropicSystemText(input["system"]); system != "" {
		out["instructions"] = system
	}

	thinkingEnabled, _ := anthropicThinkingFlags(input["thinking"])
	effort, summary, includeEnc, err := mapAnthropicThinking(input, model)
	if err != nil {
		return nil, "", false, err
	}
	if effort != "" {
		reasoning := map[string]any{"effort": effort}
		if summary != "" {
			reasoning["summary"] = summary
		}
		out["reasoning"] = reasoning
		out["reasoning_effort"] = effort
	}
	if includeEnc {
		out["include"] = []any{"reasoning.encrypted_content"}
	}

	if textFormat := anthropicOutputFormat(input["output_config"]); textFormat != nil {
		out["text"] = map[string]any{"format": textFormat}
	}

	messages, _ := input["messages"].([]any)
	items := make([]any, 0, len(messages)*2)
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		converted, err := anthropicMessageToResponsesItems(msg)
		if err != nil {
			return nil, "", false, err
		}
		items = append(items, converted...)
	}
	out["input"] = items

	hasServerWebSearch := false
	if tools := anthropicToolsToResponses(input["tools"], &hasServerWebSearch); len(tools) > 0 {
		out["tools"] = tools
	}
	if choice := input["tool_choice"]; choice != nil {
		if hasServerWebSearch && anthropicToolChoiceForcesWebSearch(choice) {
			out["tool_choice"] = "auto"
		} else if mapped := anthropicToolChoiceToResponses(choice); mapped != nil {
			out["tool_choice"] = mapped
		}
	}

	// Keep optional search flags if clients set them on Anthropic-shaped bodies.
	for _, key := range []string{"backend_search", "web_search"} {
		if value, ok := input[key]; ok {
			out[key] = value
		}
	}
	if hasServerWebSearch {
		if _, exists := out["backend_search"]; !exists {
			out["backend_search"] = true
		}
	}

	// Re-sanitize input items for Grok ModelInput safety.
	out["input"] = SanitizeResponsesInput(out["input"])

	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, "", false, fmt.Errorf("encode Responses body: %w", err)
	}
	_ = thinkingEnabled
	return encoded, model, stream, nil
}

// AnthropicThinkingBridge returns whether thinking blocks / signatures should be
// emitted on the response path for this request body.
func AnthropicThinkingBridge(payload []byte) (enabled bool, display string) {
	var input map[string]any
	if json.Unmarshal(payload, &input) != nil {
		return false, ""
	}
	return anthropicThinkingFlags(input["thinking"])
}

func anthropicSystemText(value any) string {
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	case []any:
		parts := make([]string, 0, len(typed))
		for _, raw := range typed {
			block, ok := raw.(map[string]any)
			if !ok {
				if text, ok := raw.(string); ok && strings.TrimSpace(text) != "" {
					parts = append(parts, text)
				}
				continue
			}
			typ := strings.ToLower(strings.TrimSpace(stringValue(block["type"])))
			if typ == "" || typ == "text" {
				if text := strings.TrimSpace(stringValue(block["text"])); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func anthropicMessageToResponsesItems(msg map[string]any) ([]any, error) {
	role := strings.ToLower(strings.TrimSpace(stringValue(msg["role"])))
	if role == "" {
		role = "user"
	}
	content := msg["content"]

	// Plain string content.
	if text, ok := content.(string); ok {
		partType := "input_text"
		if role == "assistant" {
			partType = "output_text"
		}
		return []any{
			map[string]any{
				"type": "message",
				"role": role,
				"content": []any{
					map[string]any{"type": partType, "text": text},
				},
			},
		}, nil
	}

	parts, ok := content.([]any)
	if !ok {
		return []any{
			map[string]any{
				"type":    "message",
				"role":    role,
				"content": []any{map[string]any{"type": "input_text", "text": fmt.Sprint(content)}},
			},
		}, nil
	}

	out := make([]any, 0, len(parts))
	textParts := make([]any, 0)
	flushText := func() {
		if len(textParts) == 0 {
			return
		}
		out = append(out, map[string]any{
			"type":    "message",
			"role":    role,
			"content": textParts,
		})
		textParts = nil
	}

	for _, raw := range parts {
		if text, ok := raw.(string); ok {
			partType := "input_text"
			if role == "assistant" {
				partType = "output_text"
			}
			textParts = append(textParts, map[string]any{"type": partType, "text": text})
			continue
		}
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(stringValue(block["type"]))) {
		case "", "text":
			partType := "input_text"
			if role == "assistant" {
				partType = "output_text"
			}
			textParts = append(textParts, map[string]any{
				"type": partType,
				"text": stringValue(block["text"]),
			})
		case "image":
			if image := anthropicImageToInputImage(block); image != nil {
				textParts = append(textParts, image)
			}
		case "tool_use":
			flushText()
			id := stringValue(block["id"])
			if id == "" {
				id = "call_" + randomID(12)
			}
			args, err := json.Marshal(defaultObject(block["input"]))
			if err != nil {
				args = []byte("{}")
			}
			out = append(out, map[string]any{
				"type":      "function_call",
				"call_id":   id,
				"id":        id,
				"name":      stringValue(block["name"]),
				"arguments": string(args),
			})
		case "tool_result":
			flushText()
			callID := fmt.Sprint(firstValue(block, "tool_use_id", "id"))
			output := toolResultText(block["content"])
			if truthy(block["is_error"]) {
				encoded, _ := json.Marshal(map[string]any{"is_error": true, "content": output})
				output = string(encoded)
			}
			out = append(out, map[string]any{
				"type":    "function_call_output",
				"call_id": callID,
				"output":  output,
			})
		case "thinking":
			signature := strings.TrimSpace(stringValue(block["signature"]))
			if signature == "" {
				continue
			}
			flushText()
			// CPA-style: Anthropic opaque signature is Grok encrypted reasoning.
			out = append(out, map[string]any{
				"type":              "reasoning",
				"summary":           []any{},
				"encrypted_content": signature,
			})
		case "redacted_thinking":
			// Not portable across providers; drop without leaking payload.
			continue
		default:
			// Unknown blocks are dropped to avoid upstream 400s.
			continue
		}
	}
	flushText()
	return out, nil
}

func anthropicImageToInputImage(block map[string]any) map[string]any {
	source, _ := block["source"].(map[string]any)
	if source == nil {
		return nil
	}
	url := strings.TrimSpace(stringValue(source["url"]))
	if url == "" {
		data := strings.TrimSpace(stringValue(source["data"]))
		if data == "" {
			return nil
		}
		mediaType := strings.TrimSpace(stringValue(source["media_type"]))
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		url = "data:" + mediaType + ";base64," + data
	}
	return map[string]any{
		"type":      "input_image",
		"image_url": url,
	}
}

func anthropicToolsToResponses(value any, hasServerWebSearch *bool) []any {
	rawTools, ok := value.([]any)
	if !ok {
		return nil
	}
	tools := make([]any, 0, len(rawTools))
	for _, raw := range rawTools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
		if isAnthropicWebSearchToolType(typ) {
			if hasServerWebSearch != nil {
				*hasServerWebSearch = true
			}
			tools = append(tools, map[string]any{"type": "web_search"})
			continue
		}
		// Already OpenAI-shaped function tool.
		if typ == "function" {
			if fn, ok := tool["function"].(map[string]any); ok {
				name := stringValue(fn["name"])
				if name == "" {
					continue
				}
				item := map[string]any{
					"type":       "function",
					"name":       name,
					"parameters": stripJSONSchemaField(fn["parameters"]),
				}
				if d := stringValue(fn["description"]); d != "" {
					item["description"] = d
				}
				tools = append(tools, item)
				continue
			}
		}
		name := stringValue(tool["name"])
		if name == "" {
			continue
		}
		params := firstValue(tool, "input_schema", "parameters")
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		item := map[string]any{
			"type":       "function",
			"name":       name,
			"parameters": stripJSONSchemaField(params),
		}
		if d := stringValue(tool["description"]); d != "" {
			item["description"] = d
		}
		tools = append(tools, item)
	}
	return tools
}

func isAnthropicWebSearchToolType(toolType string) bool {
	toolType = strings.ToLower(strings.TrimSpace(toolType))
	return toolType == "web_search" || strings.HasPrefix(toolType, "web_search_")
}

func anthropicToolChoiceForcesWebSearch(choice any) bool {
	object, ok := choice.(map[string]any)
	if !ok {
		return false
	}
	if !strings.EqualFold(stringValue(object["type"]), "tool") {
		return false
	}
	name := strings.ToLower(strings.TrimSpace(stringValue(object["name"])))
	return name == "web_search" || strings.HasPrefix(name, "web_search")
}

func anthropicToolChoiceToResponses(choice any) any {
	switch typed := choice.(type) {
	case string:
		switch typed {
		case "any":
			return "required"
		default:
			return typed
		}
	case map[string]any:
		switch strings.ToLower(strings.TrimSpace(stringValue(typed["type"]))) {
		case "auto":
			return "auto"
		case "any":
			return "required"
		case "none":
			return "none"
		case "tool":
			name := stringValue(typed["name"])
			if name == "" {
				return nil
			}
			return map[string]any{"type": "function", "name": name}
		case "function":
			return NormalizeResponsesToolChoice(typed)
		default:
			return typed
		}
	default:
		return nil
	}
}

func stripJSONSchemaField(params any) any {
	object, ok := params.(map[string]any)
	if !ok {
		// Try via JSON round-trip for nested RawMessage-like values.
		raw, err := json.Marshal(params)
		if err != nil {
			return params
		}
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil {
			return params
		}
		object = m
	}
	if _, ok := object["$schema"]; !ok {
		return object
	}
	out := make(map[string]any, len(object))
	for k, v := range object {
		if k == "$schema" {
			continue
		}
		out[k] = v
	}
	return out
}

func anthropicThinkingFlags(raw any) (enabled bool, display string) {
	object, ok := raw.(map[string]any)
	if !ok {
		return false, ""
	}
	typ := strings.ToLower(strings.TrimSpace(stringValue(object["type"])))
	switch typ {
	case "adaptive", "enabled":
		display = strings.ToLower(strings.TrimSpace(stringValue(object["display"])))
		if display == "" {
			display = "summarized"
		}
		return true, display
	default:
		return false, ""
	}
}

func mapAnthropicThinking(input map[string]any, model string) (effort string, summary string, includeEnc bool, err error) {
	thinking, _ := input["thinking"].(map[string]any)
	outputConfig, _ := input["output_config"].(map[string]any)

	requestedEffort := ""
	if outputConfig != nil {
		if e := strings.TrimSpace(stringValue(outputConfig["effort"])); e != "" {
			requestedEffort = normalizeAnthropicEffort(e)
		}
	}

	if thinking == nil {
		if requestedEffort != "" {
			return requestedEffort, "", false, nil
		}
		return "", "", false, nil
	}

	typ := strings.ToLower(strings.TrimSpace(stringValue(thinking["type"])))
	display := strings.ToLower(strings.TrimSpace(stringValue(thinking["display"])))
	maxTokens := intValue(input["max_tokens"])
	hasTools := false
	if tools, ok := input["tools"].([]any); ok && len(tools) > 0 {
		hasTools = true
	}

	switch typ {
	case "adaptive":
		if thinking["budget_tokens"] != nil {
			return "", "", false, fmt.Errorf("thinking.budget_tokens is invalid with type adaptive")
		}
		if requestedEffort == "" {
			requestedEffort = "high"
		}
		includeEnc = true
		if display != "omitted" {
			summary = "auto"
		}
		return grokReasoningEffort(model, requestedEffort), summary, includeEnc, nil
	case "enabled":
		budget := intValue(thinking["budget_tokens"])
		if budget < 1024 {
			return "", "", false, fmt.Errorf("thinking.budget_tokens must be >= 1024 with type enabled")
		}
		if !hasTools && maxTokens > 0 && budget >= maxTokens {
			return "", "", false, fmt.Errorf("thinking.budget_tokens must be less than max_tokens without tools")
		}
		if requestedEffort == "" {
			requestedEffort = effortFromThinkingBudget(budget)
		}
		includeEnc = true
		if display != "omitted" {
			summary = "auto"
		}
		return grokReasoningEffort(model, requestedEffort), summary, includeEnc, nil
	case "disabled":
		if thinking["budget_tokens"] != nil {
			return "", "", false, fmt.Errorf("thinking.budget_tokens is invalid with type disabled")
		}
		if display != "" {
			return "", "", false, fmt.Errorf("thinking.display is invalid with type disabled")
		}
		if requestedEffort != "" {
			if isNonReasoningModel(model) {
				return "", "", false, nil
			}
			return grokReasoningEffort(model, requestedEffort), "", false, nil
		}
		if isNonReasoningModel(model) {
			return "", "", false, nil
		}
		// grok-4.5 cannot fully disable reasoning; low is closest.
		if supportsDisabledReasoning(model) {
			return "none", "", false, nil
		}
		return "low", "", false, nil
	default:
		if typ == "" {
			if requestedEffort != "" {
				return grokReasoningEffort(model, requestedEffort), "", false, nil
			}
			return "", "", false, nil
		}
		return "", "", false, fmt.Errorf("unsupported thinking.type %q", typ)
	}
}

func anthropicOutputFormat(raw any) map[string]any {
	outputConfig, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	format, ok := outputConfig["format"].(map[string]any)
	if !ok {
		return nil
	}
	if !strings.EqualFold(stringValue(format["type"]), "json_schema") {
		return nil
	}
	schema := format["schema"]
	if schema == nil {
		return nil
	}
	return map[string]any{
		"type":   "json_schema",
		"name":   "anthropic_output",
		"schema": schema,
		"strict": true,
	}
}

func normalizeAnthropicEffort(effort string) string {
	// Reuse shared Grok ceiling (xhigh/max → high, minimal → low).
	return normalizeEffort(effort)
}

func effortFromThinkingBudget(budget int) string {
	switch {
	case budget < 4000:
		return "low"
	case budget < 16000:
		return "medium"
	default:
		return "high"
	}
}

func grokReasoningEffort(model, effort string) string {
	if effort == "xhigh" {
		if strings.Contains(strings.ToLower(model), "multi-agent") {
			return "xhigh"
		}
		return "high"
	}
	return effort
}

func isNonReasoningModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return strings.HasPrefix(model, "grok-composer-") || strings.Contains(model, "non-reasoning")
}

func supportsDisabledReasoning(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return model == "grok-4.3" || strings.HasPrefix(model, "grok-4.3-")
}
