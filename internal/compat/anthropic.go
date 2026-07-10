package compat

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

func AnthropicToOpenAI(payload []byte, defaultModel string) ([]byte, bool, error) {
	var input map[string]any
	if err := json.Unmarshal(payload, &input); err != nil {
		return nil, false, fmt.Errorf("decode Anthropic request: %w", err)
	}

	model, _ := input["model"].(string)
	if strings.TrimSpace(model) == "" {
		model = defaultModel
	}
	stream, _ := input["stream"].(bool)
	messages := make([]any, 0)
	if system := systemText(input["system"]); system != "" {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}
	if rawMessages, ok := input["messages"].([]any); ok {
		for _, raw := range rawMessages {
			message, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			role, _ := message["role"].(string)
			if role == "" {
				role = "user"
			}
			messages = append(messages, contentToOpenAIMessages(role, message["content"])...)
		}
	}

	output := map[string]any{
		"model":    model,
		"messages": messages,
		"stream":   stream,
	}
	copyFields(output, input, "max_tokens", "temperature", "top_p")
	if stops, ok := input["stop_sequences"].([]any); ok && len(stops) > 0 {
		output["stop"] = stops
	}
	if tools := anthropicTools(input["tools"]); len(tools) > 0 {
		output["tools"] = tools
	}
	if choice := anthropicToolChoice(input["tool_choice"]); choice != nil {
		output["tool_choice"] = choice
	}

	encoded, err := json.Marshal(output)
	if err != nil {
		return nil, false, fmt.Errorf("encode OpenAI request: %w", err)
	}
	return encoded, stream, nil
}

func OpenAIToAnthropic(payload []byte) ([]byte, error) {
	var completion map[string]any
	if err := json.Unmarshal(payload, &completion); err != nil {
		return nil, fmt.Errorf("decode OpenAI response: %w", err)
	}
	model, _ := completion["model"].(string)
	if model == "" {
		model = "grok"
	}
	content := make([]any, 0)
	stopReason := "end_turn"
	if choices, ok := completion["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if message, ok := choice["message"].(map[string]any); ok {
				if text, ok := message["content"].(string); ok && text != "" {
					content = append(content, map[string]any{"type": "text", "text": text})
				}
				if calls, ok := message["tool_calls"].([]any); ok {
					for _, rawCall := range calls {
						call, ok := rawCall.(map[string]any)
						if !ok {
							continue
						}
						function, _ := call["function"].(map[string]any)
						arguments := parseToolArguments(function["arguments"])
						id, _ := call["id"].(string)
						if id == "" {
							id = "toolu_" + randomID(12)
						}
						name, _ := function["name"].(string)
						content = append(content, map[string]any{
							"type":  "tool_use",
							"id":    id,
							"name":  name,
							"input": arguments,
						})
					}
				}
			}
			switch choice["finish_reason"] {
			case "length":
				stopReason = "max_tokens"
			case "tool_calls":
				stopReason = "tool_use"
			}
		}
	}
	if len(content) == 0 {
		content = append(content, map[string]any{"type": "text", "text": ""})
	}
	inputTokens, outputTokens := tokenUsage(completion["usage"])
	output := map[string]any{
		"id":            "msg_" + randomID(24),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
		},
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return nil, fmt.Errorf("encode Anthropic response: %w", err)
	}
	return encoded, nil
}

func systemText(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	parts, ok := value.([]any)
	if !ok {
		return ""
	}
	texts := make([]string, 0, len(parts))
	for _, raw := range parts {
		switch part := raw.(type) {
		case string:
			texts = append(texts, part)
		case map[string]any:
			if part["type"] == "text" {
				texts = append(texts, stringValue(part["text"]))
			}
		}
	}
	return strings.Join(texts, "\n")
}

