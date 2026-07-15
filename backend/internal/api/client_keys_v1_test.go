package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/clientkeys"
	"github.com/AokiAx/grok2api/backend/internal/domain/clientkey"
	"github.com/AokiAx/grok2api/backend/internal/repository"
)

type fakeClientKeyLifecycle struct {
	created clientkeys.CreateRequest
	updated clientkeys.UpdateRequest
	id      string
	revoked bool
	expires time.Time
}

func (f *fakeClientKeyLifecycle) Create(_ context.Context, request clientkeys.CreateRequest) (clientkeys.Result, error) {
	f.created = request
	return clientkeys.Result{
		Key: clientkey.ClientKey{
			ID: "ck_1", Name: request.Name, Origin: clientkey.OriginManaged, KeyPrefix: "g2a_preview",
			ModelPolicy: request.ModelPolicy, RPMLimit: request.RPMLimit, MaxConcurrent: request.MaxConcurrent,
			ExpiresAt: request.ExpiresAt, CreatedAt: apiTestTime, UpdatedAt: apiTestTime,
		},
		Scopes: request.Scopes,
		Secret: "g2a_once_only_secret",
	}, nil
}

func (f *fakeClientKeyLifecycle) Get(_ context.Context, id string) (clientkeys.Result, error) {
	f.id = id
	return clientkeys.Result{Key: clientkey.ClientKey{
		ID: id, Name: "production", Origin: clientkey.OriginManaged, KeyPrefix: "g2a_preview",
		ModelPolicy: clientkey.ModelPolicyAll, ExpiresAt: f.expires, CreatedAt: apiTestTime, UpdatedAt: apiTestTime,
	}}, nil
}

func (f *fakeClientKeyLifecycle) List(_ context.Context, query repository.ListClientKeysQuery) (clientkeys.ListResult, error) {
	item, _ := f.Get(context.Background(), "ck_1")
	return clientkeys.ListResult{Items: []clientkeys.Result{item}, Total: 1, Page: query.Page, PageSize: query.PageSize}, nil
}

func (f *fakeClientKeyLifecycle) Update(_ context.Context, id string, request clientkeys.UpdateRequest) (clientkeys.Result, error) {
	f.id, f.updated = id, request
	return clientkeys.Result{Key: clientkey.ClientKey{
		ID: id, Name: request.Name, Origin: clientkey.OriginManaged, KeyPrefix: "g2a_preview",
		ModelPolicy: request.ModelPolicy, RPMLimit: request.RPMLimit, MaxConcurrent: request.MaxConcurrent,
		ExpiresAt: request.ExpiresAt, CreatedAt: apiTestTime, UpdatedAt: apiTestTime,
	}, Scopes: request.Scopes}, nil
}

func (f *fakeClientKeyLifecycle) Revoke(_ context.Context, id string) (clientkeys.Result, error) {
	f.id, f.revoked = id, true
	return clientkeys.Result{Key: clientkey.ClientKey{
		ID: id, Name: "production", Origin: clientkey.OriginManaged, KeyPrefix: "g2a_preview",
		ModelPolicy: clientkey.ModelPolicyAll, RevokedAt: apiTestTime, CreatedAt: apiTestTime, UpdatedAt: apiTestTime,
	}}, nil
}

var apiTestTime = time.Date(2026, 7, 15, 4, 0, 0, 0, time.UTC)

