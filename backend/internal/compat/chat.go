package compat

import (
	"encoding/json"
	"fmt"
	"strings"
)

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
		// Keep nested reasoning.effort in sync when the client already sent a map.
		if reasoning, ok := input["reasoning"].(map[string]any); ok {
			reasoning["effort"] = effort
			input["reasoning"] = reasoning
		} else {
			input["reasoning"] = map[string]any{"effort": effort}
		}
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, "", false, fmt.Errorf("encode chat request: %w", err)
	}
	return encoded, model, stream, nil
}

// ChatToResponses converts an OpenAI Chat Completions body into a Responses body.
//
// Tools are kept. Codex-style {type:"namespace"} groups are expanded into nested
// function tools so strict Responses backends do not 422 on unknown variant
// "namespace". Multi-turn tool history is rewritten into Responses input items
// (function_call / function_call_output). System prompts become instructions.
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
		items, instructions := ChatMessagesToResponsesInput(rawMessages)
		output["input"] = items
		if existing := strings.TrimSpace(stringValue(input["instructions"])); existing != "" {
			output["instructions"] = existing
		} else if instructions != "" {
			output["instructions"] = instructions
		}
	}
	if maxTokens := firstNumber(input, "max_tokens", "max_completion_tokens"); maxTokens != nil {
		output["max_output_tokens"] = maxTokens
	}
	copyFields(output, input, "temperature", "top_p")
	for _, key := range []string{"backend_search", "supports_backend_search", "web_search", "include"} {
		if value, ok := input[key]; ok {
			output[key] = value
		}
	}
	if value, ok := input["instructions"]; ok && output["instructions"] == nil {
		output["instructions"] = value
	}
	toolResult := NormalizeResponsesToolsDetailed(input["tools"], MaxUpstreamTools)
	if toolResult.Err != nil {
		return nil, false, toolResult.Err
	}
	if len(toolResult.Tools) > 0 {
		output["tools"] = toolResult.Tools
	}
	if toolResult.BackendSearch != nil {
		if _, exists := output["backend_search"]; !exists {
			output["backend_search"] = *toolResult.BackendSearch
		}
	}
	if choice, ok := input["tool_choice"]; ok && choice != nil {
		tools, _ := output["tools"].([]any)
		var aligned any
		if toolResult.Compat != nil {
			aligned, _ = toolResult.Compat.AlignToolChoice(choice, tools)
		} else {
			aligned, _ = AlignResponsesToolChoice(choice, tools, toolResult.WebSearchDisabled)
		}
		if aligned != nil {
			output["tool_choice"] = aligned
		}
	}
	if _, ok := input["web_search_options"]; ok {
		if _, exists := output["backend_search"]; !exists {
			output["backend_search"] = true
		}
	}
	if hasWebSearchTool(input["tools"]) {
		if _, exists := output["backend_search"]; !exists {
			output["backend_search"] = true
		}
	}
	if effort := extractReasoningEffort(input); effort != "" {
		output["reasoning_effort"] = effort
	}
	// Structured output: Chat response_format → Responses text.format.
	if value, ok := input["text"]; ok {
		output["text"] = value
	}
	if value, ok := input["response_format"]; ok {
		output["response_format"] = value
	}
	_ = promoteResponseFormat(output)

	encoded, err := json.Marshal(output)
	if err != nil {
		return nil, false, fmt.Errorf("encode responses request: %w", err)
	}
	return encoded, stream, nil
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
	return ""
}

// normalizeEffort maps client aliases onto Grok-supported reasoning levels.
// Production Grok reasoning models (incl. grok-4.5) accept low/medium/high only;
// Codex/OpenAI often send xhigh/max/minimal, which upstream rejects as
// "Invalid reasoning effort."
func normalizeEffort(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "none":
		return "none"
	case "minimal":
		return "low"
	case "low", "medium", "high":
		return strings.ToLower(strings.TrimSpace(value))
	case "xhigh", "max":
		// Ceiling is high — xhigh is not accepted on grok-4.5.
		return "high"
	default:
		return strings.TrimSpace(value)
	}
}

func firstNumber(input map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := input[key]; ok && value != nil {
			return value
		}
	}
	return nil
}
