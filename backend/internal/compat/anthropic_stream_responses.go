package compat

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// NewResponsesToAnthropicStream converts a Grok Responses SSE body into
// Anthropic Messages SSE events without a Chat Completions intermediate hop.
func NewResponsesToAnthropicStream(source io.ReadCloser, requestModel string, thinkingEnabled bool, thinkingDisplay string) io.ReadCloser {
	reader, writer := io.Pipe()
	stream := &responsesAnthropicStream{reader: reader, source: source}
	go pipeResponsesToAnthropic(writer, source, requestModel, thinkingEnabled, thinkingDisplay)
	return stream
}

type responsesAnthropicStream struct {
	reader *io.PipeReader
	source io.ReadCloser
	once   sync.Once
}

func (s *responsesAnthropicStream) Read(buffer []byte) (int, error) {
	return s.reader.Read(buffer)
}

func (s *responsesAnthropicStream) Close() error {
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

func pipeResponsesToAnthropic(writer *io.PipeWriter, source io.ReadCloser, requestModel string, thinkingEnabled bool, thinkingDisplay string) {
	defer source.Close()
	tr := newAnthropicStreamTranslator(requestModel, thinkingEnabled, thinkingDisplay)
	write := func(event string, payload any) error {
		raw, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		var buf bytes.Buffer
		if event != "" {
			buf.WriteString("event: ")
			buf.WriteString(event)
			buf.WriteByte('\n')
		}
		buf.WriteString("data: ")
		buf.Write(raw)
		buf.WriteString("\n\n")
		_, err = writer.Write(buf.Bytes())
		return err
	}
	tr.write = write

	sc := bufio.NewScanner(source)
	buf := make([]byte, 0, 64*1024)
	// Large tool-call SSE lines (Claude Code file rewrites) can exceed 4MiB.
	sc.Buffer(buf, 32*1024*1024)
	var dataBuf []byte
	flush := func() error {
		if len(dataBuf) == 0 {
			return nil
		}
		payload := bytes.TrimSuffix(dataBuf, []byte("\n"))
		dataBuf = nil
		return tr.processData(payload)
	}
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			if err := flush(); err != nil {
				_ = tr.fail(err.Error())
				_ = writer.CloseWithError(err)
				return
			}
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if len(dataBuf) > 0 {
				dataBuf = append(dataBuf, '\n')
			}
			dataBuf = append(dataBuf, payload...)
		}
	}
	if err := sc.Err(); err != nil {
		if tr.started && !tr.finished {
			_ = tr.fail(err.Error())
		}
		_ = writer.CloseWithError(err)
		return
	}
	if err := flush(); err != nil {
		if tr.started && !tr.finished {
			_ = tr.fail(err.Error())
		}
		_ = writer.CloseWithError(err)
		return
	}
	if tr.started && !tr.finished {
		// Upstream closed without a terminal event — surface as error, not success stop.
		_ = tr.fail("upstream stream ended before a terminal event")
		_ = writer.Close()
		return
	}
	if !tr.started && !tr.finished {
		_ = tr.fail("upstream stream empty")
		_ = writer.Close()
		return
	}
	_ = writer.Close()
}

type anthropicStreamTranslator struct {
	requestModel      string
	model             string
	msgID             string
	thinkingEnabled   bool
	thinkingDisplay   string
	started           bool
	finished          bool
	failed            bool
	hasToolUse        bool
	usageIn           int
	usageOut          int
	blocks            map[string]*anthropicStreamBlock
	nextIndex         int
	thinkingBlock     *anthropicStreamBlock
	thinkingSignature string
	thinkingStopPend  bool
	currentReasoning  string
	completedReason   map[string]struct{}
	write             func(event string, payload any) error
}

type anthropicStreamBlock struct {
	Index   int
	Kind    string
	Emitted bool
	Stopped bool
}

func newAnthropicStreamTranslator(requestModel string, thinkingEnabled bool, thinkingDisplay string) *anthropicStreamTranslator {
	if thinkingDisplay == "" {
		thinkingDisplay = "summarized"
	}
	return &anthropicStreamTranslator{
		requestModel:    requestModel,
		model:           requestModel,
		thinkingEnabled: thinkingEnabled,
		thinkingDisplay: thinkingDisplay,
		blocks:          make(map[string]*anthropicStreamBlock),
		completedReason: make(map[string]struct{}),
	}
}

