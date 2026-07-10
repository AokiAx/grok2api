package compat_test

import (
	"io"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/internal/compat"
)

func TestResponsesToChatStreamConvertsDeltas(t *testing.T) {
	sse := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created"}`,
		``,
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"hi"}`,
		``,
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","output_text":"hi"}}`,
		``,
	}, "\n")
	stream := compat.NewResponsesToChatStream(io.NopCloser(strings.NewReader(sse)), "grok-4.5")
	data, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	_ = stream.Close()
	body := string(data)
	if !strings.Contains(body, `"content":"hi"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("body=%s", body)
	}
}