func contentToOpenAIMessages(role string, content any) []any {
	parts, ok := content.([]any)
	if !ok {
		return []any{map[string]any{"role": role, "content": content}}
	}
	textParts := make([]string, 0)
	toolCalls := make([]any, 0)
	output := make([]any, 0)
	flushAssistant := func() {
		if len(textParts) == 0 && len(toolCalls) == 0 {
			return
		}
		var text any
		if len(textParts) > 0 {
			text = strings.Join(textParts, "")
		}
		message := map[string]any{"role": "assistant", "content": text}
		if len(toolCalls) > 0 {
			message["tool_calls"] = toolCalls
		}
		output = append(output, message)
		textParts = nil
		toolCalls = nil
	}

	for _, raw := range parts {
		if text, ok := raw.(string); ok {
			textParts = append(textParts, text)
			continue
		}
		part, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typeName, _ := part["type"].(string)
		switch typeName {
		case "", "text":
			textParts = append(textParts, stringValue(part["text"]))
		case "tool_use":
			id, _ := part["id"].(string)
			if id == "" {
				id = "call_" + randomID(12)
			}
			arguments, err := json.Marshal(defaultObject(part["input"]))
			if err != nil {
				arguments = []byte("{}")
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      stringValue(part["name"]),
					"arguments": string(arguments),
				},
			})
		case "tool_result":
			flushAssistant()
			toolCallID := fmt.Sprint(firstValue(part, "tool_use_id", "id"))
			output = append(output, map[string]any{
				"role":         "tool",
				"tool_call_id": toolCallID,
				"content":      toolResultText(part["content"]),
			})
		default:
			encoded, err := json.Marshal(part)
			if err == nil {
				textParts = append(textParts, string(encoded))
			}
		}
	}
	if role == "assistant" {
		flushAssistant()
	} else if role == "user" {
		if len(textParts) > 0 {
			output = append(output, map[string]any{"role": "user", "content": strings.Join(textParts, "")})
		} else if len(output) == 0 {
			output = append(output, map[string]any{"role": "user", "content": ""})
		}
	} else if len(output) == 0 {
		output = append(output, map[string]any{"role": role, "content": toolResultText(parts)})
	}
	return output
}

func anthropicTools(value any) []any {
	rawTools, ok := value.([]any)
	if !ok {
		return nil
	}
	tools := make([]any, 0, len(rawTools))
	for _, raw := range rawTools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if tool["type"] == "function" {
			if _, ok := tool["function"].(map[string]any); ok {
				tools = append(tools, tool)
				continue
			}
		}
		name, _ := tool["name"].(string)
		if name == "" {
			continue
		}
		parameters := firstValue(tool, "input_schema", "parameters")
		if parameters == nil {
			parameters = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": stringValue(tool["description"]),
				"parameters":  parameters,
			},
		})
	}
	return tools
}

func anthropicToolChoice(value any) any {
	switch choice := value.(type) {
	case string:
		if choice == "any" {
			return "required"
		}
		return choice
	case map[string]any:
		typeName, _ := choice["type"].(string)
		switch typeName {
		case "any":
			return "required"
		case "auto", "none":
			return typeName
		case "tool":
			if name, ok := choice["name"].(string); ok && name != "" {
				return map[string]any{"type": "function", "function": map[string]any{"name": name}}
			}
		case "function":
			return choice
		}
	}
	return nil
}

func toolResultText(value any) string {
	switch content := value.(type) {
	case nil:
		return ""
	case string:
		return content
	case []any:
		texts := make([]string, 0, len(content))
		for _, raw := range content {
			switch part := raw.(type) {
			case string:
				texts = append(texts, part)
			case map[string]any:
				if part["type"] == nil || part["type"] == "text" {
					texts = append(texts, stringValue(part["text"]))
				}
			}
		}
		return strings.Join(texts, "")
	default:
		encoded, err := json.Marshal(content)
		if err != nil {
			return fmt.Sprint(content)
		}
		return string(encoded)
	}
}

func parseToolArguments(value any) map[string]any {
	switch arguments := value.(type) {
	case map[string]any:
		return arguments
	case string:
		var parsed any
		if err := json.Unmarshal([]byte(arguments), &parsed); err != nil {
			return map[string]any{"_raw": arguments}
		}
		if object, ok := parsed.(map[string]any); ok {
			return object
		}
		return map[string]any{"value": parsed}
	default:
		return map[string]any{}
	}
}

func tokenUsage(value any) (any, any) {
	usage, _ := value.(map[string]any)
	input := usage["prompt_tokens"]
	if input == nil {
		input = 0
	}
	output := usage["completion_tokens"]
	if output == nil {
		output = 0
	}
	return input, output
}

func copyFields(target, source map[string]any, fields ...string) {
	for _, field := range fields {
		if value, ok := source[field]; ok && value != nil {
			target[field] = value
		}
	}
}

func firstValue(values map[string]any, keys ...string) any {
	for _, key := range keys {
		if value, ok := values[key]; ok && value != nil && fmt.Sprint(value) != "" {
			return value
		}
	}
	return nil
}

