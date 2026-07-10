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
		if messages, ok := input["messages"]; ok {
			if rawMessages, ok := messages.([]any); ok {
				input["input"] = sanitizeMessages(rawMessages)
			} else {
				input["input"] = messages
			}
			delete(input, "messages")
		}
	} else if rawMessages, ok := input["input"].([]any); ok {
		// Sanitize chat-like message arrays that clients put under input.
		if looksLikeChatMessages(rawMessages) {
			input["input"] = sanitizeMessages(rawMessages)
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
	// Promote OpenAI-style search signals the same way ChatToResponses does.
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
	for _, block := range strings.Split(text, "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" || data == "[DONE]" {
				continue
			}
			var payload map[string]any
			if json.Unmarshal([]byte(data), &payload) != nil {
				continue
			}
			if payload["type"] == "response.completed" {
				if response, ok := payload["response"].(map[string]any); ok {
					if encoded, err := json.Marshal(response); err == nil {
						last = encoded
					}
				}
			}
		}
	}
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
		// Responses input items often use type=message|input_text without free-form roles
		// in mixed arrays; treat role-bearing objects as chat messages.
		if typ := strings.TrimSpace(stringValue(object["type"])); typ != "" && typ != "message" {
			if _, hasContent := object["content"]; !hasContent {
				return false
			}
		}
	}
	return true
}