func (t *anthropicStreamTranslator) processData(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil
	}
	if bytes.Equal(data, []byte("[DONE]")) {
		// Bare [DONE] is not a Responses terminal event. Ignore it so a missing
		// response.completed/incomplete still surfaces as stream failure (see pipe exit).
		return nil
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("invalid JSON event: %w", err)
	}
	typ := strings.ToLower(strings.TrimSpace(stringValue(root["type"])))

	switch typ {
	case "response.created", "response.in_progress":
		id, model := "", t.requestModel
		if response, ok := root["response"].(map[string]any); ok {
			id = stringValue(response["id"])
			if m := stringValue(response["model"]); m != "" && model == "" {
				model = m
			}
			if usage, ok := response["usage"].(map[string]any); ok {
				if v := intValue(usage["input_tokens"]); v > 0 {
					t.usageIn = v
				}
			}
		}
		if t.requestModel != "" {
			model = t.requestModel
		}
		return t.ensureStart(id, model)

	case "response.output_text.delta":
		if err := t.ensureStart(t.msgID, t.model); err != nil {
			return err
		}
		return t.textDelta(streamEventKey(root), stringValue(root["delta"]))

	case "response.reasoning_summary_part.added":
		if err := t.ensureStart(t.msgID, t.model); err != nil {
			return err
		}
		return t.startThinkingPart(reasoningEventKey(root))

	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if err := t.ensureStart(t.msgID, t.model); err != nil {
			return err
		}
		if t.thinkingBlock == nil {
			if err := t.startThinkingPart(reasoningEventKey(root)); err != nil {
				return err
			}
		}
		return t.thinkingDelta(stringValue(root["delta"]))

	case "response.reasoning_summary_part.done":
		t.thinkingStopPend = true
		return nil

	case "response.output_item.added":
		if err := t.ensureStart(t.msgID, t.model); err != nil {
			return err
		}
		item, _ := root["item"].(map[string]any)
		switch strings.ToLower(stringValue(item["type"])) {
		case "reasoning":
			key := normalizeReasoningKey(firstNonEmptyString(item["id"], reasoningEventKey(root)))
			t.currentReasoning = key
			t.captureThinkingSignature(key, stringValue(item["encrypted_content"]))
		case "function_call":
			callID := firstNonEmptyString(item["call_id"], item["id"])
			key := streamItemKey(stringValue(item["id"]), callID, streamEventKey(root))
			if _, err := t.ensureToolBlock(key, callID, stringValue(item["name"])); err != nil {
				return err
			}
			if args := stringValue(item["arguments"]); args != "" {
				return t.toolArgsDelta(key, callID, stringValue(item["name"]), args)
			}
		}
		return nil

	case "response.function_call_arguments.delta":
		if err := t.ensureStart(t.msgID, t.model); err != nil {
			return err
		}
		callID := firstNonEmptyString(root["call_id"], root["item_id"])
		key := streamItemKey(stringValue(root["item_id"]), callID, streamEventKey(root))
		return t.toolArgsDelta(key, callID, stringValue(root["name"]), stringValue(root["delta"]))

	case "response.function_call_arguments.done":
		if err := t.ensureStart(t.msgID, t.model); err != nil {
			return err
		}
		callID := firstNonEmptyString(root["call_id"], root["item_id"])
		key := streamItemKey(stringValue(root["item_id"]), callID, streamEventKey(root))
		block, err := t.ensureToolBlock(key, callID, stringValue(root["name"]))
		if err != nil {
			return err
		}
		if !block.Emitted {
			if args := stringValue(root["arguments"]); args != "" {
				if err := t.toolArgsDelta(key, callID, stringValue(root["name"]), args); err != nil {
					return err
				}
			}
		}
		return t.stopBlock(key, "tool")

	case "response.output_item.done":
		item, _ := root["item"].(map[string]any)
		switch strings.ToLower(stringValue(item["type"])) {
		case "reasoning":
			key := normalizeReasoningKey(firstNonEmptyString(item["id"], reasoningEventKey(root)))
			t.currentReasoning = key
			t.captureThinkingSignature(key, stringValue(item["encrypted_content"]))
			if t.thinkingBlock == nil {
				return t.emitFinalReasoningItem(item, key)
			}
			return t.finalizeThinkingBlock(true)
		case "function_call":
			if err := t.ensureStart(t.msgID, t.model); err != nil {
				return err
			}
			callID := firstNonEmptyString(item["call_id"], item["id"])
			key := streamItemKey(stringValue(item["id"]), callID, streamEventKey(root))
			block, err := t.ensureToolBlock(key, callID, stringValue(item["name"]))
			if err != nil {
				return err
			}
			if !block.Emitted {
				if args := stringValue(item["arguments"]); args != "" {
					if err := t.toolArgsDelta(key, callID, stringValue(item["name"]), args); err != nil {
						return err
					}
				}
			}
			return t.stopBlock(key, "tool")
		case "message":
			key := streamItemKey(stringValue(item["id"]), streamEventKey(root))
			block := t.blocks[normalizeBlockKey("text", key)]
			if block != nil && block.Emitted {
				return t.stopBlock(key, "text")
			}
			for _, bl := range extractAnthropicTextBlocks(item["content"]) {
				if m, ok := bl.(map[string]any); ok {
					if err := t.textDelta(key, stringValue(m["text"])); err != nil {
						return err
					}
				}
			}
			return t.stopBlock(key, "text")
		}
		return nil

	case "response.completed", "response.incomplete":
		stop := "end_turn"
		usageIn, usageOut := t.usageIn, t.usageOut
		if response, ok := root["response"].(map[string]any); ok {
			if id := stringValue(response["id"]); id != "" && t.msgID == "" {
				_ = t.ensureStart(id, t.requestModel)
			} else {
				_ = t.ensureStart(t.msgID, t.model)
			}
			if usage, ok := response["usage"].(map[string]any); ok {
				usageIn = intValue(firstNonNil(usage["input_tokens"], usage["prompt_tokens"]))
				usageOut = intValue(firstNonNil(usage["output_tokens"], usage["completion_tokens"]))
				t.usageIn, t.usageOut = usageIn, usageOut
			}
			if output, ok := response["output"].([]any); ok {
				if err := t.emitFinalOutput(output); err != nil {
					return err
				}
			}
		} else {
			_ = t.ensureStart(t.msgID, t.model)
		}
		if t.hasToolUse {
			stop = "tool_use"
		} else if typ == "response.incomplete" {
			stop = "max_tokens"
		}
		return t.finish(stop, usageIn, usageOut)

	case "response.failed", "error":
		msg := stringValue(root["message"])
		if msg == "" {
			if errObj, ok := root["error"].(map[string]any); ok {
				msg = stringValue(errObj["message"])
			}
		}
		if msg == "" {
			if response, ok := root["response"].(map[string]any); ok {
				if errObj, ok := response["error"].(map[string]any); ok {
					msg = stringValue(errObj["message"])
				}
			}
		}
		if msg == "" {
			msg = "upstream response failed"
		}
		return t.fail(msg)

	default:
		return nil
	}
}

