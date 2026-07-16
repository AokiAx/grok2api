package compat

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Codex / OpenAI Build tool emulations (apply_patch, local_shell upgrade).
// Kept separate from tools.go so declaration, history, and response rewrite stay co-located.

func applyPatchFunctionTool(alias string) map[string]any {
	return map[string]any{
		"type": "function",
		"name": alias,
		"description": "Create, update, or delete one file using a structured V4A patch operation. " +
			"create_file and update_file require path and diff; delete_file requires path.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"operation": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"type": map[string]any{
							"type": "string",
							"enum": []any{"create_file", "update_file", "delete_file"},
						},
						"path": map[string]any{"type": "string", "minLength": 1},
						"diff": map[string]any{"type": "string"},
					},
					"required":             []any{"type", "path"},
					"additionalProperties": false,
				},
			},
			"required":             []any{"operation"},
			"additionalProperties": false,
		},
		"strict": true,
	}
}

// normalizeApplyPatchToolDeclaration converts {type:apply_patch} into a strict function.
func (c *ToolCompatibility) normalizeApplyPatchToolDeclaration(tool map[string]any, param string) ([]any, error) {
	for key := range tool {
		if key != "type" {
			return nil, newRequestError(param+"."+key, "unsupported_parameter", "apply_patch does not accept custom fields")
		}
	}
	c.addWarning("apply_patch_emulated")
	alias := c.registerApplyPatch()
	return []any{applyPatchFunctionTool(alias)}, nil
}

// normalizeLegacyLocalShellTool upgrades bare local_shell to native shell+local env.
func (c *ToolCompatibility) normalizeLegacyLocalShellTool(tool map[string]any, param string) ([]any, error) {
	if c.nativeShell || c.legacyLocalShell {
		return nil, newRequestError(param+".type", "invalid_parameter", "only one shell/local_shell tool is allowed per request")
	}
	for key := range tool {
		if key != "type" && key != "description" {
			return nil, newRequestError(param+"."+key, "unsupported_parameter", "legacy local_shell does not accept extra fields")
		}
	}
	if c.nativeShell {
		return nil, newRequestError(param+".type", "invalid_parameter", "cannot mix shell and local_shell")
	}
	c.legacyLocalShell = true
	c.addWarning("legacy_local_shell_upgraded")
	return []any{map[string]any{
		"type":        "shell",
		"environment": map[string]any{"type": "local"},
	}}, nil
}

// normalizeNativeShellTool keeps shell with environment; bare shell falls back to function.
func (c *ToolCompatibility) normalizeNativeShellTool(tool map[string]any, param string) ([]any, error) {
	if c.legacyLocalShell {
		return nil, newRequestError(param+".type", "invalid_parameter", "cannot mix shell and local_shell in one request")
	}
	if _, hasEnv := tool["environment"]; !hasEnv {
		// Incomplete shell object 422s on Grok — emulate as shell_command function.
		c.addWarning("shell_emulated_as_function")
		return []any{syntheticFunctionTool(
			"shell_command",
			firstNonEmptyString(tool["description"], "Run a shell command."),
			shellCommandParameters(),
		)}, nil
	}
	c.nativeShell = true
	clean := map[string]any{"type": "shell", "environment": tool["environment"]}
	if name := firstNonEmptyString(tool["name"]); name != "" {
		clean["name"] = name
	}
	return []any{clean}, nil
}

func applyPatchCallToFunctionCall(item map[string]any, state *ToolCompatibility) map[string]any {
	callID := state.ensureCallID(firstNonEmptyString(item["call_id"], item["id"]))
	operation, err := validateApplyPatchOperation(firstNonNil(item["operation"], item["action"]), "input.apply_patch_call.operation")
	if err != nil {
		// Lenient multi-turn history: pack residual fields instead of 422ing the session.
		patch := firstNonEmptyString(item["patch"], digString(item, "action", "patch"), digString(item, "action", "input"))
		if patch == "" {
			patch = stringifyOutput(firstNonNil(item["operation"], item["action"], item))
		}
		operation = map[string]any{"type": "update_file", "path": "unknown", "diff": patch}
	}
	encoded, encErr := json.Marshal(map[string]any{"operation": operation})
	if encErr != nil {
		encoded = []byte(`{"operation":{"type":"update_file","path":"unknown","diff":""}}`)
	}
	out := map[string]any{
		"type":      "function_call",
		"call_id":   callID,
		"name":      state.registerApplyPatch(),
		"arguments": string(encoded),
	}
	if status := strings.TrimSpace(stringValue(item["status"])); status != "" {
		out["status"] = status
	}
	return out
}

