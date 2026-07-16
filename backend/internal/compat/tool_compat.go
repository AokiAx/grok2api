package compat

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// RequestError is a client-facing validation failure with OpenAI-style param/code.
// Prefer this over silent drops when a constraint cannot be enforced upstream.
type RequestError struct {
	Message string
	Param   string
	Code    string
}

func (e *RequestError) Error() string {
	if e == nil {
		return ""
	}
	if e.Param != "" {
		return fmt.Sprintf("%s: %s", e.Param, e.Message)
	}
	return e.Message
}

// AsRequestError extracts a *RequestError from an error chain.
func AsRequestError(err error) (*RequestError, bool) {
	var re *RequestError
	if errors.As(err, &re) {
		return re, true
	}
	return nil, false
}

func newRequestError(param, code, message string) *RequestError {
	return &RequestError{Message: message, Param: param, Code: code}
}

const maxToolAliasLength = 128

type toolIdentityKind uint8

const (
	toolKindFunction toolIdentityKind = iota
	toolKindCustom
	toolKindToolSearch
	toolKindApplyPatch
)

// toolIdentity is the client-facing identity of a rewritten tool.
type toolIdentity struct {
	Kind      toolIdentityKind
	Namespace string
	Name      string
}

func (i toolIdentity) key() string {
	return fmt.Sprintf("%d\x00%s\x00%s", i.Kind, i.Namespace, i.Name)
}

// ToolCompatibility holds per-request tool rewrite state.
// One instance per request; do not reuse across requests.
//
// Lifecycle:
//
//	normalize tools/input → register aliases → upstream → rewrite response JSON/SSE
type ToolCompatibility struct {
	warnings          []string
	warningSet        map[string]struct{}
	webSearchDisabled bool
	// legacyLocalShell: client sent local_shell → upgraded to native shell.
	// Responses of type shell_call are restored to local_shell_call.
	legacyLocalShell bool
	nativeShell      bool
	// Client tool_search (execution=client): defer_loading surfaces + synthetic search function.
	clientSearchActive bool
	clientSearchTool   map[string]any
	clientSearchParam  string
	deferredSurfaces   []string
	// Tools loaded mid-conversation from tool_search_output / additional_tools.
	historyLoadedTools []any
	// visibleTools is the client-facing tools[] list restored on Responses egress.
	visibleTools []any
	// openCallIDs pairs function_call → function_call_output when ids are missing.
	openCallIDs []string
	// aliases maps upstream function name → client identity.
	aliases map[string]toolIdentity
	// identityAliases maps identity.key → upstream alias name.
	identityAliases map[string]string
	// streamCalls tracks in-flight streamed function calls by id/call_id.
	streamCalls map[string]*toolStreamCall
	// streamArgs buffers arguments for apply_patch/tool_search until item.done.
	streamArgs map[string]*strings.Builder
	changed    bool
}

type toolStreamCall struct {
	identity toolIdentity
}

func newToolCompatibility() *ToolCompatibility {
	return &ToolCompatibility{
		warningSet:      make(map[string]struct{}),
		aliases:         make(map[string]toolIdentity),
		identityAliases: make(map[string]string),
		streamCalls:     make(map[string]*toolStreamCall),
		streamArgs:      make(map[string]*strings.Builder),
	}
}

func (c *ToolCompatibility) addWarning(code string) {
	if c == nil || code == "" {
		return
	}
	if c.warningSet == nil {
		c.warningSet = make(map[string]struct{})
	}
	if _, exists := c.warningSet[code]; exists {
		return
	}
	c.warningSet[code] = struct{}{}
	c.warnings = append(c.warnings, code)
}

// Warnings returns stable compatibility codes for X-Grok2API-Compatibility-Warnings.
func (c *ToolCompatibility) Warnings() []string {
	if c == nil {
		return nil
	}
	return append([]string(nil), c.warnings...)
}

