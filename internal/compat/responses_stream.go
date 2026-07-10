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
	)

	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 2*1024*1024)
	var eventName string
	var dataLines []string
	flush := func() error {
		if len(dataLines) == 0 {
			eventName = ""
			return nil
		}
		data := strings.Join(dataLines, "\n")
		dataLines = nil
		currentEvent := eventName
		eventName = ""
		if data == "[DONE]" {
			return nil
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			return nil
		}
		typeName := stringValue(payload["type"])
		if typeName == "" {
			typeName = currentEvent
		}
		switch typeName {
		case "response.output_text.delta":
			textBuilder.WriteString(stringValue(payload["delta"]))
		case "response.completed":
			if response, ok := payload["response"].(map[string]any); ok {
				encoded, err := json.Marshal(response)
				if err == nil {
					finalBody = encoded
				}
				if id := stringValue(response["id"]); id != "" {
					responseID = id
				}
			}
		default:
			if id := stringValue(payload["id"]); id != "" && strings.HasPrefix(id, "resp_") {
				responseID = id
			}
			if delta := stringValue(payload["delta"]); delta != "" && strings.Contains(typeName, "output_text") {
				textBuilder.WriteString(delta)
			}
		}
		return nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return nil, err
			}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if err := scanner.Err(); err != nil {
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
	encoded, err := json.Marshal(synthetic)
	if err != nil {
		return nil, err
	}
	return ResponsesToChat(encoded)
}

// tryResponsesJSONBody converts a non-SSE Responses JSON body when present.
func tryResponsesJSONBody(raw []byte) ([]byte, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return nil, false
	}
	// SSE frames always contain "event:" or "data:" prefixes; pure JSON does not.
	if bytes.Contains(trimmed, []byte("\nevent:")) || bytes.HasPrefix(trimmed, []byte("event:")) ||
		bytes.Contains(trimmed, []byte("\ndata:")) || bytes.HasPrefix(trimmed, []byte("data:")) {
		return nil, false
	}
	var envelope map[string]any
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return nil, false
	}
	// Accept both bare response objects and {type,response} wrappers.
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

// SetJSONStreamFlag forces the stream field on a JSON object payload.
func SetJSONStreamFlag(payload []byte, stream bool) ([]byte, error) {
	var input map[string]any
	if err := json.Unmarshal(payload, &input); err != nil {
		return nil, fmt.Errorf("decode json body: %w", err)
	}
	if existing, ok := input["stream"].(bool); ok && existing == stream {
		return payload, nil
	}
	input["stream"] = stream
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("encode json body: %w", err)
	}
	return encoded, nil
}

// ResponsesToChatStream converts Responses SSE into OpenAI Chat Completions SSE.
type ResponsesToChatStream struct {
	source  io.ReadCloser
	model   string
	reader  *bufio.Reader
	pending []byte
	once    sync.Once
	err     error
	done    bool
	started bool
	id      string
}

func NewResponsesToChatStream(source io.ReadCloser, model string) *ResponsesToChatStream {
	return &ResponsesToChatStream{
		source: source,
		model:  model,
		reader: bufio.NewReader(source),
		id:     "chatcmpl_" + randomID(20),
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
	eventName, data, err := readSSEEvent(s.reader)
	if err != nil {
		if err == io.EOF {
			if !s.done {
				s.queueChunk("", "stop")
				s.pending = append(s.pending, []byte("data: [DONE]\n\n")...)
				s.done = true
				return nil
			}
			return io.EOF
		}
		return err
	}
	if data == "[DONE]" {
		s.queueChunk("", "stop")
		s.pending = append(s.pending, []byte("data: [DONE]\n\n")...)
		s.done = true
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		return nil
	}
	typeName := stringValue(payload["type"])
	if typeName == "" {
		typeName = eventName
	}
	switch typeName {
	case "response.created", "response.in_progress":
		if !s.started {
			s.queueChunk("", "")
			s.started = true
		}
	case "response.output_text.delta":
		if !s.started {
			s.queueChunk("", "")
			s.started = true
		}
		s.queueChunk(stringValue(payload["delta"]), "")
	case "response.completed":
		s.queueChunk("", "stop")
		s.pending = append(s.pending, []byte("data: [DONE]\n\n")...)
		s.done = true
	}
	return nil
}

func (s *ResponsesToChatStream) queueChunk(delta string, finish string) {
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

func readSSEEvent(reader *bufio.Reader) (eventName string, data string, err error) {
	var dataLines []string
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil && len(line) == 0 {
			if len(dataLines) > 0 {
				return eventName, strings.Join(dataLines, "\n"), nil
			}
			return "", "", readErr
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if len(dataLines) == 0 && eventName == "" {
				if readErr != nil {
					return "", "", readErr
				}
				continue
			}
			return eventName, strings.Join(dataLines, "\n"), nil
		}
		if strings.HasPrefix(line, "event:") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
		if readErr != nil {
			return eventName, strings.Join(dataLines, "\n"), nil
		}
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
