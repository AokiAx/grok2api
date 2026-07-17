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

// AggregateResponsesStream reads a Responses SSE stream and returns Chat Completions JSON.
//
// If the body is a complete Responses JSON object (non-SSE) — which happens when
// the upstream request accidentally carried stream:false while the gateway treated
// the response as a stream — it falls back to ResponsesToChat so non-stream chat
// clients never receive an empty completion.
func AggregateResponsesStream(stream io.ReadCloser, model string) ([]byte, error) {
	defer stream.Close()

	raw, err := io.ReadAll(stream)
	if err != nil {
		return nil, fmt.Errorf("read responses stream: %w", err)
	}
	if converted, ok := tryResponsesJSONBody(raw); ok {
		return converted, nil
	}

	var (
		textBuilder strings.Builder
		finalBody   []byte
		responseID  string
		toolCalls   []any
	)

	err = IterateSSEBytes(raw, func(event SSEEvent) error {
		if event.Data == "[DONE]" || event.Payload == nil {
			return nil
		}
		switch event.Type {
		case "response.output_text.delta":
			textBuilder.WriteString(stringValue(event.Payload["delta"]))
		case "response.completed":
			if response, ok := event.Payload["response"].(map[string]any); ok {
				if encoded, err := json.Marshal(response); err == nil {
					finalBody = encoded
				}
				if id := stringValue(response["id"]); id != "" {
					responseID = id
				}
			}
		case "response.output_item.done", "response.output_item.added":
			if item, ok := event.Payload["item"].(map[string]any); ok {
				if call := responsesItemToChatToolCall(item); call != nil {
					// Prefer done events; for added, only keep if we do not already have it.
					toolCalls = upsertToolCall(toolCalls, call)
				}
			}
		default:
			if id := stringValue(event.Payload["id"]); id != "" && strings.HasPrefix(id, "resp_") {
				responseID = id
			}
			if delta := stringValue(event.Payload["delta"]); delta != "" && strings.Contains(event.Type, "output_text") {
				textBuilder.WriteString(delta)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read responses stream: %w", err)
	}

	if len(finalBody) > 0 {
		converted, err := ResponsesToChat(finalBody)
		if err == nil {
			return converted, nil
		}
	}

	synthetic := map[string]any{
		"id":          firstNonEmpty(responseID, "chatcmpl_"+randomID(20)),
		"model":       model,
		"output":      []any{},
		"output_text": textBuilder.String(),
		"status":      "completed",
	}
	if len(toolCalls) > 0 {
		// Reconstruct function_call items so ResponsesToChat emits tool_calls.
		output := make([]any, 0, len(toolCalls))
		for _, rawCall := range toolCalls {
			call, _ := rawCall.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			output = append(output, map[string]any{
				"type":      "function_call",
				"call_id":   stringValue(call["id"]),
				"name":      stringValue(fn["name"]),
				"arguments": stringValue(fn["arguments"]),
			})
		}
		synthetic["output"] = output
	}
	encoded, err := json.Marshal(synthetic)
	if err != nil {
		return nil, err
	}
	return ResponsesToChat(encoded)
}

func upsertToolCall(existing []any, call map[string]any) []any {
	id := stringValue(call["id"])
	if id == "" {
		return append(existing, call)
	}
	for i, raw := range existing {
		item, _ := raw.(map[string]any)
		if stringValue(item["id"]) == id {
			existing[i] = call
			return existing
		}
	}
	return append(existing, call)
}

// tryResponsesJSONBody converts a non-SSE Responses JSON body when present.
func tryResponsesJSONBody(raw []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return nil, false
	}
	if bytes.Contains(trimmed, []byte("\nevent:")) || bytes.HasPrefix(trimmed, []byte("event:")) ||
		bytes.Contains(trimmed, []byte("\ndata:")) || bytes.HasPrefix(trimmed, []byte("data:")) {
		return nil, false
	}
	var envelope map[string]any
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return nil, false
	}
	if typ := stringValue(envelope["type"]); typ == "response.completed" {
		if nested, ok := envelope["response"].(map[string]any); ok {
			encoded, err := json.Marshal(nested)
			if err != nil {
				return nil, false
			}
			converted, err := ResponsesToChat(encoded)
			if err != nil {
				return nil, false
			}
			return converted, true
		}
	}
	if _, hasOutput := envelope["output"]; hasOutput || stringValue(envelope["object"]) == "response" ||
		stringValue(envelope["output_text"]) != "" || stringValue(envelope["status"]) != "" {
		converted, err := ResponsesToChat(trimmed)
		if err != nil {
			return nil, false
		}
		return converted, true
	}
	return nil, false
}

// ResponsesToChatStream converts Responses SSE into OpenAI Chat Completions SSE,
// including function/tool call deltas for agent clients.
//
// OpenAI streaming contract (critical for Cursor/Claude Code):
//  1. First tool chunk: {index, id, type, function:{name, arguments:""}}
//  2. Later chunks: same index, only function.arguments as incremental deltas
//  3. Never invent a second index for the same call (empty {} + real args = invalid params)
type ResponsesToChatStream struct {
	source       io.ReadCloser
	model        string
	reader       *bufio.Reader
	pending      []byte
	once         sync.Once
	err          error
	done         bool
	started      bool
	id           string
	toolByCallID map[string]int
	toolByOutIdx map[int]int
	nextToolIx   int
	sawTools     bool
	// argsEmitted tracks whether any arguments delta was sent for a tool index.
	argsEmitted map[int]bool
}

func NewResponsesToChatStream(source io.ReadCloser, model string) *ResponsesToChatStream {
	return &ResponsesToChatStream{
		source:       source,
		model:        model,
		reader:       bufio.NewReader(source),
		id:           "chatcmpl_" + randomID(20),
		toolByCallID: map[string]int{},
		toolByOutIdx: map[int]int{},
		argsEmitted:  map[int]bool{},
	}
}

func (s *ResponsesToChatStream) Read(buffer []byte) (int, error) {
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

func (s *ResponsesToChatStream) Close() error {
	var closeErr error
	s.once.Do(func() {
		closeErr = s.source.Close()
	})
	return closeErr
}

func (s *ResponsesToChatStream) pull() error {
	if s.done {
		return io.EOF
	}
	event, err := ReadSSEEvent(s.reader)
	if err != nil {
		if err == io.EOF {
			if !s.done {
				// Do not forge finish_reason=stop: clients treat that as a full answer.
				msg := "upstream stream ended before a terminal event"
				if !s.started {
					msg = "upstream stream empty"
				}
				s.queueStreamError(msg, "upstream_stream_truncated")
				s.done = true
				return nil
			}
			return io.EOF
		}
		return err
	}
	if event.Data == "[DONE]" {
		// Bare [DONE] without response.completed/incomplete is not a success terminal.
		// Ignore here; EOF without a real terminal still surfaces as an error.
		return nil
	}
	outIdx := intValue(event.Payload["output_index"])
	switch event.Type {
	case "response.created", "response.in_progress":
		if !s.started {
			s.queueContentChunk("", "")
			s.started = true
		}
	case "response.output_text.delta":
		if !s.started {
			s.queueContentChunk("", "")
			s.started = true
		}
		s.queueContentChunk(stringValue(event.Payload["delta"]), "")
	case "response.output_item.added":
		if item, ok := event.Payload["item"].(map[string]any); ok {
			s.emitToolCallStart(item, outIdx)
		}
	case "response.function_call_arguments.delta":
		s.emitToolCallArgumentsDelta(event.Payload, outIdx)
	case "response.function_call_arguments.done":
		// Arguments already streamed via deltas; nothing to emit.
	case "response.output_item.done":
		if item, ok := event.Payload["item"].(map[string]any); ok {
			s.emitToolCallDone(item, outIdx)
		}
	case "response.completed":
		finish := "stop"
		if s.sawTools {
			finish = "tool_calls"
		}
		s.queueTerminal(finish)
	case "response.incomplete":
		// Truncated by max tokens / budget — still a terminal event, not a transport fault.
		finish := "length"
		if s.sawTools {
			finish = "tool_calls"
		}
		s.queueTerminal(finish)
	case "response.failed", "error":
		msg := streamErrorMessage(event.Payload, event.Data)
		if msg == "" {
			msg = "upstream response failed"
		}
		s.queueStreamError(msg, "upstream_error")
		s.done = true
	}
	return nil
}

func (s *ResponsesToChatStream) queueTerminal(finish string) {
	if s.done {
		return
	}
	if !s.started {
		s.queueContentChunk("", "")
		s.started = true
	}
	s.queueContentChunk("", finish)
	s.pending = append(s.pending, []byte("data: [DONE]\n\n")...)
	s.done = true
}

// queueStreamError emits an OpenAI-style mid-stream error object (no finish_reason stop).
func (s *ResponsesToChatStream) queueStreamError(message, code string) {
	if message == "" {
		message = "upstream stream error"
	}
	if code == "" {
		code = "upstream_error"
	}
	payload := map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": 0,
		"model":   s.model,
		"choices": []any{},
		"error": map[string]any{
			"message": message,
			"type":    "server_error",
			"code":    code,
		},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	var builder bytes.Buffer
	builder.WriteString("data: ")
	builder.Write(encoded)
	builder.WriteString("\n\n")
	s.pending = append(s.pending, builder.Bytes()...)
}

func streamErrorMessage(payload map[string]any, raw string) string {
	if payload == nil {
		return strings.TrimSpace(raw)
	}
	if msg := stringValue(payload["message"]); msg != "" {
		return msg
	}
	if errObj, ok := payload["error"].(map[string]any); ok {
		if msg := stringValue(errObj["message"]); msg != "" {
			return msg
		}
	}
	if response, ok := payload["response"].(map[string]any); ok {
		if errObj, ok := response["error"].(map[string]any); ok {
			if msg := stringValue(errObj["message"]); msg != "" {
				return msg
			}
		}
		if msg := stringValue(response["message"]); msg != "" {
			return msg
		}
	}
	return ""
}

func isFunctionCallItem(item map[string]any) bool {
	typ := strings.ToLower(strings.TrimSpace(stringValue(item["type"])))
	return typ == "function_call" || typ == "tool_call" || typ == "custom_tool_call"
}

func (s *ResponsesToChatStream) emitToolCallStart(item map[string]any, outputIndex int) {
	if !isFunctionCallItem(item) {
		return
	}
	callID, name, _ := functionCallFields(item)
	if name == "" && callID == "" {
		return
	}
	if callID == "" {
		callID = "call_" + randomID(12)
	}
	if _, exists := s.toolByCallID[callID]; exists {
		// Already started; still map output_index if missing.
		if outputIndex >= 0 {
			if idx, ok := s.toolByCallID[callID]; ok {
				s.toolByOutIdx[outputIndex] = idx
			}
		}
		return
	}
	if !s.started {
		s.queueContentChunk("", "")
		s.started = true
	}
	index := s.nextToolIx
	s.nextToolIx++
	s.toolByCallID[callID] = index
	if outputIndex >= 0 {
		s.toolByOutIdx[outputIndex] = index
	}
	s.sawTools = true
	// Start frame: id + name only. Arguments come from deltas (never invent "{}").
	s.queueToolCallChunk(index, callID, name, "", true)
}

func (s *ResponsesToChatStream) emitToolCallArgumentsDelta(payload map[string]any, outputIndex int) {
	if payload == nil {
		return
	}
	delta := jsonString(payload["delta"])
	if delta == "" {
		// Some backends put the fragment under "arguments".
		delta = jsonString(payload["arguments"])
	}
	if delta == "" {
		return
	}
	index, ok := s.resolveToolIndex(payload, outputIndex)
	if !ok {
		// Late delta without a prior added item: open a slot with whatever id we have.
		callID := firstNonEmptyString(payload["call_id"], payload["item_id"])
		if callID == "" {
			callID = "call_" + randomID(12)
		}
		if !s.started {
			s.queueContentChunk("", "")
			s.started = true
		}
		index = s.nextToolIx
		s.nextToolIx++
		s.toolByCallID[callID] = index
		if outputIndex >= 0 {
			s.toolByOutIdx[outputIndex] = index
		}
		s.sawTools = true
		s.queueToolCallChunk(index, callID, "", "", true)
	}
	s.queueToolCallChunk(index, "", "", delta, false)
	s.argsEmitted[index] = true
}

func (s *ResponsesToChatStream) emitToolCallDone(item map[string]any, outputIndex int) {
	if !isFunctionCallItem(item) {
		return
	}
	callID, name, arguments := functionCallFields(item)
	if callID == "" && name == "" {
		return
	}
	index, ok := s.resolveToolIndex(item, outputIndex)
	if !ok {
		// Missed added event entirely — emit a complete start + args once.
		if callID == "" {
			callID = "call_" + randomID(12)
		}
		if !s.started {
			s.queueContentChunk("", "")
			s.started = true
		}
		index = s.nextToolIx
		s.nextToolIx++
		s.toolByCallID[callID] = index
		if outputIndex >= 0 {
			s.toolByOutIdx[outputIndex] = index
		}
		s.sawTools = true
		s.queueToolCallChunk(index, callID, name, "", true)
		if arguments != "" {
			s.queueToolCallChunk(index, "", "", arguments, false)
			s.argsEmitted[index] = true
		}
		return
	}
	// If no argument deltas arrived, flush the final arguments once.
	if !s.argsEmitted[index] && arguments != "" {
		s.queueToolCallChunk(index, "", "", arguments, false)
		s.argsEmitted[index] = true
	}
}

func (s *ResponsesToChatStream) resolveToolIndex(payload map[string]any, outputIndex int) (int, bool) {
	if callID := firstNonEmptyString(payload["call_id"], payload["item_id"]); callID != "" {
		if idx, ok := s.toolByCallID[callID]; ok {
			return idx, true
		}
	}
	// item may carry call_id under nested fields
	if callID := firstNonEmptyString(payload["call_id"]); callID != "" {
		if idx, ok := s.toolByCallID[callID]; ok {
			return idx, true
		}
	}
	if outputIndex >= 0 {
		if idx, ok := s.toolByOutIdx[outputIndex]; ok {
			return idx, true
		}
	}
	return 0, false
}

func functionCallFields(item map[string]any) (callID, name, arguments string) {
	callID = firstNonEmptyString(item["call_id"])
	// Prefer call_id over generic id (item id may be fc_* while call_id is call-*).
	if callID == "" {
		// Only fall back to id when it looks like a call id.
		if id := stringValue(item["id"]); strings.HasPrefix(id, "call") || strings.HasPrefix(id, "fc_") {
			callID = id
		}
	}
	name = firstNonEmptyString(item["name"], item["tool_name"])
	if name == "" {
		if fn, ok := item["function"].(map[string]any); ok {
			name = stringValue(fn["name"])
		}
	}
	arguments = jsonString(item["arguments"])
	if arguments == "" {
		arguments = jsonString(item["input"])
	}
	if arguments == "" {
		if fn, ok := item["function"].(map[string]any); ok {
			arguments = jsonString(fn["arguments"])
		}
	}
	return callID, name, arguments
}

func jsonString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case []byte:
		return string(typed)
	case json.Number:
		return typed.String()
	case map[string]any, []any:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return ""
		}
		return string(encoded)
	default:
		// Avoid fmt.Sprint(map) which produces invalid tool parameters.
		encoded, err := json.Marshal(typed)
		if err != nil {
			return strings.TrimSpace(fmt.Sprint(typed))
		}
		// Numbers/bools become valid JSON tokens; fine as argument fragments.
		return string(encoded)
	}
}