// HasRewrites reports whether response-side stream/body rewrite is needed.
// Alias and local_shell restoration require parsing; visibleTools alone does not
// (and must not wrap plain JSON upstream bodies as SSE).
func (c *ToolCompatibility) HasRewrites() bool {
	return c != nil && (len(c.aliases) > 0 || c.legacyLocalShell)
}

// CaptureVisibleTools stores the client-facing tools list for response restore.
func (c *ToolCompatibility) CaptureVisibleTools(raw any) {
	if c == nil {
		return
	}
	tools, ok := raw.([]any)
	if !ok || len(tools) == 0 {
		return
	}
	c.visibleTools = deepCloneAnySlice(tools)
}

// AppendVisibleTools appends tools revealed mid-conversation for response restore.
func (c *ToolCompatibility) AppendVisibleTools(tools []any) {
	if c == nil || len(tools) == 0 {
		return
	}
	c.visibleTools = append(c.visibleTools, deepCloneAnySlice(tools)...)
}

// RestoreVisibleTools rewrites response.tools back to the client-facing declaration.
func (c *ToolCompatibility) RestoreVisibleTools(response map[string]any) {
	if c == nil || response == nil || len(c.visibleTools) == 0 {
		return
	}
	if _, exists := response["tools"]; !exists {
		return
	}
	response["tools"] = deepCloneAnySlice(c.visibleTools)
}

func deepCloneAnySlice(values []any) []any {
	if len(values) == 0 {
		return nil
	}
	encoded, err := json.Marshal(values)
	if err != nil {
		out := make([]any, len(values))
		copy(out, values)
		return out
	}
	var out []any
	if json.Unmarshal(encoded, &out) != nil {
		out = make([]any, len(values))
		copy(out, values)
	}
	return out
}

func (c *ToolCompatibility) pushCallID(id string) {
	if c == nil || id == "" {
		return
	}
	c.openCallIDs = append(c.openCallIDs, id)
}

// takeCallID returns explicit when set; otherwise the oldest unmatched call id.
// Generates a new id only when there is no open call to pair with.
func (c *ToolCompatibility) takeCallID(explicit string) string {
	if explicit != "" {
		if c != nil {
			for i, id := range c.openCallIDs {
				if id == explicit {
					c.openCallIDs = append(c.openCallIDs[:i], c.openCallIDs[i+1:]...)
					break
				}
			}
		}
		return explicit
	}
	if c != nil && len(c.openCallIDs) > 0 {
		id := c.openCallIDs[0]
		c.openCallIDs = c.openCallIDs[1:]
		return id
	}
	return "call_" + randomID(12)
}

func (c *ToolCompatibility) ensureCallID(explicit string) string {
	id := strings.TrimSpace(explicit)
	if id == "" {
		id = "call_" + randomID(12)
	}
	c.pushCallID(id)
	return id
}

// registerFunction maps a (namespace, name) function onto an upstream-safe alias.
func (c *ToolCompatibility) registerFunction(namespace, name string) string {
	if c == nil {
		return name
	}
	return c.alias(toolIdentity{Kind: toolKindFunction, Namespace: namespace, Name: name})
}

// registerCustom maps a custom/freeform tool onto a function alias.
func (c *ToolCompatibility) registerCustom(namespace, name string) string {
	if c == nil {
		return name
	}
	return c.alias(toolIdentity{Kind: toolKindCustom, Namespace: namespace, Name: name})
}

// registerToolSearch maps client tool_search onto a function alias.
func (c *ToolCompatibility) registerToolSearch(name string) string {
	if c == nil {
		return name
	}
	if name == "" {
		name = "tool_search"
	}
	return c.alias(toolIdentity{Kind: toolKindToolSearch, Name: name})
}

