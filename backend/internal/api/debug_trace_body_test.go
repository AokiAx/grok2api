package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/backend/internal/domain/clientkey"
	"github.com/AokiAx/grok2api/backend/internal/intercept"
	"github.com/AokiAx/grok2api/backend/internal/service"
)

type singlePassBody struct {
	payload []byte
	reads   int
}

func (b *singlePassBody) Read(destination []byte) (int, error) {
	b.reads++
	if len(b.payload) == 0 {
		return 0, io.EOF
	}
	count := copy(destination, b.payload)
	b.payload = b.payload[count:]
	return count, io.EOF
}

func (*singlePassBody) Close() error { return nil }

func TestDebugTraceReusesSingleBoundedInferenceBody(t *testing.T) {
	dir := t.TempDir()
	tracer := intercept.New(intercept.Options{Enabled: true, Dir: dir, MaxBody: 4096})
	access := &orderedInferenceAccess{grant: service.ClientGrant{
		Authenticated: true,
		KeyID:         "ck_trace",
		Principal:     "client-key:ck_trace",
		ModelPolicy:   clientkey.ModelPolicyAll,
		MaxConcurrent: 1,
	}}
	payload := `{"model":"grok-4.5","input":"hi"}`
	body := &singlePassBody{payload: []byte(payload)}

	var downstreamBody io.ReadCloser
	handler := intercept.Middleware(tracer, ClientInferenceMiddleware(access, "grok-4.5", http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		access.events = append(access.events, "handler")
		downstreamBody = request.Body
		first, err := readInferenceRequestBody(writer, request)
		if err != nil {
			t.Fatalf("read cached body: %v", err)
		}
		second, err := readInferenceRequestBody(writer, request)
		if err != nil || string(second) != payload {
			t.Fatalf("reused body=%q err=%v", second, err)
		}
		if len(first) == 0 || &first[0] != &second[0] {
			t.Fatal("trace and inference pipeline did not share one cached byte slice")
		}
		writer.WriteHeader(http.StatusNoContent)
	})))

	request := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	request.Header.Set("Authorization", "Bearer trace-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if body.reads != 1 {
		t.Fatalf("original request body reads=%d want=1", body.reads)
	}
	if downstreamBody != body {
		t.Fatal("request body was rebuilt instead of reusing the shared bounded cache")
	}
	wantEvents := "authenticate,rpm,concurrency,body,handler,release"
	if got := strings.Join(access.events, ","); got != wantEvents {
		t.Fatalf("events=%s want=%s", got, wantEvents)
	}

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("trace entries=%d err=%v", len(entries), err)
	}
	trace, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read trace: %v", err)
	}
	if !strings.Contains(string(trace), `"model":"grok-4.5"`) {
		t.Fatalf("trace did not reuse cached request payload: %s", trace)
	}
}