func (s *ResponsesToChatStream) queueContentChunk(delta string, finish string) {
	choice := map[string]any{
		"index": 0,
		"delta": map[string]any{},
	}
	if delta != "" {
		choice["delta"] = map[string]any{"content": delta}
	} else if finish == "" && !s.started {
		choice["delta"] = map[string]any{"role": "assistant"}
	}
	if finish != "" {
		choice["finish_reason"] = finish
	} else {
		choice["finish_reason"] = nil
	}
	s.queueChoice(choice)
}

// queueToolCallChunk emits one OpenAI tool_calls delta.
// startFrame=true includes id/type/name; later frames should only carry arguments.
func (s *ResponsesToChatStream) queueToolCallChunk(index int, id, name, arguments string, startFrame bool) {
	toolCall := map[string]any{
		"index": index,
	}
	if startFrame {
		if id != "" {
			toolCall["id"] = id
		}
		toolCall["type"] = "function"
	}
	function := map[string]any{}
	if name != "" {
		function["name"] = name
	}
	if startFrame {
		// OpenAI clients expect arguments key present on the first frame.
		function["arguments"] = arguments
	} else if arguments != "" {
		function["arguments"] = arguments
	}
	if len(function) > 0 {
		toolCall["function"] = function
	}
	choice := map[string]any{
		"index":         0,
		"delta":         map[string]any{"tool_calls": []any{toolCall}},
		"finish_reason": nil,
	}
	s.queueChoice(choice)
}

func (s *ResponsesToChatStream) queueChoice(choice map[string]any) {
	payload := map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"created": 0,
		"model":   s.model,
		"choices": []any{choice},
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	var builder bytes.Buffer
	builder.WriteString("data: ")
	builder.Write(encoded)
	builder.WriteString("\n\n")
	s.pending = append(s.pending, builder.Bytes()...)
}
