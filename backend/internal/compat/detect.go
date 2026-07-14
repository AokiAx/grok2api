package compat

import (
	"encoding/json"
	"strings"
)

// PayloadKind classifies a client JSON body by protocol shape.
type PayloadKind int

const (
	// KindUnknown cannot be classified.
	KindUnknown PayloadKind = iota
	// KindChat is OpenAI Chat Completions (messages + roles).
	KindChat
	// KindAnthropic is Anthropic Messages API.
	KindAnthropic
	// KindResponses is OpenAI/Grok Responses API (input / instructions).
	KindResponses
)

// DetectPayload sniffs the request body so misrouted clients (e.g. Codex Responses
// payload posted to /v1/messages) still hit the correct conversion path.
func DetectPayload(body []byte) PayloadKind {
	var input map[string]any
	if err := json.Unmarshal(body, &input); err != nil || input == nil {
		return KindUnknown
	}

	_, hasInput := input["input"]
	_, hasMessages := input["messages"]
	_, hasInstructions := input["instructions"]
	_, hasMaxTokens := input["max_tokens"]
	_, hasMaxOutput := input["max_output_tokens"]
	_, hasParallel := input["parallel_tool_calls"]
	_, hasStore := input["store"]
	_, hasPrevious := input["previous_response_id"]

	// Strong Responses signals even when path is wrong.
	if hasInput && !hasMessages {
		return KindResponses
	}
	if hasInput && (hasInstructions || hasMaxOutput || hasParallel || hasStore || hasPrevious) {
		return KindResponses
	}

	if hasMessages {
		if looksAnthropicMessages(input["messages"]) || (hasMaxTokens && !hasInput) {
			// Anthropic always uses max_tokens; Chat may too, so inspect content blocks.
			if looksAnthropicMessages(input["messages"]) {
				return KindAnthropic
			}
			return KindChat
		}
		return KindChat
	}

	// No messages/input: still might be Responses with only instructions (rare).
	if hasInstructions || hasMaxOutput || hasPrevious {
		return KindResponses
	}
	return KindUnknown
}

func looksAnthropicMessages(raw any) bool {
	messages, ok := raw.([]any)
	if !ok || len(messages) == 0 {
		return false
	}
	for _, item := range messages {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		// Anthropic tool_result / tool_use live inside content arrays.
		content, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for _, part := range content {
			block, ok := part.(map[string]any)
			if !ok {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(stringValue(block["type"]))) {
			case "tool_use", "tool_result", "input_text", "input_image", "thinking":
				return true
			}
		}
	}
	// system as top-level string/array is Anthropic-ish when paired with messages.
	return false
}
