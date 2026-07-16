package compat

import (
	"fmt"
	"strings"
)

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

// searchBuiltinTypes are bare Grok search tools that collide with function tools
// of the same name ("Duplicate tool names: web_search").
var searchBuiltinTypes = map[string]struct{}{
	"web_search": {},
	"x_search":   {},
}

// tool fields Grok rejects on web_search (Codex OpenAI Responses shape).
// Compatibility fields may be stripped with a warning; constraint fields that
// cannot be enforced safely are hard-rejected (see normalizeWebSearchTool).
var webSearchRejectedFields = []string{
	"external_web_access",
	"indexed_web_access",
	"filters",
	"user_location",
	"search_context_size",
	"search_content_types",
	"allowed_domains",
	"max_search_results",
	"safe_search",
}

// webSearchConstraintFields expand client authorization; Grok Build cannot honor them.
var webSearchConstraintFields = map[string]struct{}{
	"filters":         {},
	"allowed_domains": {},
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
	// WebSearchDisabled is true when external_web_access:false removed search tools.
	WebSearchDisabled bool
	// Warnings are compatibility codes for X-Grok2API-Compatibility-Warnings.
	Warnings []string
	// Err is a client-facing validation error (hard reject); tools must not be sent upstream.
	Err error
	// Compat is the request-scoped rewrite state (aliases for response restore).
	// Nil when no tools were present.
	Compat *ToolCompatibility
}

// NormalizeResponsesTools adapts client tool lists for strict Grok Responses backends.
//
// Policy:
//  1. Expand Codex {type:"namespace", tools:[function...]} into flat function tools
//  2. Collapse Codex web_search extras to bare {"type":"web_search"}
//  3. Map OpenAI-only types onto function tools (local_shell→shell_command, custom, …)
//     instead of dropping — aligns with input history sanitization
//  4. Bare shell without environment → shell_command function (avoids 422)
//  5. Soft-cap count when maxTools > 0
//  6. Prefer bare web_search/x_search over function tools with the same name
//  7. external_web_access:false removes web_search (safe capability subset)
//  8. Non-empty filters / allowed_domains hard-reject (cannot enforce)
func NormalizeResponsesTools(raw any, maxTools int) []any {
	return NormalizeResponsesToolsDetailed(raw, maxTools).Tools
}

