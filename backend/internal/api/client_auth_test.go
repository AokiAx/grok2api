package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/clientkey"
	"github.com/AokiAx/grok2api/backend/internal/repository"
	"github.com/AokiAx/grok2api/backend/internal/service"
)

type fakeRequestAuthenticator struct {
	secrets []string
	grants  map[string]service.ClientGrant
}

type orderedInferenceAccess struct {
	grant  service.ClientGrant
	events []string
	permit *orderedPermit
}

func (a *orderedInferenceAccess) Authenticate(context.Context, string) (service.ClientGrant, error) {
	a.events = append(a.events, "authenticate")
	return a.grant, nil
}

func (a *orderedInferenceAccess) ConsumeRPM(context.Context, service.ClientGrant) (repository.RateLimitDecision, error) {
	a.events = append(a.events, "rpm")
	return repository.RateLimitDecision{Allowed: true, Limit: 10, Remaining: 9}, nil
}

func (a *orderedInferenceAccess) AcquireConcurrency(service.ClientGrant) (service.ClientPermit, error) {
	a.events = append(a.events, "concurrency")
	a.permit = &orderedPermit{events: &a.events}
	return a.permit, nil
}

type orderedPermit struct {
	events   *[]string
	released bool
}

func (p *orderedPermit) Release() {
	if !p.released {
		*p.events = append(*p.events, "release")
		p.released = true
	}
}

func (f *fakeRequestAuthenticator) Authenticate(_ context.Context, secret string) (service.ClientGrant, error) {
	f.secrets = append(f.secrets, secret)
	grant, ok := f.grants[secret]
	if !ok {
		return service.ClientGrant{}, service.ErrClientUnauthorized
	}
	return grant, nil
}

