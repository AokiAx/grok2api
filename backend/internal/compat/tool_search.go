package compat

import (
	"fmt"
	"strings"
)

const maxToolSearchDescriptionBytes = 16 << 10

// registerClientToolSearch records a client-side tool_search declaration.
// The synthetic function is emitted later via buildClientSearchFunction.
func (c *ToolCompatibility) registerClientToolSearch(tool map[string]any, param string) error {
	if c == nil {
		return newRequestError(param, "invalid_parameter", "tool compatibility state missing")
	}
	execution := strings.ToLower(strings.TrimSpace(stringValue(tool["execution"])))
	// Upstream has no server-side tool_search; only client execution is emulatable.
	if execution == "" || execution == "server" {
		return newRequestError(param+".execution", "unsupported_parameter",
			`server-side tool_search is not supported; use execution: "client"`)
	}
	if execution != "client" {
		return newRequestError(param+".execution", "unsupported_parameter",
			`tool_search.execution only supports "client"`)
	}
	if c.clientSearchActive {
		return newRequestError(param, "invalid_parameter", "only one client tool_search is allowed per request")
	}
	c.clientSearchActive = true
	c.clientSearchTool = cloneMap(tool)
	c.clientSearchParam = param
	c.addWarning("client_tool_search_emulated")
	c.changed = true
	return nil
}

func (c *ToolCompatibility) addDeferredSurface(name, description string) {
	if c == nil {
		return
	}
	surface := describeDeferredTool(name, description)
	if surface == "" {
		return
	}
	for _, existing := range c.deferredSurfaces {
		if existing == surface {
			return
		}
	}
	c.deferredSurfaces = append(c.deferredSurfaces, surface)
	c.changed = true
}

func describeDeferredTool(name, description string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	description = strings.TrimSpace(description)
	if description == "" {
		return name
	}
	if len(description) > 240 {
		description = description[:240]
	}
	return name + ": " + description
}

func namespaceHasDeferredFunctions(children []any) bool {
	for _, raw := range children {
		child, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if strings.ToLower(strings.TrimSpace(stringValue(child["type"]))) != "function" {
			continue
		}
		if deferred, _ := child["defer_loading"].(bool); deferred {
			return true
		}
		// Nested Chat Completions shape.
		if fn, ok := child["function"].(map[string]any); ok {
			if deferred, _ := fn["defer_loading"].(bool); deferred {
				return true
			}
		}
	}
	return false
}

// buildClientSearchFunction builds the synthetic function tool for client tool_search.
func (c *ToolCompatibility) buildClientSearchFunction() (map[string]any, error) {
	if c == nil || !c.clientSearchActive {
		return nil, nil
	}
	alias := c.registerToolSearch("tool_search")
	description := ""
	if c.clientSearchTool != nil {
		description = strings.TrimSpace(stringValue(c.clientSearchTool["description"]))
	}
	if description == "" {
		description = "Search for tools needed to continue the task."
	}
	if len(c.deferredSurfaces) > 0 {
		description += "\nDeferred tool surfaces available to search:\n- " + strings.Join(c.deferredSurfaces, "\n- ")
	}
	if len(description) > maxToolSearchDescriptionBytes {
		description = description[:maxToolSearchDescriptionBytes]
	}
	var parameters any
	if c.clientSearchTool != nil {
		if raw, ok := c.clientSearchTool["parameters"]; ok && raw != nil {
			parameters = raw
		}
	}
	if parameters == nil {
		parameters = map[string]any{
			"type":                 "object",
			"properties":           map[string]any{},
			"additionalProperties": true,
		}
	} else if _, ok := parameters.(map[string]any); !ok {
		return nil, newRequestError(c.clientSearchParam+".parameters", "invalid_parameter", "tool_search.parameters must be an object")
	}
	return syntheticFunctionTool(alias, description, parameters), nil
}

// appendHistoryLoadedTool records tools revealed mid-conversation (tool_search_output).
func (c *ToolCompatibility) appendHistoryLoadedTool(tool map[string]any) {
	if c == nil || tool == nil {
		return
	}
	c.historyLoadedTools = append(c.historyLoadedTools, tool)
	c.changed = true
}

// mergeToolsAppend appends extra tools with type+name dedupe (last wins on collision).
func mergeToolsAppend(base []any, extra []any) []any {
	if len(extra) == 0 {
		return base
	}
	out := make([]any, 0, len(base)+len(extra))
	pos := map[string]int{}
	add := func(raw any) {
		tool, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			return
		}
		kind := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
		name := firstNonEmptyString(tool["name"])
		key := kind + "\x00" + name
		if name == "" {
			key = kind + "\x00"
		}
		if i, exists := pos[key]; exists {
			out[i] = tool
			return
		}
		pos[key] = len(out)
		out = append(out, tool)
	}
	for _, raw := range base {
		add(raw)
	}
	for _, raw := range extra {
		add(raw)
	}
	return out
}

