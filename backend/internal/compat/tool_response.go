package compat

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
)

const (
	maxCompatibleResponseBytes = 128 << 20
	maxCompatibleSSEEventBytes = 8 << 20
)

// RewriteResponseJSON restores client-facing tool names/types on a completed
// Responses JSON body (or response.completed envelope).
func (c *ToolCompatibility) RewriteResponseJSON(body []byte) ([]byte, error) {
	if c == nil || len(body) == 0 {
		return body, nil
	}
	if !c.HasRewrites() && len(c.visibleTools) == 0 {
		return body, nil
	}
	trimmed := bytes.TrimSpace(body)
	// SSE blob: rewrite each completed payload via stream path helper.
	if bytes.Contains(trimmed, []byte("\nevent:")) || bytes.HasPrefix(trimmed, []byte("event:")) ||
		bytes.Contains(trimmed, []byte("\ndata:")) || bytes.HasPrefix(trimmed, []byte("data:")) {
		var last []byte
		err := IterateSSEBytes(body, func(event SSEEvent) error {
			if event.Data == "[DONE]" || len(event.Data) == 0 {
				return nil
			}
			rewritten, rewriteErr := c.rewriteSSEPayload(event.Type, []byte(event.Data))
			if rewriteErr != nil {
				return rewriteErr
			}
			if len(rewritten) > 0 {
				last = rewritten
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if len(last) > 0 {
			// Prefer completed response object when present.
			if completed := ExtractCompletedResponse(last); len(completed) > 0 {
				return completed, nil
			}
			return last, nil
		}
		return body, nil
	}

	var response map[string]any
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("decode responses body for tool rewrite: %w", err)
	}
	if nested, ok := response["response"].(map[string]any); ok && stringValue(response["type"]) == "response.completed" {
		if err := c.rewriteResponseValue(nested); err != nil {
			return nil, err
		}
		c.RestoreVisibleTools(nested)
		response["response"] = nested
	} else {
		if err := c.rewriteResponseValue(response); err != nil {
			return nil, err
		}
		c.RestoreVisibleTools(response)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return nil, fmt.Errorf("encode rewritten responses body: %w", err)
	}
	return encoded, nil
}

// RewriteResponseStream wraps an upstream Responses SSE stream and restores
// client-facing tool identities event-by-event.
func (c *ToolCompatibility) RewriteResponseStream(source io.ReadCloser) io.ReadCloser {
	if c == nil || !c.HasRewrites() || source == nil {
		return source
	}
	return &toolRewriteStream{source: source, compat: c, reader: bufio.NewReader(source)}
}

type toolRewriteStream struct {
	source  io.ReadCloser
	compat  *ToolCompatibility
	reader  *bufio.Reader
	pending []byte
	once    sync.Once
	err     error
	done    bool
}

func (s *toolRewriteStream) Read(buffer []byte) (int, error) {
	for len(s.pending) == 0 && s.err == nil && !s.done {
		if err := s.pull(); err != nil {
			s.err = err
			break
		}
	}
	if len(s.pending) > 0 {
		n := copy(buffer, s.pending)
		s.pending = s.pending[n:]
		return n, nil
	}
	if s.err != nil {
		return 0, s.err
	}
	return 0, io.EOF
}

func (s *toolRewriteStream) Close() error {
	var closeErr error
	s.once.Do(func() {
		closeErr = s.source.Close()
	})
	return closeErr
}

func (s *toolRewriteStream) pull() error {
	if s.done {
		return io.EOF
	}
	event, err := ReadSSEEvent(s.reader)
	if err != nil {
		if err == io.EOF {
			s.done = true
			return nil
		}
		return err
	}
	if event.Data == "[DONE]" {
		s.pending = append(s.pending, []byte("data: [DONE]\n\n")...)
		s.done = true
		return nil
	}
	if event.Data == "" && event.Type == "" {
		return nil
	}
	data := []byte(event.Data)
	if len(data) > 0 && event.Data != "[DONE]" {
		rewritten, rewriteErr := s.compat.rewriteSSEPayload(event.Type, data)
		if rewriteErr != nil {
			return rewriteErr
		}
		if rewritten == nil {
			// Drop internal-only events.
			return nil
		}
		data = rewritten
	}
	var b bytes.Buffer
	if event.Type != "" {
		b.WriteString("event: ")
		b.WriteString(event.Type)
		b.WriteByte('\n')
	}
	if len(data) > 0 {
		b.WriteString("data: ")
		b.Write(data)
		b.WriteString("\n\n")
	} else if event.Type != "" {
		b.WriteByte('\n')
	}
	s.pending = append(s.pending, b.Bytes()...)
	return nil
}

func (c *ToolCompatibility) rewriteSSEPayload(eventType string, data []byte) ([]byte, error) {
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return data, nil
	}
	if len(data) > maxCompatibleSSEEventBytes {
		return nil, fmt.Errorf("SSE event exceeds %d bytes", maxCompatibleSSEEventBytes)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		// Non-JSON data: pass through.
		return data, nil
	}
	kind := stringValue(payload["type"])
	if kind == "" {
		kind = eventType
	}
	// Track stream calls so argument deltas resolve aliases.
	if kind == "response.output_item.added" {
		if item, ok := payload["item"].(map[string]any); ok {
			c.rememberStreamCall(item)
			// Hold apply_patch start frames until arguments are complete on done.
			if c.isApplyPatchItem(item) {
				return nil, nil
			}
		}
	}
	if kind == "response.function_call_arguments.delta" {
		c.bufferStreamArgs(payload)
		if identity, _, ok := c.streamIdentity(payload); ok && identity.Kind == toolKindApplyPatch {
			// Hide synthetic function argument fragments for apply_patch.
			return nil, nil
		}
	}
	if kind == "response.function_call_arguments.done" {
		if identity, _, ok := c.streamIdentity(payload); ok && identity.Kind == toolKindApplyPatch {
			return nil, nil
		}
	}
	if kind == "response.output_item.done" {
		if item, ok := payload["item"].(map[string]any); ok {
			if c.isApplyPatchItem(item) {
				// Emit restored apply_patch_call as a single completed item event.
				if err := c.rewriteResponseValue(payload); err != nil {
					return nil, err
				}
				encoded, err := json.Marshal(payload)
				if err != nil {
					return nil, err
				}
				return encoded, nil
			}
		}
	}
	if err := c.rewriteResponseValue(payload); err != nil {
		return nil, err
	}
	if response, ok := payload["response"].(map[string]any); ok {
		_ = c.rewriteResponseValue(response)
		c.RestoreVisibleTools(response)
	}
	// Bare completed response objects (no envelope) may also carry tools.
	if stringValue(payload["object"]) == "response" || stringValue(payload["status"]) != "" {
		c.RestoreVisibleTools(payload)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode rewritten SSE payload: %w", err)
	}
	return encoded, nil
}

func (c *ToolCompatibility) isApplyPatchItem(item map[string]any) bool {
	if c == nil || item == nil {
		return false
	}
	name := firstNonEmptyString(item["name"])
	if identity, ok := c.aliases[name]; ok && identity.Kind == toolKindApplyPatch {
		return true
	}
	return false
}

func (c *ToolCompatibility) bufferStreamArgs(payload map[string]any) {
	if c == nil || payload == nil {
		return
	}
	if c.streamArgs == nil {
		c.streamArgs = make(map[string]*strings.Builder)
	}
	key := firstNonEmptyString(payload["call_id"], payload["item_id"])
	if key == "" {
		return
	}
	delta := jsonString(payload["delta"])
	if delta == "" {
		delta = jsonString(payload["arguments"])
	}
	if delta == "" {
		return
	}
	b, ok := c.streamArgs[key]
	if !ok {
		b = &strings.Builder{}
		c.streamArgs[key] = b
	}
	b.WriteString(delta)
}

func (c *ToolCompatibility) streamIdentity(payload map[string]any) (toolIdentity, *toolStreamCall, bool) {
	if c == nil {
		return toolIdentity{}, nil, false
	}
	for _, key := range []string{firstNonEmptyString(payload["call_id"], payload["item_id"]), firstNonEmptyString(payload["call_id"])} {
		if key == "" {
			continue
		}
		if state, ok := c.streamCalls[key]; ok {
			return state.identity, state, true
		}
	}
	name := firstNonEmptyString(payload["name"])
	if identity, ok := c.aliases[name]; ok {
		state := &toolStreamCall{identity: identity}
		for _, key := range []string{firstNonEmptyString(payload["call_id"], payload["item_id"])} {
			if key != "" {
				c.streamCalls[key] = state
			}
		}
		return identity, state, true
	}
	return toolIdentity{}, nil, false
}

func (c *ToolCompatibility) rememberStreamCall(item map[string]any) {
	if c == nil || !isFunctionCallItem(item) {
		return
	}
	name := firstNonEmptyString(item["name"])
	identity, exists := c.aliases[name]
	if !exists {
		return
	}
	state := &toolStreamCall{identity: identity}
	for _, key := range []string{stringValue(item["id"]), stringValue(item["call_id"])} {
		if key != "" {
			c.streamCalls[key] = state
		}
	}
}

func (c *ToolCompatibility) rewriteResponseValue(value any) error {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if err := c.rewriteResponseValue(item); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, item := range typed {
			if err := c.rewriteResponseValue(item); err != nil {
				return err
			}
		}
		switch strings.ToLower(strings.TrimSpace(stringValue(typed["type"]))) {
		case "function_call", "tool_call":
			c.rewriteFunctionCall(typed)
		case "shell_call":
			if c.legacyLocalShell {
				rewriteLegacyLocalShellCall(typed)
			}
		}
		// Also restore bare name fields on argument delta events.
		if name := firstNonEmptyString(typed["name"]); name != "" {
			if identity, ok := c.aliases[name]; ok {
				c.applyIdentityToNameFields(typed, identity)
			}
		}
	}
	return nil
}