func applyPatchOutputToFunctionCallOutput(item map[string]any, state *ToolCompatibility) map[string]any {
	callID := state.takeCallID(firstNonEmptyString(item["call_id"], item["id"]))
	status := strings.TrimSpace(stringValue(item["status"]))
	if status == "" {
		status = "completed"
	}
	output := stringifyOutput(firstNonNil(item["output"], item["result"]))
	message := "Apply patch status: " + status
	if output != "" {
		message += "\n" + output
	}
	return map[string]any{
		"type":    "function_call_output",
		"call_id": callID,
		"output":  message,
	}
}

func validateApplyPatchOperation(value any, param string) (map[string]any, error) {
	operation, ok := value.(map[string]any)
	if !ok || operation == nil {
		return nil, newRequestError(param, "invalid_parameter", "apply_patch operation must be an object")
	}
	kind := strings.TrimSpace(stringValue(operation["type"]))
	path := strings.TrimSpace(stringValue(operation["path"]))
	if path == "" {
		return nil, newRequestError(param+".path", "invalid_parameter", "apply_patch operation.path is required")
	}
	switch kind {
	case "create_file", "update_file":
		if _, ok := operation["diff"].(string); !ok {
			// tolerate non-string diff via stringify
			if operation["diff"] == nil {
				return nil, newRequestError(param+".diff", "invalid_parameter", kind+" requires diff string")
			}
		}
	case "delete_file":
	default:
		return nil, newRequestError(param+".type", "invalid_parameter", "apply_patch operation.type is invalid")
	}
	return cloneMap(operation), nil
}

func decodeApplyPatchArguments(value any, param string) (map[string]any, error) {
	text, ok := value.(string)
	if !ok {
		if m, isMap := value.(map[string]any); isMap {
			return validateApplyPatchOperation(m["operation"], param+".operation")
		}
		return nil, newRequestError(param, "invalid_parameter", "apply_patch function arguments must be a string")
	}
	var wrapper map[string]any
	if err := json.Unmarshal([]byte(text), &wrapper); err != nil {
		return nil, newRequestError(param, "invalid_parameter", "apply_patch function arguments must be valid JSON")
	}
	if op, ok := wrapper["operation"]; ok {
		return validateApplyPatchOperation(op, param+".operation")
	}
	// Some models emit the operation object at the top level.
	if _, hasType := wrapper["type"]; hasType {
		return validateApplyPatchOperation(wrapper, param)
	}
	return nil, newRequestError(param+".operation", "invalid_parameter", "apply_patch arguments missing operation")
}

// localShellCallToNativeShellCall maps history local_shell_call onto shell_call
// when tools were upgraded to native shell; otherwise keeps function_call path.
func localShellCallToNativeOrFunction(item map[string]any, state *ToolCompatibility) map[string]any {
	if state != nil && (state.legacyLocalShell || state.nativeShell) {
		callID := state.ensureCallID(firstNonEmptyString(item["call_id"], item["id"]))
		action := shellActionFromItem(item)
		out := map[string]any{
			"type":    "shell_call",
			"call_id": callID,
			"action":  action,
		}
		if status := strings.TrimSpace(stringValue(item["status"])); status != "" {
			out["status"] = status
		}
		return out
	}
	return localShellCallToFunctionCall(item, state)
}

func shellActionFromItem(item map[string]any) map[string]any {
	action, _ := item["action"].(map[string]any)
	if action == nil {
		action = map[string]any{}
	}
	command := ""
	switch typed := action["command"].(type) {
	case string:
		command = typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, p := range typed {
			parts = append(parts, fmt.Sprint(p))
		}
		command = strings.Join(parts, " ")
	}
	if command == "" {
		command = stringifyOutput(item["command"])
	}
	out := map[string]any{
		"type":    firstNonEmptyString(action["type"], "exec"),
		"command": command,
	}
	if wd := firstNonEmptyString(action["working_directory"], action["workdir"]); wd != "" {
		out["working_directory"] = wd
	}
	if env, ok := action["env"].(map[string]any); ok {
		out["env"] = env
	}
	return out
}

func rewriteLegacyLocalShellCall(call map[string]any) {
	call["type"] = "local_shell_call"
	if action, ok := call["action"].(map[string]any); ok {
		// Prefer Codex local_shell action shape.
		if _, hasType := action["type"]; !hasType {
			action["type"] = "exec"
		}
		call["action"] = action
	}
	delete(call, "max_output_length")
}