// NormalizeResponsesToolsDetailed is like NormalizeResponsesTools but also returns
// backend_search hints inferred from nested Codex tool fields.
//
// Single-pass policy: expand → map → hard-validate → collapse names.
// Call AlignResponsesToolChoice afterwards so tool_choice stays coherent.
func NormalizeResponsesToolsDetailed(raw any, maxTools int) ToolNormalizeResult {
	tools, ok := raw.([]any)
	if !ok || len(tools) == 0 {
		return ToolNormalizeResult{}
	}
	compatState := newToolCompatibility()
	// Pre-register client tool_search so defer_loading decisions are correct regardless of list order.
	for index, item := range tools {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if strings.ToLower(strings.TrimSpace(stringValue(tool["type"]))) != "tool_search" {
			continue
		}
		if err := compatState.registerClientToolSearch(tool, fmt.Sprintf("tools[%d]", index)); err != nil {
			return ToolNormalizeResult{Err: err}
		}
	}

	out := make([]any, 0, len(tools))
	seen := map[string]struct{}{}
	var backendSearch *bool
	webSearchDisabled := false
	truncated := false
	clientSearch := compatState.clientSearchActive

	appendTool := func(tool map[string]any) {
		if maxTools > 0 && len(out) >= maxTools {
			if !truncated {
				compatState.addWarning("tools_soft_capped")
				truncated = true
			}
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

	for index, item := range tools {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		typeName := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
		param := fmt.Sprintf("tools[%d]", index)
		switch typeName {
		case "namespace":
			nested, _ := tool["tools"].([]any)
			nsName := firstNonEmptyString(tool["name"])
			if nsName == "" {
				return ToolNormalizeResult{Err: newRequestError(param+".name", "invalid_parameter", "namespace.name is required")}
			}
			compatState.addWarning("namespace_flattened")
			if clientSearch && namespaceHasDeferredFunctions(nested) {
				compatState.addDeferredSurface(nsName, firstNonEmptyString(tool["description"]))
			}
			for _, child := range nested {
				childTool, ok := child.(map[string]any)
				if !ok {
					continue
				}
				definition, valid := parseToolDefinition(childTool)
				if !valid || definition.Type != "function" {
					continue
				}
				deferred := toolHasDeferLoading(childTool)
				if deferred && clientSearch {
					// Keep surface only; do not send deferred children until tool_search_output.
					continue
				}
				if deferred && !clientSearch {
					return ToolNormalizeResult{Err: newRequestError(param+".tools", "invalid_parameter",
						`defer_loading: true requires a client tool_search (execution: "client")`)}
				}
				rt := definition.responseTool()
				rt["name"] = compatState.registerFunction(nsName, definition.Name)
				delete(rt, "defer_loading")
				appendTool(rt)
			}
		case "tool_search":
			// Already registered in pre-scan; synthetic function emitted after the loop.
			continue
		case "web_search", "web_search_preview", "web_search_preview_2025_03_11", "web_search_2025_08_26", "websearch":
			// Codex: {type:web_search, external_web_access:bool, search_context_size, …}
			// Grok only accepts the bare variant (and uses top-level backend_search).
			normalized, access, warn, err := normalizeWebSearchTool(tool, param)
			if err != nil {
				return ToolNormalizeResult{Err: err}
			}
			for _, code := range warn {
				compatState.addWarning(code)
			}
			if access != nil {
				backendSearch = access
				if !*access {
					webSearchDisabled = true
					continue
				}
			}
			if normalized != nil {
				appendTool(normalized)
			}
		case "local_shell":
			// Upgrade bare local_shell → native shell + local environment.
			converted, err := compatState.normalizeLegacyLocalShellTool(tool, param)
			if err != nil {
				return ToolNormalizeResult{Err: err}
			}
			for _, item := range converted {
				if m, ok := item.(map[string]any); ok {
					appendTool(m)
				}
			}
		case "shell":
			converted, err := compatState.normalizeNativeShellTool(tool, param)
			if err != nil {
				return ToolNormalizeResult{Err: err}
			}
			for _, item := range converted {
				if m, ok := item.(map[string]any); ok {
					appendTool(m)
				}
			}
		case "apply_patch":
			converted, err := compatState.normalizeApplyPatchToolDeclaration(tool, param)
			if err != nil {
				return ToolNormalizeResult{Err: err}
			}
			for _, item := range converted {
				if m, ok := item.(map[string]any); ok {
					appendTool(m)
				}
			}
		case "custom":
			// Codex freeform / custom tools → flat function (restored on response).
			// Grammar formats other than text cannot be expressed on Grok Build.
			if format, exists := tool["format"]; exists {
				if formatObj, ok := format.(map[string]any); ok {
					ft := strings.ToLower(strings.TrimSpace(stringValue(formatObj["type"])))
					if ft != "" && ft != "text" {
						return ToolNormalizeResult{Err: newRequestError(param+".format", "unsupported_parameter", "Grok Build cannot emulate custom tool grammar")}
					}
				}
			}
			compatState.addWarning("custom_tool_emulated")
			name := firstNonEmptyString(tool["name"], "custom_tool")
			alias := compatState.registerCustom("", name)
			desc := firstNonEmptyString(tool["description"], "Custom tool "+name)
			if desc != "" && !strings.Contains(desc, "input string") {
				desc += "\nProvide the custom tool input in the input string field."
			}
			params := firstNonNil(tool["parameters"], tool["input_schema"])
			if params == nil {
				params = freeformToolParameters(tool)
			}
			appendTool(syntheticFunctionTool(alias, desc, params))
		case "mcp":
			deferred, _ := tool["defer_loading"].(bool)
			if deferred && !clientSearch {
				return ToolNormalizeResult{Err: newRequestError(param+".defer_loading", "invalid_parameter",
					`MCP defer_loading: true requires a client tool_search (execution: "client")`)}
			}
			if deferred && clientSearch {
				label := firstNonEmptyString(tool["server_label"], tool["name"])
				if label == "" {
					return ToolNormalizeResult{Err: newRequestError(param+".server_label", "invalid_parameter",
						"deferred MCP tool requires server_label")}
				}
				compatState.addDeferredSurface(label, firstNonEmptyString(tool["description"]))
				continue
			}
			clean := cloneMap(tool)
			delete(clean, "defer_loading")
			// Keep known-safe MCP keys only.
			allowed := map[string]struct{}{
				"type": {}, "server_label": {}, "server_url": {}, "server_description": {},
				"name": {}, "description": {}, "headers": {}, "require_approval": {},
				"allowed_tools": {}, "authorization": {},
			}
			for key := range clean {
				if _, ok := allowed[key]; !ok {
					delete(clean, key)
				}
			}
			appendTool(clean)
		case "computer_use_preview":
			return ToolNormalizeResult{Err: newRequestError(param+".type", "unsupported_parameter",
				fmt.Sprintf("tools.type=%q is not supported on this backend", typeName))}
		case "function", "":
			definition, valid := parseToolDefinition(tool)
			if valid && definition.Type == "function" {
				deferred := toolHasDeferLoading(tool)
				if deferred && !clientSearch {
					return ToolNormalizeResult{Err: newRequestError(param+".defer_loading", "invalid_parameter",
						`defer_loading: true requires a client tool_search (execution: "client")`)}
				}
				if deferred && clientSearch {
					compatState.addDeferredSurface(definition.Name, definition.Description)
					continue
				}
				// Client-declared function named apply_patch still gets reverse mapping.
				if strings.EqualFold(definition.Name, "apply_patch") {
					alias := compatState.registerApplyPatch()
					rt := definition.responseTool()
					rt["name"] = alias
					if !functionSchemaHasOperation(rt["parameters"]) {
						rt = applyPatchFunctionTool(alias)
					}
					delete(rt, "defer_loading")
					appendTool(rt)
					compatState.addWarning("apply_patch_emulated")
					continue
				}
				rt := definition.responseTool()
				delete(rt, "defer_loading")
				appendTool(rt)
			}
		default:
			if deferred, _ := tool["defer_loading"].(bool); deferred {
				return ToolNormalizeResult{Err: newRequestError(param+".defer_loading", "unsupported_parameter",
					"defer_loading is only supported on function and mcp tools with client tool_search")}
			}
			if _, allowed := grokBuiltinToolTypes[typeName]; allowed {
				// Keep type (+ name/description when present); drop Codex/OpenAI extras.
				// mcp is handled above; do not double-handle.
				if typeName == "mcp" {
					continue
				}
				clean := map[string]any{"type": typeName}
				if name := firstNonEmptyString(tool["name"]); name != "" {
					clean["name"] = name
				}
				if desc := firstNonEmptyString(tool["description"]); desc != "" {
					clean["description"] = desc
				}
				// file_search / code tools: pass known-safe keys.
				passthrough := []string{
					"server_label", "server_url", "server_description",
					"vector_store_ids", "max_num_results", "container", "parameters",
				}
				if typeName == "file_search" {
					passthrough = append(passthrough, "filters")
				}
				for _, key := range passthrough {
					if value, ok := tool[key]; ok {
						clean[key] = value
					}
				}
				appendTool(clean)
				continue
			}
			// Unknown OpenAI/Codex type: hard-reject (do not invent a fake function).
			return ToolNormalizeResult{Err: newRequestError(param+".type", "unsupported_parameter",
				fmt.Sprintf("tools.type=%q is not supported on this backend", typeName))}
		}
	}
	// Emit synthetic client tool_search function after all deferred surfaces are known.
	if clientSearch {
		searchFn, err := compatState.buildClientSearchFunction()
		if err != nil {
			return ToolNormalizeResult{Err: err}
		}
		if searchFn != nil {
			appendTool(searchFn)
		}
	}

	if webSearchDisabled {
		// Strip any bare web_search that slipped in from other tools in the same list.
		out = StripSearchTools(out)
		if out == nil {
			out = []any{}
		}
		falseVal := false
		backendSearch = &falseVal
		compatState.webSearchDisabled = true
		compatState.addWarning("web_search_disabled_no_external_access")
	}
	if len(out) == 0 {
		return ToolNormalizeResult{
			BackendSearch:     backendSearch,
			WebSearchDisabled: webSearchDisabled || compatState.webSearchDisabled,
			Warnings:          compatState.Warnings(),
			Compat:            compatState,
		}
	}
	// Grok keys tools by name across types: bare web_search + function web_search → 422.
	out = CollapseSearchToolNameCollisions(out)
	if maxTools > 0 && len(out) > maxTools {
		out = out[:maxTools]
		compatState.addWarning("tools_soft_capped")
	}
	return ToolNormalizeResult{
		Tools:             out,
		BackendSearch:     backendSearch,
		WebSearchDisabled: webSearchDisabled || compatState.webSearchDisabled,
		Warnings:          compatState.Warnings(),
		Compat:            compatState,
	}
}

func toolHasDeferLoading(tool map[string]any) bool {
	if deferred, ok := tool["defer_loading"].(bool); ok && deferred {
		return true
	}
	if fn, ok := tool["function"].(map[string]any); ok {
		if deferred, ok := fn["defer_loading"].(bool); ok && deferred {
			return true
		}
	}
	return false
}

// normalizeWebSearchTool collapses Codex web_search to bare form or rejects unsafe constraints.
// Returns (tool, externalAccess, warnings, err). tool is nil when the search tool is dropped.
func normalizeWebSearchTool(tool map[string]any, param string) (map[string]any, *bool, []string, error) {
	var access *bool
	var warnings []string
	if rawAccess, ok := tool["external_web_access"]; ok {
		value := truthy(rawAccess)
		access = &value
		if !value {
			// Expanding to bare web_search would grant external access the client forbade.
			return nil, access, []string{"web_search_disabled_no_external_access"}, nil
		}
	}
	if filters, ok := tool["filters"]; ok && hasNonEmptyConstraint(filters) {
		return nil, access, nil, newRequestError(param+".filters", "unsupported_parameter", "Grok Build cannot enforce web_search filters")
	}
	if domains, ok := tool["allowed_domains"]; ok && hasNonEmptyConstraint(domains) {
		return nil, access, nil, newRequestError(param+".allowed_domains", "unsupported_parameter", "Grok Build cannot enforce allowed_domains")
	}
	if contentTypes, ok := tool["search_content_types"]; ok {
		if values, isArr := contentTypes.([]any); isArr {
			for _, value := range values {
				if !strings.EqualFold(strings.TrimSpace(stringValue(value)), "text") &&
					strings.TrimSpace(stringValue(value)) != "" {
					return nil, access, nil, newRequestError(param+".search_content_types", "unsupported_parameter", "Grok Build only supports text web search")
				}
			}
		}
	}
	// Strip remaining Codex extras; track that we degraded.
	stripped := false
	for _, key := range webSearchRejectedFields {
		if _, exists := tool[key]; exists {
			if key == "external_web_access" {
				continue
			}
			if _, constrained := webSearchConstraintFields[key]; constrained {
				continue
			}
			stripped = true
		}
	}
	if stripped {
		warnings = append(warnings, "web_search_fields_stripped")
	}
	return map[string]any{"type": "web_search"}, access, warnings, nil
}

func hasNonEmptyConstraint(value any) bool {
	if value == nil {
		return false
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed) != ""
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	default:
		// Numbers / bools count as present constraints.
		return true
	}
}

// CollapseSearchToolNameCollisions prefers bare {type:web_search|x_search} over
// function tools named the same. Production error: "Duplicate tool names: web_search"
// when client sends function:web_search and we inject (or keep) bare web_search.
func CollapseSearchToolNameCollisions(tools []any) []any {
	if len(tools) == 0 {
		return tools
	}
	// First pass: which search builtins are needed / already bare.
	needBare := map[string]bool{}
	haveBare := map[string]bool{}
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typeName := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
		if _, isSearch := searchBuiltinTypes[typeName]; isSearch {
			haveBare[typeName] = true
			needBare[typeName] = true
			continue
		}
		if typeName == "function" || typeName == "" {
			name := strings.ToLower(strings.TrimSpace(firstNonEmptyString(tool["name"])))
			if _, isSearch := searchBuiltinTypes[name]; isSearch {
				needBare[name] = true
			}
		}
	}
	if len(needBare) == 0 {
		return tools
	}

	out := make([]any, 0, len(tools))
	// Emit needed bare search tools first (stable order).
	for _, name := range []string{"web_search", "x_search"} {
		if needBare[name] {
			out = append(out, map[string]any{"type": name})
			haveBare[name] = true
		}
	}
	seenFunc := map[string]struct{}{}
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typeName := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
		if _, isSearch := searchBuiltinTypes[typeName]; isSearch {
			// Already emitted bare form.
			continue
		}
		if typeName == "function" || typeName == "" {
			name := strings.ToLower(strings.TrimSpace(firstNonEmptyString(tool["name"])))
			if _, isSearch := searchBuiltinTypes[name]; isSearch {
				// Drop function shadow of search builtin.
				continue
			}
			if name != "" {
				if _, exists := seenFunc[name]; exists {
					continue
				}
				seenFunc[name] = struct{}{}
			}
		}
		out = append(out, tool)
	}
	return out
}