// registerApplyPatch maps Codex apply_patch onto a strict function alias.
func (c *ToolCompatibility) registerApplyPatch() string {
	if c == nil {
		return "grok2api_apply_patch"
	}
	return c.alias(toolIdentity{Kind: toolKindApplyPatch, Name: "apply_patch"})
}

func (c *ToolCompatibility) alias(identity toolIdentity) string {
	if c.aliases == nil {
		c.aliases = make(map[string]toolIdentity)
	}
	if c.identityAliases == nil {
		c.identityAliases = make(map[string]string)
	}
	key := identity.key()
	if existing, ok := c.identityAliases[key]; ok {
		return existing
	}
	base := identity.Name
	switch identity.Kind {
	case toolKindToolSearch:
		base = "grok2api_tool_search"
	case toolKindApplyPatch:
		base = "grok2api_apply_patch"
	case toolKindFunction, toolKindCustom:
		if identity.Namespace != "" {
			base = identity.Namespace + "__" + identity.Name
		}
	}
	alias := truncateToolAlias(base, key)
	if prev, collision := c.aliases[alias]; collision && prev.key() != key {
		alias = hashedToolAlias(base, key)
	}
	c.aliases[alias] = identity
	c.identityAliases[key] = alias
	if alias != identity.Name || identity.Namespace != "" || identity.Kind != toolKindFunction {
		c.changed = true
	}
	return alias
}

func truncateToolAlias(base, key string) string {
	if len(base) <= maxToolAliasLength {
		return base
	}
	return hashedToolAlias(base, key)
}

func hashedToolAlias(base, key string) string {
	suffix := "__" + shortToolHash(key)
	limit := maxToolAliasLength - len(suffix)
	if limit < 1 {
		return suffix
	}
	if len(base) > limit {
		base = base[:limit]
	}
	return base + suffix
}

func shortToolHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:9]
}

// lookupAlias resolves a client (namespace, name) to the upstream alias when registered.
func (c *ToolCompatibility) lookupAlias(namespace, name string) (string, bool) {
	if c == nil {
		return "", false
	}
	for _, kind := range []toolIdentityKind{toolKindFunction, toolKindCustom, toolKindToolSearch} {
		key := toolIdentity{Kind: kind, Namespace: namespace, Name: name}.key()
		if alias, ok := c.identityAliases[key]; ok {
			return alias, true
		}
	}
	return "", false
}

// AlignToolChoice rewrites tool_choice after tools[] normalization.
func (c *ToolCompatibility) AlignToolChoice(choice any, tools []any) (any, []string) {
	webSearchDisabled := c != nil && c.webSearchDisabled
	aligned, warnings := AlignResponsesToolChoice(choice, tools, webSearchDisabled)
	if c == nil || aligned == nil {
		return aligned, warnings
	}
	// Map namespaced / original client names onto registered aliases.
	obj, ok := aligned.(map[string]any)
	if !ok {
		return aligned, warnings
	}
	kind := strings.ToLower(strings.TrimSpace(stringValue(obj["type"])))
	if kind == "apply_patch" {
		alias, exists := c.lookupAlias("", "apply_patch")
		if !exists {
			// Also match apply_patch identity kind.
			if a, ok := c.identityAliases[toolIdentity{Kind: toolKindApplyPatch, Name: "apply_patch"}.key()]; ok {
				alias, exists = a, true
			}
		}
		if !exists {
			return aligned, warnings
		}
		return map[string]any{"type": "function", "name": alias}, append(warnings, "apply_patch_tool_choice_aligned")
	}
	if kind == "tool_search" {
		if !c.clientSearchActive {
			return aligned, warnings
		}
		alias := c.registerToolSearch("tool_search")
		return map[string]any{"type": "function", "name": alias}, append(warnings, "tool_search_tool_choice_aligned")
	}
	if kind != "function" {
		return aligned, warnings
	}
	name := firstNonEmptyString(obj["name"])
	namespace := firstNonEmptyString(obj["namespace"])
	if alias, ok := c.lookupAlias(namespace, name); ok {
		obj = cloneMap(obj)
		obj["name"] = alias
		delete(obj, "namespace")
		c.changed = true
		return obj, warnings
	}
	// function named apply_patch after register
	if strings.EqualFold(name, "apply_patch") {
		if alias, ok := c.identityAliases[toolIdentity{Kind: toolKindApplyPatch, Name: "apply_patch"}.key()]; ok {
			obj = cloneMap(obj)
			obj["name"] = alias
			c.changed = true
			return obj, warnings
		}
	}
	return aligned, warnings
}