// validateClientToolSearchParallel rejects parallel tool calls when client tool_search is active.
func validateClientToolSearchParallel(parallel any, clientSearch bool) error {
	if !clientSearch || parallel == nil {
		return nil
	}
	if truthy(parallel) {
		return newRequestError("parallel_tool_calls", "unsupported_parameter",
			"client tool_search does not support parallel tool calls")
	}
	return nil
}

// loadToolsFromHistory normalizes tools from tool_search_output / additional_tools (force load).
func (c *ToolCompatibility) loadToolsFromHistory(rawTools []any, param string) error {
	if c == nil {
		return nil
	}
	for index, raw := range rawTools {
		tool, ok := raw.(map[string]any)
		if !ok {
			return newRequestError(fmt.Sprintf("%s[%d]", param, index), "invalid_parameter", "tool must be an object")
		}
		// Nested tool_search is not allowed inside search results.
		if strings.ToLower(strings.TrimSpace(stringValue(tool["type"]))) == "tool_search" {
			return newRequestError(fmt.Sprintf("%s[%d]", param, index), "unsupported_parameter",
				"tool_search_output.tools cannot declare tool_search again")
		}
		converted, err := c.forceNormalizeHistoryTool(tool, fmt.Sprintf("%s[%d]", param, index))
		if err != nil {
			return err
		}
		for _, item := range converted {
			if m, ok := item.(map[string]any); ok {
				c.appendHistoryLoadedTool(m)
			}
		}
	}
	return nil
}

// forceNormalizeHistoryTool normalizes a tool that was dynamically loaded (defer_loading ignored).
func (c *ToolCompatibility) forceNormalizeHistoryTool(tool map[string]any, param string) ([]any, error) {
	// Strip defer_loading so delayed tools become active.
	if _, ok := tool["defer_loading"]; ok {
		tool = cloneMap(tool)
		delete(tool, "defer_loading")
		if fn, ok := tool["function"].(map[string]any); ok {
			fn = cloneMap(fn)
			delete(fn, "defer_loading")
			tool["function"] = fn
		}
	}
	typeName := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
	switch typeName {
	case "function", "":
		definition, valid := parseToolDefinition(tool)
		if !valid || definition.Type != "function" {
			return nil, newRequestError(param, "invalid_parameter", "invalid function tool")
		}
		rt := definition.responseTool()
		// Prefer namespaced alias if namespace field present on freeform history tools.
		if ns := firstNonEmptyString(tool["namespace"]); ns != "" {
			rt["name"] = c.registerFunction(ns, definition.Name)
		}
		return []any{rt}, nil
	case "namespace":
		ns := firstNonEmptyString(tool["name"])
		if ns == "" {
			return nil, newRequestError(param+".name", "invalid_parameter", "namespace.name is required")
		}
		children, _ := tool["tools"].([]any)
		out := make([]any, 0, len(children))
		for i, childRaw := range children {
			child, ok := childRaw.(map[string]any)
			if !ok {
				continue
			}
			def, valid := parseToolDefinition(child)
			if !valid || def.Type != "function" {
				return nil, newRequestError(fmt.Sprintf("%s.tools[%d]", param, i), "invalid_parameter", "namespace only supports function tools")
			}
			rt := def.responseTool()
			rt["name"] = c.registerFunction(ns, def.Name)
			out = append(out, rt)
		}
		return out, nil
	case "custom":
		name := firstNonEmptyString(tool["name"], "custom_tool")
		alias := c.registerCustom("", name)
		params := firstNonNil(tool["parameters"], tool["input_schema"])
		if params == nil {
			params = freeformToolParameters(tool)
		}
		return []any{syntheticFunctionTool(alias, firstNonEmptyString(tool["description"], "Custom tool "+name), params)}, nil
	case "apply_patch":
		return c.normalizeApplyPatchToolDeclaration(tool, param)
	case "mcp":
		// Pass through cleaned mcp without defer_loading.
		clean := cloneMap(tool)
		delete(clean, "defer_loading")
		return []any{clean}, nil
	default:
		// Fall back to a one-shot normalize through the main detailed path for a single tool.
		result := NormalizeResponsesToolsDetailed([]any{tool}, MaxUpstreamTools)
		if result.Err != nil {
			return nil, result.Err
		}
		// Merge aliases from nested normalize into this state.
		if result.Compat != nil {
			for alias, identity := range result.Compat.aliases {
				c.aliases[alias] = identity
				c.identityAliases[identity.key()] = alias
			}
			for _, w := range result.Compat.Warnings() {
				c.addWarning(w)
			}
		}
		return result.Tools, nil
	}
}