func TestClientKeyAdminCreateReturnsSecretOnceAndDisablesCaching(t *testing.T) {
	service := &fakeClientKeyLifecycle{}
	handler := NewClientKeyAdminHandler(service, func(request *http.Request) bool {
		return request.Header.Get("Authorization") == "Bearer admin"
	})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/v1/client-keys", strings.NewReader(`{
		"name":"production","model_policy":"allowlist","model_scopes":["grok-4.5"],
		"rpm_limit":60,"max_concurrent":2,"expires_at":"2026-08-15T04:00:00Z"
	}`))
	req.Header.Set("Authorization", "Bearer admin")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Location") != "/api/admin/v1/client-keys/ck_1" || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create headers=%v", rec.Header())
	}
	if !strings.Contains(rec.Body.String(), `"secret":"g2a_once_only_secret"`) {
		t.Fatalf("create omitted one-time secret: %s", rec.Body.String())
	}
	if service.created.ModelPolicy != clientkey.ModelPolicyAllowlist || len(service.created.Scopes) != 1 {
		t.Fatalf("create request=%+v", service.created)
	}

	for _, target := range []string{"/api/admin/v1/client-keys", "/api/admin/v1/client-keys/ck_1"} {
		req = httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Authorization", "Bearer admin")
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status=%d body=%s", target, rec.Code, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), "key_hash") {
			t.Fatalf("GET %s disclosed credential material: %s", target, rec.Body.String())
		}
	}
}

func TestClientKeyAdminPatchRejectsImmutableAndRevocationFields(t *testing.T) {
	handler := NewClientKeyAdminHandler(&fakeClientKeyLifecycle{}, func(*http.Request) bool { return true })
	for _, body := range []string{
		`{"name":"x","model_policy":"all","key_hash":"replace"}`,
		`{"name":"x","model_policy":"all","origin":"managed"}`,
		`{"name":"x","model_policy":"all","revoked_at":null}`,
		`{"name":"x","model_policy":"all","key_prefix":"replace"}`,
	} {
		req := httptest.NewRequest(http.MethodPatch, "/api/admin/v1/client-keys/ck_1", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"invalid_request"`) {
			t.Fatalf("PATCH body=%s status=%d response=%s", body, rec.Code, rec.Body.String())
		}
	}
}

func TestClientKeyAdminLifecycleRoutesAndEnvelope(t *testing.T) {
	service := &fakeClientKeyLifecycle{}
	handler := NewClientKeyAdminHandler(service, func(*http.Request) bool { return true })

	req := httptest.NewRequest(http.MethodPatch, "/api/admin/v1/client-keys/ck_1", strings.NewReader(`{
		"name":"limited","model_policy":"allowlist","model_scopes":["grok-4.5"],"rpm_limit":10,"max_concurrent":1
	}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || service.updated.Name != "limited" || service.id != "ck_1" {
		t.Fatalf("patch status=%d body=%s update=%+v", rec.Code, rec.Body.String(), service.updated)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/admin/v1/client-keys/ck_1/revoke", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !service.revoked || strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("revoke status=%d body=%s called=%v", rec.Code, rec.Body.String(), service.revoked)
	}

	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil || !envelope.OK || envelope.Data.ID != "ck_1" {
		t.Fatalf("envelope=%+v err=%v body=%s", envelope, err, rec.Body.String())
	}
}

func TestClientKeyAdminPatchDistinguishesOmittedAndNullExpiry(t *testing.T) {
	service := &fakeClientKeyLifecycle{expires: apiTestTime.Add(24 * time.Hour)}
	handler := NewClientKeyAdminHandler(service, func(*http.Request) bool { return true })

	req := httptest.NewRequest(http.MethodPatch, "/api/admin/v1/client-keys/ck_1", strings.NewReader(`{"name":"kept"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !service.updated.ExpiresAt.Equal(service.expires) {
		t.Fatalf("omitted expiry status=%d updated=%v want=%v body=%s", rec.Code, service.updated.ExpiresAt, service.expires, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodPatch, "/api/admin/v1/client-keys/ck_1", strings.NewReader(`{"expires_at":null}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !service.updated.ExpiresAt.IsZero() {
		t.Fatalf("null expiry status=%d updated=%v body=%s", rec.Code, service.updated.ExpiresAt, rec.Body.String())
	}
}

func TestClientKeyAdminRequiresAuthorization(t *testing.T) {
	handler := NewClientKeyAdminHandler(&fakeClientKeyLifecycle{}, func(*http.Request) bool { return false })
	req := httptest.NewRequest(http.MethodGet, "/api/admin/v1/client-keys", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"code":"unauthorized"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