func (t *anthropicStreamTranslator) ensureStart(id, model string) error {
	if t.started {
		return nil
	}
	if id == "" {
		id = "msg_" + randomID(24)
	}
	if strings.HasPrefix(id, "resp_") {
		id = "msg_" + strings.TrimPrefix(id, "resp_")
	}
	if model == "" {
		model = t.requestModel
	}
	if model == "" {
		model = t.model
	}
	if model == "" {
		model = "grok"
	}
	t.msgID = id
	t.model = model
	t.started = true
	return t.write("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            id,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  t.usageIn,
				"output_tokens": 0,
			},
		},
	})
}

func (t *anthropicStreamTranslator) ensureTextBlock(key string) (*anthropicStreamBlock, error) {
	if err := t.finishThinkingBeforeContent(); err != nil {
		return nil, err
	}
	key = normalizeBlockKey("text", key)
	if block := t.blocks[key]; block != nil {
		return block, nil
	}
	block := &anthropicStreamBlock{Index: t.nextIndex, Kind: "text"}
	t.nextIndex++
	if err := t.write("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": block.Index,
		"content_block": map[string]any{
			"type": "text",
			"text": "",
		},
	}); err != nil {
		return nil, err
	}
	t.blocks[key] = block
	return block, nil
}

func (t *anthropicStreamTranslator) ensureToolBlock(key, callID, name string) (*anthropicStreamBlock, error) {
	if err := t.finishThinkingBeforeContent(); err != nil {
		return nil, err
	}
	key = normalizeBlockKey("tool", firstNonEmptyString(key, callID))
	if block := t.blocks[key]; block != nil {
		return block, nil
	}
	if callID == "" {
		callID = strings.TrimPrefix(key, "tool:")
	}
	if name == "" {
		name = "tool"
	}
	block := &anthropicStreamBlock{Index: t.nextIndex, Kind: "tool"}
	t.nextIndex++
	if err := t.write("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": block.Index,
		"content_block": map[string]any{
			"type":  "tool_use",
			"id":    callID,
			"name":  name,
			"input": map[string]any{},
		},
	}); err != nil {
		return nil, err
	}
	t.blocks[key] = block
	t.hasToolUse = true
	return block, nil
}

