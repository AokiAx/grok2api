package compat

import "strings"

type toolDefinition struct {
	Type        string
	Name        string
	Description string
	Parameters  any
	Strict      any
	Raw         map[string]any
}

func parseToolDefinition(tool map[string]any) (toolDefinition, bool) {
	typeName := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
	if typeName == "" {
		if _, hasFunction := tool["function"]; hasFunction || strings.TrimSpace(stringValue(tool["name"])) != "" {
			typeName = "function"
		}
	}
	if typeName != "function" {
		if typeName == "" {
			return toolDefinition{}, false
		}
		return toolDefinition{Type: typeName, Raw: cloneMap(tool)}, true
	}

	function, _ := tool["function"].(map[string]any)
	name := firstNonEmptyString(tool["name"], function["name"])
	if name == "" {
		return toolDefinition{}, false
	}
	parameters := firstNonNil(
		tool["parameters"],
		tool["input_schema"],
		function["parameters"],
		function["input_schema"],
	)
	if parameters == nil {
		parameters = emptyObjectSchema()
	}
	return toolDefinition{
		Type:        "function",
		Name:        name,
		Description: firstNonEmptyString(tool["description"], function["description"]),
		Parameters:  parameters,
		Strict:      firstNonNil(tool["strict"], function["strict"]),
	}, true
}

func (definition toolDefinition) responseTool() map[string]any {
	if definition.Type != "function" {
		return cloneMap(definition.Raw)
	}
	tool := map[string]any{
		"type":       "function",
		"name":       definition.Name,
		"parameters": definition.Parameters,
	}
	if definition.Description != "" {
		tool["description"] = definition.Description
	}
	if definition.Strict != nil {
		tool["strict"] = definition.Strict
	}
	return tool
}

func cloneMap(input map[string]any) map[string]any {
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func firstNonEmptyString(values ...any) string {
	for _, value := range values {
		if result := strings.TrimSpace(stringValue(value)); result != "" {
			return result
		}
	}
	return ""
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func emptyObjectSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

// NormalizeResponsesToolChoice converts Chat Completions function selection
// into the flat shape required by the Responses API.
func NormalizeResponsesToolChoice(raw any) any {
	choice, ok := raw.(map[string]any)
	if !ok {
		return raw
	}
	if !strings.EqualFold(strings.TrimSpace(stringValue(choice["type"])), "function") {
		return cloneMap(choice)
	}
	function, _ := choice["function"].(map[string]any)
	name := firstNonEmptyString(choice["name"], function["name"])
	if name == "" {
		return cloneMap(choice)
	}
	return map[string]any{
		"type": "function",
		"name": name,
	}
}

// NormalizeResponsesTools adapts client tool lists for strict Responses backends.
//
// Policy:
//  1. Expand Codex {type:"namespace", tools:[function...]} into flat function tools
//  2. Keep function/web_search/mcp/shell and other known types
//  3. Soft-cap count when maxTools > 0
//
// This preserves tool capability while avoiding 422 "unknown variant namespace".
func NormalizeResponsesTools(raw any, maxTools int) []any {
	tools, ok := raw.([]any)
	if !ok || len(tools) == 0 {
		return nil
	}
	out := make([]any, 0, len(tools))
	seen := map[string]struct{}{}
	appendTool := func(definition toolDefinition) {
		if maxTools > 0 && len(out) >= maxTools {
			return
		}
		if definition.Name != "" {
			key := definition.Type + "\x00" + definition.Name
			if _, exists := seen[key]; exists {
				return
			}
			seen[key] = struct{}{}
		}
		out = append(out, definition.responseTool())
	}

	for _, item := range tools {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		typeName := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
		switch typeName {
		case "namespace":
			nested, _ := tool["tools"].([]any)
			for _, child := range nested {
				childTool, ok := child.(map[string]any)
				if !ok {
					continue
				}
				definition, valid := parseToolDefinition(childTool)
				if !valid || definition.Type != "function" {
					continue
				}
				appendTool(definition)
			}
		default:
			definition, valid := parseToolDefinition(tool)
			if valid {
				appendTool(definition)
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func hasWebSearchTool(raw any) bool {
	tools, ok := raw.([]any)
	if !ok {
		return false
	}
	for _, item := range tools {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(stringValue(tool["type"]))) {
		case "web_search", "web_search_preview", "websearch":
			return true
		}
	}
	return false
}
