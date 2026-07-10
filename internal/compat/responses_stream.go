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
type ResponsesToChatStream struct {
	source     io.ReadCloser
	model      string
	reader     *bufio.Reader
	pending    []byte
	once       sync.Once
	err        error
	done       bool
	started    bool
	id         string
	toolIndex  map[string]int
	nextToolIx int
	sawTools   bool
}

func NewResponsesToChatStream(source io.ReadCloser, model string) *ResponsesToChatStream {
	return &ResponsesToChatStream{
		source:    source,
		model:     model,
		reader:    bufio.NewReader(source),
		id:        "chatcmpl_" + randomID(20),
		toolIndex: map[string]int{},
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
				finish := "stop"
				if s.sawTools {
					finish = "tool_calls"
				}
				s.queueContentChunk("", finish)
				s.pending = append(s.pending, []byte("data: [DONE]\n\n")...)
				s.done = true
				return nil
			}
			return io.EOF
		}
		return err
	}
	if event.Data == "[DONE]" {
		finish := "stop"
		if s.sawTools {
			finish = "tool_calls"
		}
		s.queueContentChunk("", finish)
		s.pending = append(s.pending, []byte("data: [DONE]\n\n")...)
		s.done = true
		return nil
	}
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
			s.emitToolCallStart(item)
		}
	case "response.function_call_arguments.delta":
		s.emitToolCallArgumentsDelta(event.Payload)
	case "response.output_item.done":
		if item, ok := event.Payload["item"].(map[string]any); ok {
			// Ensure name/id were emitted even if added was missed.
			s.emitToolCallStart(item)
		}
	case "response.completed":
		finish := "stop"
		if s.sawTools {
			finish = "tool_calls"
		}
		s.queueContentChunk("", finish)
		s.pending = append(s.pending, []byte("data: [DONE]\n\n")...)
		s.done = true
	}
	return nil
}

func (s *ResponsesToChatStream) emitToolCallStart(item map[string]any) {
	typ := strings.ToLower(strings.TrimSpace(stringValue(item["type"])))
	if typ != "function_call" && typ != "tool_call" && typ != "custom_tool_call" {
		return
	}
	call := responsesItemToChatToolCall(item)
	if call == nil {
		return
	}
	callID := stringValue(call["id"])
	if _, exists := s.toolIndex[callID]; exists {
		return
	}
	if !s.started {
		s.queueContentChunk("", "")
		s.started = true
	}
	index := s.nextToolIx
	s.toolIndex[callID] = index
	s.nextToolIx++
	s.sawTools = true
	fn, _ := call["function"].(map[string]any)
	s.queueToolCallChunk(index, callID, stringValue(fn["name"]), stringValue(fn["arguments"]))
}

func (s *ResponsesToChatStream) emitToolCallArgumentsDelta(payload map[string]any) {
	if payload == nil {
		return
	}
	delta := stringValue(payload["delta"])
	if delta == "" {
		return
	}
	callID := firstNonEmptyString(payload["call_id"], payload["item_id"], payload["id"])
	index, ok := s.toolIndex[callID]
	if !ok {
		// Unknown call — allocate a slot.
		index = s.nextToolIx
		if callID == "" {
			callID = "call_" + randomID(8)
		}
		s.toolIndex[callID] = index
		s.nextToolIx++
		s.sawTools = true
		if !s.started {
			s.queueContentChunk("", "")
			s.started = true
		}
		s.queueToolCallChunk(index, callID, "", delta)
		return
	}
	s.queueToolCallChunk(index, "", "", delta)
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

func (s *ResponsesToChatStream) queueToolCallChunk(index int, id, name, arguments string) {
	toolCall := map[string]any{
		"index": index,
	}
	if id != "" {
		toolCall["id"] = id
		toolCall["type"] = "function"
	}
	function := map[string]any{}
	if name != "" {
		function["name"] = name
	}
	if arguments != "" {
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