func (t *anthropicStreamTranslator) textDelta(key, text string) error {
	if text == "" {
		return nil
	}
	block, err := t.ensureTextBlock(key)
	if err != nil || block == nil || block.Stopped {
		return err
	}
	if err := t.write("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": block.Index,
		"delta": map[string]any{"type": "text_delta", "text": text},
	}); err != nil {
		return err
	}
	block.Emitted = true
	return nil
}

func (t *anthropicStreamTranslator) toolArgsDelta(key, callID, name, args string) error {
	if args == "" {
		return nil
	}
	block, err := t.ensureToolBlock(key, callID, name)
	if err != nil || block == nil || block.Stopped {
		return err
	}
	if err := t.write("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": block.Index,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
	}); err != nil {
		return err
	}
	block.Emitted = true
	return nil
}

func (t *anthropicStreamTranslator) stopBlock(key, kind string) error {
	key = normalizeBlockKey(kind, key)
	block := t.blocks[key]
	if block == nil || block.Stopped {
		return nil
	}
	if err := t.write("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": block.Index,
	}); err != nil {
		return err
	}
	block.Stopped = true
	return nil
}

func (t *anthropicStreamTranslator) stopAllBlocks() error {
	blocks := make([]*anthropicStreamBlock, 0, len(t.blocks))
	for _, block := range t.blocks {
		if !block.Stopped {
			blocks = append(blocks, block)
		}
	}
	sort.Slice(blocks, func(i, j int) bool { return blocks[i].Index < blocks[j].Index })
	for _, block := range blocks {
		if err := t.write("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": block.Index,
		}); err != nil {
			return err
		}
		block.Stopped = true
	}
	return nil
}

func (t *anthropicStreamTranslator) startThinkingPart(key string) error {
	if !t.thinkingEnabled {
		return nil
	}
	if t.thinkingBlock != nil && t.thinkingStopPend {
		if err := t.finalizeThinkingBlock(false); err != nil {
			return err
		}
	}
	t.currentReasoning = normalizeReasoningKey(key)
	_, err := t.ensureThinkingBlock()
	return err
}

func (t *anthropicStreamTranslator) ensureThinkingBlock() (*anthropicStreamBlock, error) {
	if !t.thinkingEnabled {
		return nil, nil
	}
	if t.thinkingBlock != nil {
		return t.thinkingBlock, nil
	}
	if err := t.ensureStart(t.msgID, t.model); err != nil {
		return nil, err
	}
	block := &anthropicStreamBlock{Index: t.nextIndex, Kind: "thinking"}
	t.nextIndex++
	if err := t.write("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": block.Index,
		"content_block": map[string]any{
			"type":     "thinking",
			"thinking": "",
		},
	}); err != nil {
		return nil, err
	}
	t.thinkingBlock = block
	t.thinkingStopPend = false
	return block, nil
}

func (t *anthropicStreamTranslator) thinkingDelta(text string) error {
	if !t.thinkingEnabled || t.thinkingDisplay == "omitted" || text == "" {
		return nil
	}
	block, err := t.ensureThinkingBlock()
	if err != nil || block == nil {
		return err
	}
	if err := t.write("content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": block.Index,
		"delta": map[string]any{"type": "thinking_delta", "thinking": text},
	}); err != nil {
		return err
	}
	block.Emitted = true
	return nil
}

func (t *anthropicStreamTranslator) captureThinkingSignature(key, signature string) {
	if !t.thinkingEnabled || signature == "" {
		return
	}
	if key = strings.TrimSpace(key); key != "" {
		t.currentReasoning = normalizeReasoningKey(key)
	}
	t.thinkingSignature = signature
}

func (t *anthropicStreamTranslator) finalizeThinkingBlock(clearSignature bool) error {
	if !t.thinkingEnabled {
		return nil
	}
	if t.thinkingBlock == nil {
		if clearSignature {
			t.thinkingSignature = ""
		}
		return nil
	}
	block := t.thinkingBlock
	if t.thinkingSignature != "" {
		if err := t.write("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": block.Index,
			"delta": map[string]any{"type": "signature_delta", "signature": t.thinkingSignature},
		}); err != nil {
			return err
		}
	}
	if err := t.write("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": block.Index,
	}); err != nil {
		return err
	}
	if t.currentReasoning != "" {
		t.completedReason[t.currentReasoning] = struct{}{}
	}
	t.thinkingBlock = nil
	t.thinkingStopPend = false
	if clearSignature {
		t.thinkingSignature = ""
		t.currentReasoning = ""
	}
	return nil
}

func (t *anthropicStreamTranslator) finishThinkingBeforeContent() error {
	if t.thinkingBlock == nil {
		return nil
	}
	return t.finalizeThinkingBlock(true)
}

