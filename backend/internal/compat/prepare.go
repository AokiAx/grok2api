package compat

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ModelHints carries catalog-derived policy for preparing upstream Responses requests.
type ModelHints struct {
	// SupportsBackendSearch means the model can honor native search. It does NOT
	// auto-inject search tools (search is opt-in only).
	SupportsBackendSearch bool
	// InjectDefaultSearchTools prepends bare web_search + x_search when true and
	// the client has not disabled search. Default false — clients must request search.
	InjectDefaultSearchTools bool
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
	// Always sanitize for Grok ModelInput (drops local_shell_call, item_reference, …).
	if _, hasInput := input["input"]; hasInput {
		input["input"] = SanitizeResponsesInput(input["input"])
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
	toolResult := NormalizeResponsesToolsDetailed(input["tools"], MaxUpstreamTools)
	if toolResult.Err != nil {
		return nil, "", false, toolResult.Err
	}
	if len(toolResult.Tools) > 0 {
		input["tools"] = toolResult.Tools
	} else {
		delete(input, "tools")
	}
	// Codex nests live-search policy on tools[].external_web_access; Grok uses backend_search.
	if toolResult.BackendSearch != nil {
		if _, exists := input["backend_search"]; !exists {
			input["backend_search"] = *toolResult.BackendSearch
		}
	}
	if toolResult.WebSearchDisabled {
		if raw, ok := input["backend_search"]; !ok || truthy(raw) {
			input["backend_search"] = false
		}
		input["tools"] = StripSearchTools(input["tools"])
		if tools, ok := input["tools"].([]any); !ok || len(tools) == 0 {
			delete(input, "tools")
		}
	}
	if choice, exists := input["tool_choice"]; exists && choice != nil {
		tools, _ := input["tools"].([]any)
		var aligned any
		if toolResult.Compat != nil {
			aligned, _ = toolResult.Compat.AlignToolChoice(choice, tools)
		} else {
			aligned, _ = AlignResponsesToolChoice(choice, tools, toolResult.WebSearchDisabled)
		}
		if aligned == nil {
			delete(input, "tool_choice")
		} else {
			input["tool_choice"] = aligned
		}
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
	_ = promoteResponseFormat(input)
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
// Uses a direct Anthropic → Responses path so thinking signatures, server web_search,
// and image blocks are preserved (no Chat Completions intermediate hop).
func PrepareResponsesFromAnthropic(payload []byte, defaultModel string) ([]byte, string, bool, error) {
	return AnthropicToResponses(payload, defaultModel)
}

// PrepareResponsesFromAnthropicWithOptions is PrepareResponsesFromAnthropic with
// session sticky (prompt_cache_key / conv id).
func PrepareResponsesFromAnthropicWithOptions(payload []byte, opts AnthropicToResponsesOptions) ([]byte, string, bool, error) {
	return AnthropicToResponsesWithOptions(payload, opts)
}

// FinalizeResponsesUpstream applies catalog policy and hard invariants required
// before calling the Grok /responses endpoint:
//  1. strip unknown fields + normalize tools/input once (avoid 422)
//  2. align backend_search with client search signals (never force-on by model alone)
//  3. optionally inject web_search + x_search when InjectDefaultSearchTools
//  4. force stream:true so the gateway can always read SSE (non-stream clients
//     are aggregated after the fact)
func FinalizeResponsesUpstream(payload []byte, hints ModelHints) ([]byte, error) {
	body, _, _, err := FinalizeResponsesUpstreamDetailed(payload, hints)
	return body, err
}

// FinalizeResponsesUpstreamDetailed is FinalizeResponsesUpstream plus compatibility
// warning codes and request-scoped tool rewrite state for response-side restore.
func FinalizeResponsesUpstreamDetailed(payload []byte, hints ModelHints) ([]byte, []string, *ToolCompatibility, error) {
	body, warnings, toolCompat, err := SanitizeResponsesWithWarnings(payload)
	if err != nil {
		return nil, nil, nil, err
	}
	// Align backend_search with existing client search signals only — no force-true.
	if ensured, ensureErr := AlignBackendSearch(body); ensureErr == nil {
		body = ensured
	}
	// Opt-in default search tools (off by default).
	if hints.InjectDefaultSearchTools {
		if withTools, toolErr := EnsureDefaultSearchTools(body, true); toolErr == nil {
			body = withTools
			// Re-sanitize after inject so extras/collisions cannot leak.
			if stripped, more, moreCompat, stripErr := SanitizeResponsesWithWarnings(body); stripErr == nil {
				body = stripped
				warnings = mergeWarningCodes(warnings, more)
				// Prefer the post-inject compat (has full alias table).
				if moreCompat != nil {
					toolCompat = moreCompat
				}
			}
		}
	}
	forced, err := SetJSONStreamFlag(body, true)
	if err != nil {
		return nil, warnings, toolCompat, err
	}
	return forced, warnings, toolCompat, nil
}

func mergeWarningCodes(a, b []string) []string {
	if len(b) == 0 {
		return a
	}
	out := append([]string(nil), a...)
	for _, code := range b {
		found := false
		for _, existing := range out {
			if existing == code {
				found = true
				break
			}
		}
		if !found {
			out = append(out, code)
		}
	}
	return out
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
