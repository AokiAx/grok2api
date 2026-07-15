package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	service "github.com/AokiAx/grok2api/backend/internal/adminauth"
	domain "github.com/AokiAx/grok2api/backend/internal/domain/adminauth"
	"github.com/AokiAx/grok2api/backend/internal/infra/persistence/sqlite"
	"github.com/AokiAx/grok2api/backend/internal/security"
)

func TestAdminAuthHTTPLoginMeAndLogout(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.OpenSQLite(ctx, t.TempDir()+"/auth.db")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	now := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	cred, _ := security.HashAdminPassword("secret", 4)
	u, _ := domain.NewAdminUser("u1", "admin", cred, now)
	if err := repo.CreateAdminUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	svc := service.NewService(repo, service.WithClock(func() time.Time { return now }))
	handler := NewAdminAuthHandler(svc, AdminAuthHandlerOptions{SecureCookies: true, Clock: func() time.Time { return now }})
	req := httptest.NewRequest(http.MethodPost, "/api/admin/v1/auth/login", strings.NewReader(`{"username":"admin","password":"secret","remember":true}`))
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"accessToken"`) || !strings.Contains(rec.Header().Get("Set-Cookie"), "HttpOnly") || !strings.Contains(rec.Header().Get("Set-Cookie"), "SameSite=Strict") {
		t.Fatalf("login %d %s %v", rec.Code, rec.Body, rec.Header())
	}
	var body struct {
		Data struct {
			Tokens struct {
				Access string `json:"accessToken"`
			} `json:"tokens"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	me := httptest.NewRequest(http.MethodGet, "/api/admin/v1/auth/me", nil)
	me.Header.Set("Authorization", "Bearer "+body.Data.Tokens.Access)
	mrec := httptest.NewRecorder()
	handler.ServeHTTP(mrec, me)
	if mrec.Code != 200 || !strings.Contains(mrec.Body.String(), `"username":"admin"`) {
		t.Fatalf("me %d %s", mrec.Code, mrec.Body)
	}
	logout := httptest.NewRequest(http.MethodPost, "/api/admin/v1/auth/logout", nil)
	logout.Header = me.Header.Clone()
	lrec := httptest.NewRecorder()
	handler.ServeHTTP(lrec, logout)
	if lrec.Code != 200 || !strings.Contains(lrec.Header().Get("Set-Cookie"), "Max-Age=0") {
		t.Fatalf("logout %d %v", lrec.Code, lrec.Header())
	}
}

func TestAdminAuthErrorsAreJSONAndNilServiceDoesNotPanic(t *testing.T) {
	handler := NewAdminAuthHandler(nil, AdminAuthHandlerOptions{})
	tests := []struct{ method, path, body string }{
		{http.MethodPost, "/api/admin/v1/auth/login", `{"username":"admin","password":"secret"}`},
		{http.MethodPost, "/api/admin/v1/auth/refresh", ""},
		{http.MethodPost, "/api/admin/v1/auth/logout", ""},
		{http.MethodGet, "/api/admin/v1/auth/me", ""},
	}
	for _, tc := range tests {
		t.Run(tc.path+tc.method, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer token")
			req.AddCookie(&http.Cookie{Name: "grok2api_admin_refresh", Value: "session.secret"})
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("headers=%v", rec.Header())
			}
			if rec.Body.String() != "{\"error\":{\"code\":\"internal_error\",\"message\":\"\"},\"ok\":false}\n" {
				t.Fatalf("body=%q", rec.Body.String())
			}
		})
	}
}

func TestAdminAuthMalformedAndEmptyLoginRequests(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.OpenSQLite(ctx, t.TempDir()+"/empty-login.db")
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	now := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	cred, _ := security.HashAdminPassword("secret", 4)
	u, _ := domain.NewAdminUser("u1", "admin", cred, now)
	if err := repo.CreateAdminUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	handler := NewAdminAuthHandler(service.NewService(repo, service.WithClock(func() time.Time { return now })), AdminAuthHandlerOptions{})
	for _, body := range []string{"{", `{}`, `{"username":"","password":""}`} {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/v1/auth/login", strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:1"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"invalid_request"`) {
			t.Fatalf("body=%q status=%d response=%s", body, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Content-Type") != "application/json" {
			t.Fatalf("content type=%q", rec.Header().Get("Content-Type"))
		}
	}
}

func TestRequestIPSupportsIPv6WithAndWithoutPort(t *testing.T) {
	for remote, want := range map[string]string{
		"[2001:db8::1]:443": "2001:db8::1",
		"2001:db8::1":       "2001:db8::1",
		"127.0.0.1:8080":    "127.0.0.1",
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = remote
		if got := requestIP(r); got != want {
			t.Fatalf("requestIP(%q)=%q want=%q", remote, got, want)
		}
	}
}
