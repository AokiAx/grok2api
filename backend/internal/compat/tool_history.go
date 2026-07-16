package compat

import (
	"encoding/json"
	"strings"
)

// History-only item rewrites (agent messages, MCP orphan outputs, compaction markers).

func agentMessageToHistory(item map[string]any, state *ToolCompatibility) map[string]any {
	content, visible := textInputContent(item["content"])
	if !visible {
		if state != nil {
			state.addWarning("opaque_agent_message_redacted")
		}
		return sanitizeMessageItem(map[string]any{
			"type": "message",
			"role": "developer",
			"content": []any{
				map[string]any{
					"type": "input_text",
					"text": "An encrypted inter-agent message occurred here but is not portable to this backend.",
				},
			},
		})
	}
	author := firstNonEmptyString(item["author"], "agent")
	recipient := firstNonEmptyString(item["recipient"], "recipient")
	return sanitizeMessageItem(map[string]any{
		"type": "message",
		"role": "developer",
		"content": []any{
			map[string]any{
				"type": "input_text",
				"text": "Agent message (" + author + " -> " + recipient + "):\n" + content,
			},
		},
	})
}

func mcpToolCallOutputToHistory(item map[string]any) map[string]any {
	callID := firstNonEmptyString(item["call_id"], item["id"], "unknown")
	output := stringifyOutput(firstNonNil(item["output"], item["result"], item["content"]))
	return sanitizeMessageItem(map[string]any{
		"type": "message",
		"role": "developer",
		"content": []any{
			map[string]any{
				"type": "input_text",
				"text": "MCP tool output for call " + callID + ": " + output,
			},
		},
	})
}

func compactionTriggerToHistory(state *ToolCompatibility) map[string]any {
	if state != nil {
		state.addWarning("compaction_boundary_preserved")
	}
	return sanitizeMessageItem(map[string]any{
		"type": "message",
		"role": "developer",
		"content": []any{
			map[string]any{
				"type": "input_text",
				"text": "Context compaction boundary reached.",
			},
		},
	})
}

func textInputContent(raw any) (string, bool) {
	if text, ok := raw.(string); ok {
		return text, true
	}
	items, ok := raw.([]any)
	if !ok {
		return "", false
	}
	parts := make([]string, 0, len(items))
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			return "", false
		}
		switch strings.ToLower(strings.TrimSpace(stringValue(item["type"]))) {
		case "input_text", "output_text", "text":
			parts = append(parts, stringValue(item["text"]))
		case "encrypted_text", "encrypted_content":
			return "", false
		default:
			// Unknown structured blocks are not treated as portable text.
			if _, hasEnc := item["encrypted_content"]; hasEnc {
				return "", false
			}
			if text := stringValue(item["text"]); text != "" {
				parts = append(parts, text)
				continue
			}
			return "", false
		}
	}
	return strings.Join(parts, "\n"), true
}

func encodeFunctionArguments(value any) (string, error) {
	if text, ok := value.(string); ok {
		return text, nil
	}
	if value == nil {
		return "{}", nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
