package compat

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// SSEEvent is one Server-Sent Events frame from a Responses stream.
type SSEEvent struct {
	Name    string
	Data    string
	Payload map[string]any
	Type    string
}

// ReadSSEEvent reads the next SSE event from a buffered reader.
func ReadSSEEvent(reader *bufio.Reader) (SSEEvent, error) {
	eventName, data, err := readSSEEvent(reader)
	if err != nil {
		return SSEEvent{}, err
	}
	event := SSEEvent{Name: eventName, Data: data}
	if data != "" && data != "[DONE]" {
		var payload map[string]any
		if json.Unmarshal([]byte(data), &payload) == nil {
			event.Payload = payload
			event.Type = stringValue(payload["type"])
		}
	}
	if event.Type == "" {
		event.Type = eventName
	}
	return event, nil
}

// IterateSSEBytes walks an entire SSE document and invokes handle for each event.
func IterateSSEBytes(raw []byte, handle func(SSEEvent) error) error {
	reader := bufio.NewReader(bytes.NewReader(raw))
	for {
		event, err := ReadSSEEvent(reader)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if err := handle(event); err != nil {
			return err
		}
	}
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
