package compat

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Shared helpers used by Anthropic ↔ Responses conversion and other compat paths.
// The old Anthropic↔Chat hop (AnthropicToOpenAI / OpenAIToAnthropic / NewAnthropicStream)
// was removed; Messages always go through AnthropicToResponses / ResponsesToAnthropic*.

func toolResultText(value any) string {
	switch content := value.(type) {
	case nil:
		return ""
	case string:
		return content
	case []any:
		texts := make([]string, 0, len(content))
		for _, raw := range content {
			switch part := raw.(type) {
			case string:
				texts = append(texts, part)
			case map[string]any:
				if part["type"] == nil || part["type"] == "text" {
					texts = append(texts, stringValue(part["text"]))
				}
			}
		}
		return strings.Join(texts, "")
	default:
		encoded, err := json.Marshal(content)
		if err != nil {
			return fmt.Sprint(content)
		}
		return string(encoded)
	}
}

func copyFields(target, source map[string]any, fields ...string) {
	for _, field := range fields {
		if value, ok := source[field]; ok && value != nil {
			target[field] = value
		}
	}
}

func firstValue(values map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := values[key]; ok && value != nil && fmt.Sprint(value) != "" {
			return value
		}
	}
	return nil
}

func defaultObject(value any) any {
	if value == nil {
		return map[string]any{}
	}
	return value
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func randomID(length int) string {
	buffer := make([]byte, (length+1)/2)
	if _, err := rand.Read(buffer); err != nil {
		return strings.Repeat("0", length)
	}
	return hex.EncodeToString(buffer)[:length]
}
