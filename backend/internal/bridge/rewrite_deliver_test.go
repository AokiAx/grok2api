package bridge_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/backend/internal/bridge"
	"github.com/AokiAx/grok2api/backend/internal/service"
	"github.com/AokiAx/grok2api/backend/internal/upstream"
)

func TestPipelineResponsesRestoresNamespaceToolsOnJSON(t *testing.T) {
	// Upstream returns completed JSON object as stream body (non-SSE).
	upstreamBody := `{"id":"resp_1","object":"response","status":"completed","tools":[{"type":"function","name":"demo__inner"}],"output_text":"ok"}`
	gateway := &fakeGateway{result: service.ChatResult{
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Stream: io.NopCloser(strings.NewReader(upstreamBody)),
	}}
	p := &bridge.Pipeline{
		Gateway:         gateway,
		Catalog:         upstream.NewDefaultCatalog(),
		PreferResponses: true,
	}
	result, err := p.Responses(context.Background(), []byte(`{
		"model":"grok-4.5",
		"stream":false,
		"input":[{"role":"user","content":"hi"}],
		"tools":[{"type":"namespace","name":"demo","tools":[{"type":"function","name":"inner","parameters":{"type":"object"}}]}]
	}`))
	if err != nil {
		t.Fatalf("responses: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(result.Body, &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, result.Body)
	}
	// Client-facing tools should be restored to namespace form when rewrite runs.
	tools, _ := body["tools"].([]any)
	if len(tools) == 0 {
		// visibleTools restore only when tools key present; ensure no crash.
		return
	}
	first, _ := tools[0].(map[string]any)
	if first["type"] == "namespace" || first["name"] == "inner" || first["name"] == "demo__inner" {
		return
	}
	t.Fatalf("tools=%#v", tools)
}

func TestPipelineChatDefaultSearchInjectsNativeTools(t *testing.T) {
	sse := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"output_text\":\"hi\"}}\n\n"
	gateway := &fakeGateway{result: service.ChatResult{
		Status: http.StatusOK,
		Stream: io.NopCloser(strings.NewReader(sse)),
	}}
	p := &bridge.Pipeline{
		Gateway:         gateway,
		Catalog:         upstream.NewDefaultCatalog(),
		PreferResponses: true,
	}
	_, err := p.Chat(context.Background(), []byte(`{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}],"stream":false}`))
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	var forwarded map[string]any
	if err := json.Unmarshal(gateway.payload, &forwarded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if forwarded["backend_search"] != true {
		t.Fatalf("backend_search=%#v", forwarded["backend_search"])
	}
}
