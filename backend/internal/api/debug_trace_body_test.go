package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/backend/internal/domain/clientkey"
	"github.com/AokiAx/grok2api/backend/internal/intercept"
	"github.com/AokiAx/grok2api/backend/internal/repository"
	"github.com/AokiAx/grok2api/backend/internal/service"
)

type singlePassBody struct {
	payload []byte
	reads   int
	events  *[]string
	seen    bool
}

func (b *singlePassBody) Read(destination []byte) (int, error) {
	b.reads++
	if !b.seen && b.events != nil {
		*b.events = append(*b.events, "body")
		b.seen = true
	}
	if len(b.payload) == 0 {
		return 0, io.EOF
	}
	count := copy(destination, b.payload)
	b.payload = b.payload[count:]
	return count, io.EOF
}

func (*singlePassBody) Close() error { return nil }

type rejectingTraceAccess struct {
	authErr error
	rpmErr  error
}

func (a *rejectingTraceAccess) Authenticate(context.Context, string) (service.ClientGrant, error) {
	return service.ClientGrant{
		Authenticated: true,
		KeyID:         "ck_rejected",
		Principal:     "client-key:ck_rejected",
		ModelPolicy:   clientkey.ModelPolicyAll,
		MaxConcurrent: 1,
	}, a.authErr
}

func (a *rejectingTraceAccess) ConsumeRPM(context.Context, service.ClientGrant) (repository.RateLimitDecision, error) {
	return repository.RateLimitDecision{Allowed: a.rpmErr == nil}, a.rpmErr
}

func (*rejectingTraceAccess) AcquireConcurrency(service.ClientGrant) (service.ClientPermit, error) {
	return nil, nil
}

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
	body := &singlePassBody{payload: []byte(payload), events: &access.events}

	handler := intercept.Middleware(tracer, ClientInferenceMiddleware(access, "grok-4.5", http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		access.events = append(access.events, "handler")
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

func TestDebugTraceDoesNotReadRejectedInferenceBody(t *testing.T) {
	tests := []struct {
		name       string
		access     *rejectingTraceAccess
		wantStatus int
	}{
		{name: "unauthorized", access: &rejectingTraceAccess{authErr: service.ErrClientUnauthorized}, wantStatus: http.StatusUnauthorized},
		{name: "rate limited", access: &rejectingTraceAccess{rpmErr: service.ErrClientRateLimited}, wantStatus: http.StatusTooManyRequests},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			tracer := intercept.New(intercept.Options{Enabled: true, Dir: dir, MaxBody: 4096})
			body := &singlePassBody{payload: []byte(`{"model":"grok-4.5"}`)}
			handler := intercept.Middleware(tracer, ClientInferenceMiddleware(test.access, "grok-4.5", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("rejected request reached handler")
			})))
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
			request.Header.Set("Authorization", "Bearer rejected-key")
			recorder := httptest.NewRecorder()

			handler.ServeHTTP(recorder, request)

			if recorder.Code != test.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", recorder.Code, test.wantStatus, recorder.Body.String())
			}
			if body.reads != 0 {
				t.Fatalf("rejected request body reads=%d want=0", body.reads)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 {
				t.Fatalf("trace entries=%d err=%v", len(entries), err)
			}
			trace, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
			if err != nil {
				t.Fatalf("read trace: %v", err)
			}
			text := string(trace)
			if !strings.Contains(text, `"body_available":false`) || !strings.Contains(text, `"bytes":0`) {
				t.Fatalf("rejected trace did not mark body unavailable: %s", text)
			}
		})
	}
}