func syntheticFunctionTool(name, description string, parameters any) map[string]any {
	if parameters == nil {
		parameters = emptyObjectSchema()
	}
	tool := map[string]any{
		"type":       "function",
		"name":       name,
		"parameters": parameters,
	}
	if strings.TrimSpace(description) != "" {
		tool["description"] = description
	}
	return tool
}

func shellCommandParameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "Shell command to execute",
			},
			"workdir": map[string]any{
				"type":        "string",
				"description": "Optional working directory",
			},
		},
		"required": []any{"command"},
	}
}

func functionSchemaHasOperation(params any) bool {
	obj, ok := params.(map[string]any)
	if !ok {
		return false
	}
	props, ok := obj["properties"].(map[string]any)
	if !ok {
		return false
	}
	_, has := props["operation"]
	return has
}

func freeformToolParameters(tool map[string]any) map[string]any {
	// Preserve freeform format metadata in the schema description so the model
	// still sees syntax hints when Grok only accepts function tools.
	desc := "Freeform / custom tool input"
	if format, ok := tool["format"].(map[string]any); ok {
		if syntax := firstNonEmptyString(format["syntax"], format["type"]); syntax != "" {
			desc = "Freeform input (syntax: " + syntax + ")"
		}
	}
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"input": map[string]any{
				"type":        "string",
				"description": desc,
			},
		},
		"required": []any{"input"},
	}
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
		typeName := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
		switch typeName {
		case "web_search", "web_search_preview", "websearch":
			return true
		case "function", "":
			if strings.EqualFold(firstNonEmptyString(tool["name"]), "web_search") {
				return true
			}
		}
	}
	return false
}