func TestClientAuthMiddlewarePrefersBearerAndUsesUnified401(t *testing.T) {
	auth := &fakeRequestAuthenticator{grants: map[string]service.ClientGrant{
		"header-key": {Authenticated: true, KeyID: "ck_header", Principal: "client-key:ck_header"},
	}}
	nextCalled := false
	handler := ClientAuthMiddleware(auth, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		nextCalled = true
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.Header.Set("Authorization", "Bearer bad-bearer")
	req.Header.Set("x-api-key", "header-key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || nextCalled {
		t.Fatalf("status=%d next=%v body=%s", rec.Code, nextCalled, rec.Body.String())
	}
	if len(auth.secrets) != 1 || auth.secrets[0] != "bad-bearer" {
		t.Fatalf("selected secrets=%#v", auth.secrets)
	}
	if !strings.Contains(rec.Body.String(), `"code":"invalid_api_key"`) {
		t.Fatalf("unified error body=%s", rec.Body.String())
	}
}

func TestClientAuthMiddlewareCarriesGrantWithoutPlaintext(t *testing.T) {
	auth := &fakeRequestAuthenticator{grants: map[string]service.ClientGrant{
		"header-key": {Authenticated: true, KeyID: "ck_header", Principal: "client-key:ck_header"},
	}}
	var got service.ClientGrant
	handler := ClientAuthMiddleware(auth, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var ok bool
		got, ok = service.ClientGrantFromContext(request.Context())
		if !ok {
			t.Fatal("client grant missing from context")
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("x-api-key", "header-key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || got.Principal != "client-key:ck_header" || got.KeyID != "ck_header" {
		t.Fatalf("status=%d grant=%+v", rec.Code, got)
	}
	if got.Principal == "header-key" || strings.Contains(got.Principal, "header-key") {
		t.Fatal("context grant leaked plaintext key")
	}
}

func TestClientAuthMiddlewareMapsAllCredentialFailuresToSameResponse(t *testing.T) {
	auth := &fakeRequestAuthenticator{}
	handler := ClientAuthMiddleware(auth, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("unauthorized request reached next handler")
	}))
	for _, header := range []string{"", "unknown", "revoked", "expired"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		if header != "" {
			req.Header.Set("Authorization", "Bearer "+header)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"code":"invalid_api_key"`) {
			t.Fatalf("header=%q status=%d body=%s", header, rec.Code, rec.Body.String())
		}
	}
}

func TestClientModelAuthorizationAppliesDefaultBeforeScopeCheck(t *testing.T) {
	grant := service.ClientGrant{
		Authenticated: true, KeyID: "ck_limited", Principal: "client-key:ck_limited",
		ModelPolicy: clientkey.ModelPolicyAllowlist, ModelScopes: []string{"grok-4.5"},
	}
	nextCalled := false
	handler := ClientModelAuthorizationMiddleware("grok-3", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		nextCalled = true
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"hi"}`))
	req = req.WithContext(service.WithClientGrant(req.Context(), grant))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || nextCalled || !strings.Contains(rec.Body.String(), `"code":"model_not_allowed"`) {
		t.Fatalf("status=%d next=%v body=%s", rec.Code, nextCalled, rec.Body.String())
	}

	handler = ClientModelAuthorizationMiddleware("grok-4.5", http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		if !bytes.Equal(body, []byte(`{"input":"hi"}`)) {
			t.Fatalf("middleware changed body: %s", body)
		}
		model, ok := service.EffectiveModelFromContext(request.Context())
		if !ok || model != "grok-4.5" {
			t.Fatalf("effective model=%q ok=%v", model, ok)
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	req = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"hi"}`))
	req = req.WithContext(service.WithClientGrant(req.Context(), grant))
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("allowed status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestClientModelsScopeMiddlewareFiltersCatalog(t *testing.T) {
	grant := service.ClientGrant{
		Authenticated: true, KeyID: "ck_limited", ModelPolicy: clientkey.ModelPolicyAllowlist,
		ModelScopes: []string{"grok-4.5"},
	}
	upstream := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.Header().Set("X-Upstream", "preserved")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{"object":"list","data":[{"id":"grok-3"},{"id":"grok-4.5","name":"Grok"},{"model":"grok-4.5"}]}`))
	})
	handler := ClientModelsScopeMiddleware(upstream)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req = req.WithContext(service.WithClientGrant(req.Context(), grant))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("X-Upstream") != "preserved" {
		t.Fatalf("status=%d headers=%v", rec.Code, rec.Header())
	}
	if strings.Contains(rec.Body.String(), `"id":"grok-3"`) || strings.Count(rec.Body.String(), "grok-4.5") != 2 {
		t.Fatalf("filtered catalog=%s", rec.Body.String())
	}
}

type fakeRateConsumer struct {
	decision repository.RateLimitDecision
	err      error
	grants   []service.ClientGrant
}

func (f *fakeRateConsumer) ConsumeRPM(_ context.Context, grant service.ClientGrant) (repository.RateLimitDecision, error) {
	f.grants = append(f.grants, grant)
	return f.decision, f.err
}