func (t *anthropicStreamTranslator) emitFinalReasoningItem(item map[string]any, fallbackKey string) error {
	if !t.thinkingEnabled {
		return nil
	}
	key := normalizeReasoningKey(firstNonEmptyString(item["id"], fallbackKey))
	if _, done := t.completedReason[key]; done {
		return nil
	}
	summary := extractReasoningSummaryText(item["summary"])
	if t.thinkingDisplay == "omitted" {
		summary = ""
	}
	signature := stringValue(item["encrypted_content"])
	if signature == "" {
		signature = t.thinkingSignature
	}
	if summary == "" && signature == "" {
		return nil
	}
	t.currentReasoning = key
	if signature != "" {
		t.thinkingSignature = signature
	}
	if _, err := t.ensureThinkingBlock(); err != nil {
		return err
	}
	if err := t.thinkingDelta(summary); err != nil {
		return err
	}
	return t.finalizeThinkingBlock(true)
}

func (t *anthropicStreamTranslator) emitFinalOutput(items []any) error {
	for i, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch strings.ToLower(stringValue(item["type"])) {
		case "reasoning":
			if err := t.emitFinalReasoningItem(item, fmt.Sprintf("final-reasoning-%d", i)); err != nil {
				return err
			}
		case "message":
			key := streamItemKey(stringValue(item["id"]), fmt.Sprintf("final-%d", i))
			block := t.blocks[normalizeBlockKey("text", key)]
			if block == nil && t.hasEmittedKind("text") {
				continue
			}
			if block != nil && block.Emitted {
				if err := t.stopBlock(key, "text"); err != nil {
					return err
				}
				continue
			}
			for _, bl := range extractAnthropicTextBlocks(item["content"]) {
				if m, ok := bl.(map[string]any); ok {
					if err := t.textDelta(key, stringValue(m["text"])); err != nil {
						return err
					}
				}
			}
			if err := t.stopBlock(key, "text"); err != nil {
				return err
			}
		case "function_call":
			callID := firstNonEmptyString(item["call_id"], item["id"])
			key := streamItemKey(stringValue(item["id"]), callID, fmt.Sprintf("final-%d", i))
			block, err := t.ensureToolBlock(key, callID, stringValue(item["name"]))
			if err != nil {
				return err
			}
			if !block.Emitted {
				args := stringValue(item["arguments"])
				if args == "" {
					args = "{}"
				}
				if err := t.toolArgsDelta(key, callID, stringValue(item["name"]), args); err != nil {
					return err
				}
			}
			if err := t.stopBlock(key, "tool"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *anthropicStreamTranslator) hasEmittedKind(kind string) bool {
	for _, block := range t.blocks {
		if block.Kind == kind && block.Emitted {
			return true
		}
	}
	return false
}

func (t *anthropicStreamTranslator) finish(stopReason string, usageIn, usageOut int) error {
	if t.finished {
		return nil
	}
	if err := t.ensureStart(t.msgID, t.model); err != nil {
		return err
	}
	if err := t.finalizeThinkingBlock(true); err != nil {
		return err
	}
	if err := t.stopAllBlocks(); err != nil {
		return err
	}
	if stopReason == "" {
		if t.hasToolUse {
			stopReason = "tool_use"
		} else {
			stopReason = "end_turn"
		}
	}
	if err := t.write("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"input_tokens":  usageIn,
			"output_tokens": usageOut,
		},
	}); err != nil {
		return err
	}
	if err := t.write("message_stop", map[string]any{"type": "message_stop"}); err != nil {
		return err
	}
	t.finished = true
	return nil
}

func (t *anthropicStreamTranslator) fail(message string) error {
	if t.finished {
		return nil
	}
	_ = t.finalizeThinkingBlock(true)
	_ = t.stopAllBlocks()
	// Anthropic error events — never emit message_stop after failure.
	_ = t.write("error", map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "api_error",
			"message": message,
		},
	})
	t.failed = true
	t.finished = true
	return nil
}

func normalizeBlockKey(kind, key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		key = "default"
	}
	prefix := kind + ":"
	if strings.HasPrefix(key, prefix) {
		return key
	}
	return prefix + key
}

func normalizeReasoningKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return "reasoning:default"
	}
	if strings.HasPrefix(key, "reasoning:") {
		return key
	}
	return "reasoning:" + key
}

func streamEventKey(root map[string]any) string {
	return streamItemKey(
		stringValue(root["item_id"]),
		stringValue(root["output_index"]),
		stringValue(root["content_index"]),
	)
}

func streamItemKey(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return "default"
}

func reasoningEventKey(root map[string]any) string {
	return normalizeReasoningKey(firstNonEmptyString(root["item_id"], root["output_index"]))
}