// AlignResponsesToolChoice rewrites tool_choice after tools[] normalization so
// hosted/search collapses cannot leave a dangling function force.
//
// Policy:
//   - web_search disabled and no tools left → "none"
//   - tool_choice forces web_search/x_search (function or hosted) while bare
//     search tool exists → "required"
//   - tool_choice forces search while search was stripped → "none"
func AlignResponsesToolChoice(choice any, tools []any, webSearchDisabled bool) (any, []string) {
	if choice == nil {
		return nil, nil
	}
	var warnings []string
	warn := func(code string) {
		for _, existing := range warnings {
			if existing == code {
				return
			}
		}
		warnings = append(warnings, code)
	}

	if s, ok := choice.(string); ok {
		if (s == "auto" || s == "required") && webSearchDisabled && len(tools) == 0 {
			warn("web_search_tool_choice_disabled")
			return "none", warnings
		}
		return choice, nil
	}

	// Flatten Chat Completions nested function shape first.
	normalized := NormalizeResponsesToolChoice(choice)
	obj, ok := normalized.(map[string]any)
	if !ok {
		return normalized, nil
	}

	kind := strings.ToLower(strings.TrimSpace(stringValue(obj["type"])))
	if hosted := normalizeHostedToolChoiceKind(kind); hosted != "" {
		if webSearchDisabled && hosted == "web_search" && !hasToolType(tools, "web_search") {
			warn("web_search_tool_choice_disabled")
			return "none", warnings
		}
		if hasToolType(tools, hosted) {
			warn("hosted_tool_choice_required")
			return "required", warnings
		}
		return normalized, nil
	}

	if kind == "function" {
		name := strings.ToLower(strings.TrimSpace(firstNonEmptyString(obj["name"])))
		if _, isSearch := searchBuiltinTypes[name]; isSearch {
			if hasToolType(tools, name) {
				warn("search_tool_choice_aligned")
				return "required", warnings
			}
			if webSearchDisabled {
				warn("web_search_tool_choice_disabled")
				return "none", warnings
			}
		}
		return normalized, nil
	}

	return normalized, nil
}

func normalizeHostedToolChoiceKind(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "web_search", "web_search_preview", "web_search_preview_2025_03_11", "web_search_2025_08_26", "websearch":
		return "web_search"
	case "x_search":
		return "x_search"
	default:
		return ""
	}
}

func hasToolType(tools []any, kind string) bool {
	kind = strings.ToLower(strings.TrimSpace(kind))
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if strings.ToLower(strings.TrimSpace(stringValue(tool["type"]))) == kind {
			return true
		}
	}
	return false
}

// StripSearchTools removes bare and function-named web_search / x_search tools.
func StripSearchTools(raw any) []any {
	tools, ok := raw.([]any)
	if !ok || len(tools) == 0 {
		return nil
	}
	out := make([]any, 0, len(tools))
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			out = append(out, rawTool)
			continue
		}
		typeName := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
		if _, isSearch := searchBuiltinTypes[typeName]; isSearch {
			continue
		}
		if typeName == "function" || typeName == "" {
			name := strings.ToLower(strings.TrimSpace(firstNonEmptyString(tool["name"])))
			if _, isSearch := searchBuiltinTypes[name]; isSearch {
				continue
			}
		}
		out = append(out, tool)
	}
	return out
}