func TestClientRateLimitMiddlewareReturns429AndRepositoryHeaders(t *testing.T) {
	reset := time.Date(2026, 7, 15, 5, 31, 0, 0, time.UTC)
	consumer := &fakeRateConsumer{
		decision: repository.RateLimitDecision{Allowed: false, Limit: 2, Remaining: 0, ResetAt: reset},
		err:      service.ErrClientRateLimited,
	}
	handler := ClientRateLimitMiddleware(consumer, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("rate-limited request reached next handler")
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req = req.WithContext(service.WithClientGrant(req.Context(), service.ClientGrant{
		Authenticated: true, KeyID: "ck_limited", RPMLimit: 999,
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), `"code":"rate_limit_exceeded"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-RateLimit-Limit-Requests") != "2" || rec.Header().Get("X-RateLimit-Remaining-Requests") != "0" || rec.Header().Get("X-RateLimit-Reset-Requests") != strconv.FormatInt(reset.Unix(), 10) {
		t.Fatalf("headers=%v", rec.Header())
	}
	if len(consumer.grants) != 1 || consumer.grants[0].KeyID != "ck_limited" {
		t.Fatalf("grants=%+v", consumer.grants)
	}
}

func TestClientRateLimitMiddlewareAllowsUnlimitedDecision(t *testing.T) {
	consumer := &fakeRateConsumer{decision: repository.RateLimitDecision{Allowed: true, Limit: 0}}
	handler := ClientRateLimitMiddleware(consumer, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req = req.WithContext(service.WithClientGrant(req.Context(), service.ClientGrant{Authenticated: true, KeyID: "ck_unlimited"}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || len(consumer.grants) != 1 {
		t.Fatalf("status=%d grants=%+v body=%s", rec.Code, consumer.grants, rec.Body.String())
	}
}

func TestClientConcurrencyMiddlewareHoldsPermitUntilStreamCompletes(t *testing.T) {
	access := service.NewClientAccess(nil)
	started := make(chan struct{})
	finish := make(chan struct{})
	handler := ClientConcurrencyMiddleware(access, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("data: first\n\n"))
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		close(started)
		<-finish
		_, _ = writer.Write([]byte("data: done\n\n"))
	}))
	grant := service.ClientGrant{Authenticated: true, KeyID: "ck_stream", MaxConcurrent: 1}

	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		req = req.WithContext(service.WithClientGrant(req.Context(), grant))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		firstDone <- rec
	}()
	<-started

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req = req.WithContext(service.WithClientGrant(req.Context(), grant))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), `"code":"concurrent_limit_exceeded"`) {
		t.Fatalf("while stream active status=%d body=%s", rec.Code, rec.Body.String())
	}

	close(finish)
	first := <-firstDone
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "data: done") {
		t.Fatalf("first stream status=%d body=%s", first.Code, first.Body.String())
	}

	// The stream handler returned, so the permit must be available again.
	req = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req = req.WithContext(service.WithClientGrant(req.Context(), grant))
	rec = httptest.NewRecorder()
	quick := ClientConcurrencyMiddleware(access, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	quick.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("after stream status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestClientInferenceMiddlewareFixesSecurityCheckOrder(t *testing.T) {
	access := &orderedInferenceAccess{grant: service.ClientGrant{
		Authenticated: true, KeyID: "ck_1", Principal: "client-key:ck_1",
		ModelPolicy: clientkey.ModelPolicyAllowlist, ModelScopes: []string{"grok-4.5"}, MaxConcurrent: 1,
	}}
	handler := ClientInferenceMiddleware(access, "grok-4.5", http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		access.events = append(access.events, "handler")
		if model, ok := service.EffectiveModelFromContext(request.Context()); !ok || model != "grok-4.5" {
			t.Fatalf("effective model=%q ok=%v", model, ok)
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"hi"}`))
	req.Header.Set("Authorization", "Bearer raw-secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	want := []string{"authenticate", "rpm", "concurrency", "handler", "release"}
	if strings.Join(access.events, ",") != strings.Join(want, ",") {
		t.Fatalf("events=%#v want=%#v", access.events, want)
	}
}

func TestClientModelsMiddlewareAuthenticatesThenFiltersWithoutConsumingTrafficLimits(t *testing.T) {
	access := &orderedInferenceAccess{grant: service.ClientGrant{
		Authenticated: true, KeyID: "ck_1", Principal: "client-key:ck_1",
		ModelPolicy: clientkey.ModelPolicyAllowlist, ModelScopes: []string{"grok-4.5"},
	}}
	handler := ClientModelsMiddleware(access, http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		access.events = append(access.events, "handler")
		writeJSON(writer, http.StatusOK, map[string]any{"object": "list", "data": []map[string]any{
			{"id": "grok-3"}, {"id": "grok-4.5"},
		}})
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("x-api-key", "raw-secret")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "grok-3") || !strings.Contains(rec.Body.String(), "grok-4.5") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Join(access.events, ",") != "authenticate,handler" {
		t.Fatalf("model-list events=%#v", access.events)
	}
}

var _ ClientAuthenticator = (*fakeRequestAuthenticator)(nil)
