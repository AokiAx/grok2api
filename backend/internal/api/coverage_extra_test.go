package api_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/backend/internal/api"
	"github.com/AokiAx/grok2api/backend/internal/service"
)

func TestResponsesNonStreamAggregatesCompletedEvent(t *testing.T) {
	sse := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_9\",\"model\":\"grok-4.5\",\"output_text\":\"done\"}}\n\n"
	gateway := &fakeGateway{requestResult: service.ChatResult{
		Status: http.StatusOK,
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Stream: io.NopCloser(strings.NewReader(sse)),
	}}
	server := api.NewServer(gateway, fakeStatus{}, "")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(
		rec,
		httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"grok-4.5","input":"hi","stream":false}`)),
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"id":"resp_9"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if !gateway.stream {
		t.Fatal("expected upstream stream=true for non-stream responses aggregation")
	}
}

func TestPanelMetaReportsAuthRequirement(t *testing.T) {
	server := api.NewServer(&fakeGateway{}, fakeStatus{}, "", api.WithAdmin(&fakeAdmin{}, "secret"))
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/api/panel-meta", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"auth_required":true`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestResponsesMapsChatShapedPayloadToInput(t *testing.T) {
	gateway := &fakeGateway{requestResult: service.ChatResult{
		Status: http.StatusOK,
		Header: make(http.Header),
		Body:   []byte(`{"id":"resp_1","object":"response"}`),
	}}
	server := api.NewServer(gateway, fakeStatus{}, "")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(
		rec,
		httptest.NewRequest(
			http.MethodPost,
			"/v1/responses",
			strings.NewReader(`{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}],"max_tokens":16,"stream":true}`),
		),
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := string(gateway.payload)
	if !strings.Contains(body, `"input"`) {
		t.Fatalf("expected input mapping, payload=%s", body)
	}
	if !strings.Contains(body, `"max_output_tokens"`) {
		t.Fatalf("expected max_output_tokens, payload=%s", body)
	}
}

func TestOptionsWiringPreferResponsesAndCatalog(t *testing.T) {
	gateway := &fakeGateway{result: service.ChatResult{
		Status: http.StatusOK,
		Header: make(http.Header),
		Body:   []byte(`{"object":"chat.completion","choices":[]}`),
	}}
	server := api.NewServer(
		gateway,
		fakeStatus{},
		"",
		api.WithPreferResponses(false),
		api.WithModelCatalog(nil),
	)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(
		rec,
		httptest.NewRequest(
			http.MethodPost,
			"/v1/chat/completions",
			strings.NewReader(`{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}]}`),
		),
	)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if len(gateway.payload) == 0 {
		t.Fatal("expected chat payload")
	}
}

func TestResponsesUpstreamErrorPassthrough(t *testing.T) {
	gateway := &fakeGateway{requestResult: service.ChatResult{
		Status: http.StatusUnprocessableEntity,
		Header: make(http.Header),
		Body:   []byte(`{"error":"invalid payload"}`),
	}}
	server := api.NewServer(gateway, fakeStatus{}, "")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(
		rec,
		httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"grok-4.5","input":"hi","stream":false}`)),
	)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestResponsesExpandsNamespaceTools(t *testing.T) {
	gateway := &fakeGateway{requestResult: service.ChatResult{
		Status: http.StatusOK,
		Header: make(http.Header),
		Body:   []byte(`{"id":"resp_1","object":"response"}`),
	}}
	server := api.NewServer(gateway, fakeStatus{}, "")
	rec := httptest.NewRecorder()
	body := `{"model":"grok-4.5","input":[{"role":"user","content":"hi"}],"tools":[{"type":"namespace","name":"demo","tools":[{"type":"function","name":"inner","parameters":{"type":"object"}}]}],"stream":true}`
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	forwarded := string(gateway.payload)
	if strings.Contains(forwarded, `"type":"namespace"`) {
		t.Fatalf("namespace should be expanded: %s", forwarded)
	}
	if !strings.Contains(forwarded, `"type":"function"`) || !strings.Contains(forwarded, `"name":"inner"`) {
		t.Fatalf("expected expanded function tool: %s", forwarded)
	}
}