func (c *ToolCompatibility) rewriteFunctionCall(call map[string]any) {
	name := firstNonEmptyString(call["name"])
	identity, exists := c.aliases[name]
	if !exists {
		// Try stream call registry.
		for _, key := range []string{stringValue(call["call_id"]), stringValue(call["id"])} {
			if state, ok := c.streamCalls[key]; ok {
				identity = state.identity
				exists = true
				break
			}
		}
	}
	if !exists {
		return
	}
	switch identity.Kind {
	case toolKindFunction:
		call["name"] = identity.Name
		if identity.Namespace != "" {
			call["namespace"] = identity.Namespace
		} else {
			delete(call, "namespace")
		}
	case toolKindCustom:
		call["type"] = "custom_tool_call"
		call["name"] = identity.Name
		if identity.Namespace != "" {
			call["namespace"] = identity.Namespace
		} else {
			delete(call, "namespace")
		}
		if _, hasInput := call["input"]; !hasInput {
			call["input"] = decodeCustomToolInput(call["arguments"])
		}
		delete(call, "arguments")
	case toolKindToolSearch:
		call["type"] = "tool_search_call"
		call["execution"] = "client"
		if args := call["arguments"]; args != nil {
			call["arguments"] = decodeToolSearchArguments(args)
		}
		delete(call, "name")
		delete(call, "namespace")
	case toolKindApplyPatch:
		operation, err := decodeApplyPatchArguments(call["arguments"], "response.output[].arguments")
		if err != nil {
			// Best-effort: keep function_call shape rather than drop the turn.
			call["name"] = "apply_patch"
			return
		}
		call["type"] = "apply_patch_call"
		call["operation"] = operation
		delete(call, "name")
		delete(call, "namespace")
		delete(call, "arguments")
	}
}