func defaultObject(value any) any {
	if value == nil {
		return map[string]any{}
	}
	return value
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func randomID(length int) string {
	buffer := make([]byte, (length+1)/2)
	if _, err := rand.Read(buffer); err != nil {
		return strings.Repeat("0", length)
	}
	return hex.EncodeToString(buffer)[:length]
}

type anthropicStream struct {
	reader *io.PipeReader
	source io.ReadCloser
	once   sync.Once
}

func NewAnthropicStream(source io.ReadCloser, model string) io.ReadCloser {
	reader, writer := io.Pipe()
	stream := &anthropicStream{reader: reader, source: source}
	go convertAnthropicStream(writer, source, model)
	return stream
}

func (s *anthropicStream) Read(buffer []byte) (int, error) {
	return s.reader.Read(buffer)
}

func (s *anthropicStream) Close() error {
	var closeErr error
	s.once.Do(func() {
		readerErr := s.reader.Close()
		sourceErr := s.source.Close()
		if readerErr != nil {
			closeErr = readerErr
		} else {
			closeErr = sourceErr
		}
	})
	return closeErr
}

func convertAnthropicStream(writer *io.PipeWriter, source io.ReadCloser, model string) {
	defer source.Close()
	converter := newStreamConverter(model)
	writeEvents := func(events []map[string]any) error {
		for _, event := range events {
			encoded, err := json.Marshal(event)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(writer, "event: %s\ndata: %s\n\n", event["type"], encoded); err != nil {
				return err
			}
		}
		return nil
	}
	if err := writeEvents(converter.prelude()); err != nil {
		_ = writer.CloseWithError(err)
		return
	}
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			if err := writeEvents(converter.finish()); err != nil {
				_ = writer.CloseWithError(err)
				return
			}
			_ = writer.Close()
			return
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if err := writeEvents(converter.feed(chunk)); err != nil {
			_ = writer.CloseWithError(err)
			return
		}
	}
	if err := scanner.Err(); err != nil {
		_ = writer.CloseWithError(err)
		return
	}
	if err := writeEvents(converter.finish()); err != nil {
		_ = writer.CloseWithError(err)
		return
	}
	_ = writer.Close()
}

type streamConverter struct {
	model          string
	toolBlocks     map[int]struct{}
	blocksClosed   bool
	messageStopped bool
}

func newStreamConverter(model string) *streamConverter {
	return &streamConverter{model: model, toolBlocks: make(map[int]struct{})}
}

func (c *streamConverter) prelude() []map[string]any {
	return []map[string]any{
		{
			"type": "message_start",
			"message": map[string]any{
				"id": "msg_" + randomID(24), "type": "message", "role": "assistant",
				"model": c.model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		},
		{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}},
		{"type": "ping"},
	}
}

func (c *streamConverter) feed(chunk map[string]any) []map[string]any {
	choices, _ := chunk["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	choice, _ := choices[0].(map[string]any)
	delta, _ := choice["delta"].(map[string]any)
	events := make([]map[string]any, 0)
	if text, ok := delta["content"].(string); ok && text != "" {
		events = append(events, map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": text},
		})
	}
	if calls, ok := delta["tool_calls"].([]any); ok {
		for _, raw := range calls {
			call, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			index := numberToInt(call["index"]) + 1
			function, _ := call["function"].(map[string]any)
			if _, exists := c.toolBlocks[index]; !exists {
				c.toolBlocks[index] = struct{}{}
				id, _ := call["id"].(string)
				if id == "" {
					id = fmt.Sprintf("toolu_%d", index)
				}
				name, _ := function["name"].(string)
				events = append(events, map[string]any{
					"type": "content_block_start", "index": index,
					"content_block": map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}},
				})
			}
			if arguments, ok := function["arguments"].(string); ok && arguments != "" {
				events = append(events, map[string]any{
					"type": "content_block_delta", "index": index,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": arguments},
				})
			}
		}
	}
	if reason, ok := choice["finish_reason"].(string); ok && reason != "" {
		events = append(events, c.closeBlocks(reason)...)
	}
	return events
}

func (c *streamConverter) closeBlocks(reason string) []map[string]any {
	if c.blocksClosed {
		return nil
	}
	c.blocksClosed = true
	events := []map[string]any{{"type": "content_block_stop", "index": 0}}
	indices := make([]int, 0, len(c.toolBlocks))
	for index := range c.toolBlocks {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	for _, index := range indices {
		events = append(events, map[string]any{"type": "content_block_stop", "index": index})
	}
	stopReason := "end_turn"
	if reason == "tool_calls" {
		stopReason = "tool_use"
	} else if reason == "length" {
		stopReason = "max_tokens"
	}
	events = append(events, map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": 0},
	})
	return events
}

func (c *streamConverter) finish() []map[string]any {
	events := c.closeBlocks("stop")
	if !c.messageStopped {
		c.messageStopped = true
		events = append(events, map[string]any{"type": "message_stop"})
	}
	return events
}

func numberToInt(value any) int {
	switch number := value.(type) {
	case float64:
		return int(number)
	case int:
		return number
	default:
		return 0
	}
}
