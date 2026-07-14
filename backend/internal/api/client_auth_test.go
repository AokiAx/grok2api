package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/backend/internal/service"
)

type fakeRequestAuthenticator struct {
	secrets []string
	grants  map[string]service.ClientGrant
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

var _ ClientAuthenticator = (*fakeRequestAuthenticator)(nil)
var _ = errors.Is