func (c *ToolCompatibility) applyIdentityToNameFields(payload map[string]any, identity toolIdentity) {
	switch identity.Kind {
	case toolKindFunction:
		payload["name"] = identity.Name
		if identity.Namespace != "" {
			payload["namespace"] = identity.Namespace
		}
	case toolKindCustom:
		payload["name"] = identity.Name
		if identity.Namespace != "" {
			payload["namespace"] = identity.Namespace
		}
	case toolKindToolSearch, toolKindApplyPatch:
		// Keep synthetic name out of client-facing deltas when possible.
		delete(payload, "name")
	}
}

func decodeCustomToolInput(arguments any) string {
	switch typed := arguments.(type) {
	case string:
		var obj map[string]any
		if json.Unmarshal([]byte(typed), &obj) == nil {
			if input, ok := obj["input"].(string); ok {
				return input
			}
			// freeform may store the whole string as input
			if raw, ok := obj["input"]; ok {
				return stringifyOutput(raw)
			}
		}
		return typed
	default:
		return stringifyOutput(typed)
	}
}

func decodeToolSearchArguments(arguments any) any {
	switch typed := arguments.(type) {
	case string:
		var obj any
		if json.Unmarshal([]byte(typed), &obj) == nil {
			return obj
		}
		return map[string]any{"query": typed}
	default:
		return typed
	}
}
