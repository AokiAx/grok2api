package compat

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ModelHints carries catalog-derived policy for preparing upstream Responses requests.
type ModelHints struct {
	// SupportsBackendSearch enables default-on native WebSearch when the client
	// did not already set backend_search / web_search.
	SupportsBackendSearch bool
}

// NormalizeResponsesRequest turns a client Responses (or chat-shaped) body into a
// clean Responses payload: messages→input, max_tokens→max_output_tokens, tools
// and tool_choice normalized for strict Grok backends.
func NormalizeResponsesRequest(payload []byte, defaultModel string) ([]byte, string, bool, error) {
	normalized, model, stream, err := NormalizeChatRequest(payload, defaultModel)
	if err != nil {
		return nil, "", false, err
	}
	var input map[string]any
	if err := json.Unmarshal(normalized, &input); err != nil {
		return nil, "", false, fmt.Errorf("decode responses request: %w", err)
	}
	if _, hasInput := input["input"]; !hasInput {
		if messages, ok := input["messages"].([]any); ok {
			items, instructions := ChatMessagesToResponsesInput(messages)
			input["input"] = items
			if strings.TrimSpace(stringValue(input["instructions"])) == "" && instructions != "" {
				input["instructions"] = instructions
			}
			delete(input, "messages")
		} else if messages, ok := input["messages"]; ok {
			input["input"] = messages
			delete(input, "messages")
		}
	} else if rawMessages, ok := input["input"].([]any); ok {
		// Only rewrite chat-shaped history. Codex/Responses already send
		// input_text / function_call items — re-running chat conversion empties them.
		if looksLikeChatMessages(rawMessages) && !looksLikeResponsesInput(rawMessages) {
			items, instructions := ChatMessagesToResponsesInput(rawMessages)
			input["input"] = items
			if strings.TrimSpace(stringValue(input["instructions"])) == "" && instructions != "" {
				input["instructions"] = instructions
			}
		}
	}
	if maxTokens, ok := input["max_tokens"]; ok {
		if _, exists := input["max_output_tokens"]; !exists {
			input["max_output_tokens"] = maxTokens
		}
		delete(input, "max_tokens")
	}
	if maxCompletion, ok := input["max_completion_tokens"]; ok {
		if _, exists := input["max_output_tokens"]; !exists {
			input["max_output_tokens"] = maxCompletion
		}
		delete(input, "max_completion_tokens")
	}
	if tools := NormalizeResponsesTools(input["tools"], MaxUpstreamTools); len(tools) > 0 {
		input["tools"] = tools
	} else {
		delete(input, "tools")
	}
	if choice, exists := input["tool_choice"]; exists && choice != nil {
		input["tool_choice"] = NormalizeResponsesToolChoice(choice)
	}
	if _, ok := input["web_search_options"]; ok {
		if _, exists := input["backend_search"]; !exists {
			input["backend_search"] = true
		}
		delete(input, "web_search_options")
	}
	if hasWebSearchTool(input["tools"]) {
		if _, exists := input["backend_search"]; !exists {
			input["backend_search"] = true
		}
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, "", false, fmt.Errorf("encode responses request: %w", err)
	}
	return encoded, model, stream, nil
}

// PrepareResponsesFromChat converts a Chat Completions body into a Responses body.
func PrepareResponsesFromChat(payload []byte, defaultModel string) ([]byte, string, bool, error) {
	normalized, model, stream, err := NormalizeChatRequest(payload, defaultModel)
	if err != nil {
		return nil, "", false, err
	}
	responsesBody, _, err := ChatToResponses(normalized)
	if err != nil {
		return nil, "", false, err
	}
	return responsesBody, model, stream, nil
}

// PrepareResponsesFromAnthropic converts an Anthropic Messages body into a Responses body.
func PrepareResponsesFromAnthropic(payload []byte, defaultModel string) ([]byte, string, bool, error) {
	openAIBody, stream, err := AnthropicToOpenAI(payload, defaultModel)
	if err != nil {
		return nil, "", false, err
	}
	responsesBody, model, _, err := PrepareResponsesFromChat(openAIBody, defaultModel)
	if err != nil {
		return nil, "", false, err
	}
	return responsesBody, model, stream, nil
}

// FinalizeResponsesUpstream applies catalog policy and hard invariants required
// before calling the Grok /responses endpoint:
//  1. default backend_search when the model supports it
//  2. strip unknown fields (avoid 422)
//  3. force stream:true so the gateway can always read SSE (non-stream clients
//     are aggregated after the fact)
func FinalizeResponsesUpstream(payload []byte, hints ModelHints) ([]byte, error) {
	body := payload
	if ensured, err := EnsureBackendSearch(body, hints.SupportsBackendSearch); err == nil {
		body = ensured
	}
	if stripped, err := StripUnknownResponsesFields(body); err == nil {
		body = stripped
	} else {
		return nil, err
	}
	forced, err := SetJSONStreamFlag(body, true)
	if err != nil {
		return nil, err
	}
	return forced, nil
}

// ExtractCompletedResponse pulls the final Responses object from an SSE stream
// or returns a bare JSON body unchanged.
func ExtractCompletedResponse(sseOrJSON []byte) []byte {
	text := string(sseOrJSON)
	if !strings.Contains(text, "event:") && json.Valid(sseOrJSON) {
		return sseOrJSON
	}
	var last []byte
	_ = IterateSSEBytes(sseOrJSON, func(event SSEEvent) error {
		if event.Type == "response.completed" {
			if response, ok := event.Payload["response"].(map[string]any); ok {
				if encoded, err := json.Marshal(response); err == nil {
					last = encoded
				}
			}
		}
		return nil
	})
	return last
}

func looksLikeChatMessages(items []any) bool {
	if len(items) == 0 {
		return false
	}
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			return false
		}
		role := strings.TrimSpace(stringValue(object["role"]))
		if role == "" {
			return false
		}
		if typ := strings.TrimSpace(stringValue(object["type"])); typ != "" && typ != "message" {
			if _, hasContent := object["content"]; !hasContent {
				return false
			}
		}
	}
	return true
}

// looksLikeResponsesInput reports Codex/OpenAI Responses native input items.
func looksLikeResponsesInput(items []any) bool {
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(stringValue(object["type"]))) {
		case "function_call", "function_call_output", "message", "input_text", "input_image",
			"item_reference", "reasoning":
			return true
		}
		content, ok := object["content"].([]any)
		if !ok {
			continue
		}
		for _, part := range content {
			block, ok := part.(map[string]any)
			if !ok {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(stringValue(block["type"]))) {
			case "input_text", "input_image", "output_text", "text":
				// input_text is Responses-native; plain "text" alone is ambiguous,
				// but combined with role+array content from Codex still safe to keep.
				if strings.EqualFold(stringValue(block["type"]), "input_text") ||
					strings.EqualFold(stringValue(block["type"]), "input_image") {
					return true
				}
			}
		}
	}
	return false
}
