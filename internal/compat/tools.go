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

// Grok /responses accepts only these built-in tool type variants (plus function).
// Codex also emits local_shell / tool_search / custom / web_search extras that 400/422.
var grokBuiltinToolTypes = map[string]struct{}{
	"web_search":         {},
	"x_search":           {},
	"collections_search": {},
	"file_search":        {},
	"code_execution":     {},
	"code_interpreter":   {},
	"mcp":                {},
	"shell":              {},
}

// tool fields Grok rejects on web_search (Codex OpenAI Responses shape).
var webSearchRejectedFields = []string{
	"external_web_access",
	"indexed_web_access",
	"filters",
	"user_location",
	"search_context_size",
	"search_content_types",
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

// ToolNormalizeResult is the sanitized tools list plus optional search policy
// inferred from Codex web_search.external_web_access.
type ToolNormalizeResult struct {
	Tools []any
	// BackendSearch is set when a Codex web_search tool carried external_web_access.
	BackendSearch *bool
}

// NormalizeResponsesTools adapts client tool lists for strict Grok Responses backends.
//
// Policy:
//  1. Expand Codex {type:"namespace", tools:[function...]} into flat function tools
//  2. Collapse Codex web_search extras (external_web_access, search_context_size, …)
//     to bare {"type":"web_search"} — otherwise Grok returns 400 Argument not supported
//  3. Drop unknown OpenAI-only types (local_shell, tool_search, custom, …)
//  4. Drop shell without required environment
//  5. Soft-cap count when maxTools > 0
func NormalizeResponsesTools(raw any, maxTools int) []any {
	return NormalizeResponsesToolsDetailed(raw, maxTools).Tools
}

// NormalizeResponsesToolsDetailed is like NormalizeResponsesTools but also returns
// backend_search hints inferred from nested Codex tool fields.
func NormalizeResponsesToolsDetailed(raw any, maxTools int) ToolNormalizeResult {
	tools, ok := raw.([]any)
	if !ok || len(tools) == 0 {
		return ToolNormalizeResult{}
	}
	out := make([]any, 0, len(tools))
	seen := map[string]struct{}{}
	var backendSearch *bool

	appendTool := func(tool map[string]any) {
		if maxTools > 0 && len(out) >= maxTools {
			return
		}
		name := firstNonEmptyString(tool["name"])
		typeName := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
		if name != "" || typeName == "function" {
			key := typeName + "\x00" + name
			if name == "" {
				// built-ins without name: allow one of each type
				key = typeName + "\x00"
			}
			if _, exists := seen[key]; exists {
				return
			}
			seen[key] = struct{}{}
		} else if typeName != "" {
			key := typeName + "\x00"
			if _, exists := seen[key]; exists {
				return
			}
			seen[key] = struct{}{}
		}
		out = append(out, tool)
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
				// Nested namespaces only carry function tools in Codex.
				definition, valid := parseToolDefinition(childTool)
				if !valid || definition.Type != "function" {
					continue
				}
				appendTool(definition.responseTool())
			}
		case "web_search", "web_search_preview", "websearch":
			// Codex: {type:web_search, external_web_access:bool, search_context_size, …}
			// Grok only accepts the bare variant (and uses top-level backend_search).
			if rawAccess, ok := tool["external_web_access"]; ok {
				value := truthy(rawAccess)
				backendSearch = &value
			}
			appendTool(map[string]any{"type": "web_search"})
		case "local_shell":
			// OpenAI built-in; Grok has no local_shell variant. Drop — Codex still
			// sends shell_command/apply_patch as function tools for agent work.
			continue
		case "tool_search", "custom":
			// OpenAI/Codex-only; Grok rejects unknown variants.
			continue
		case "shell":
			// Grok shell requires environment; incomplete objects 422.
			if _, hasEnv := tool["environment"]; !hasEnv {
				continue
			}
			clean := map[string]any{"type": "shell", "environment": tool["environment"]}
			if name := firstNonEmptyString(tool["name"]); name != "" {
				clean["name"] = name
			}
			appendTool(clean)
		case "function", "":
			definition, valid := parseToolDefinition(tool)
			if valid && definition.Type == "function" {
				appendTool(definition.responseTool())
			}
		default:
			if _, allowed := grokBuiltinToolTypes[typeName]; !allowed {
				continue
			}
			// Keep type (+ name/description when present); drop Codex/OpenAI extras.
			clean := map[string]any{"type": typeName}
			if name := firstNonEmptyString(tool["name"]); name != "" {
				clean["name"] = name
			}
			if desc := firstNonEmptyString(tool["description"]); desc != "" {
				clean["description"] = desc
			}
			// mcp / file_search may need server_label / vector_store_ids — pass through
			// only a small allowlist of known-safe keys.
			for _, key := range []string{
				"server_label", "server_url", "server_description",
				"vector_store_ids", "max_num_results", "filters",
				"container", "parameters",
			} {
				if value, ok := tool[key]; ok {
					clean[key] = value
				}
			}
			// Never forward external_web_access on any builtin.
			for _, key := range webSearchRejectedFields {
				delete(clean, key)
			}
			appendTool(clean)
		}
	}
	if len(out) == 0 {
		return ToolNormalizeResult{BackendSearch: backendSearch}
	}
	return ToolNormalizeResult{Tools: out, BackendSearch: backendSearch}
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
