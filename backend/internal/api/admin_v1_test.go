package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/backend/internal/account"
	"github.com/AokiAx/grok2api/backend/internal/api"
)

func TestAdminV1EnvelopeAndLegacyAlias(t *testing.T) {
	adminService := &fakeAdmin{accounts: []account.Account{
		{ID: "a1", Pool: account.PoolReady, MaxActive: 1, RequestCount: 3},
		{ID: "a2", Pool: account.PoolUnavailable, UnavailableReason: account.ReasonQuota, MaxActive: 1},
	}}
	server := api.NewServer(
		&fakeGateway{},
		fakeStatus{},
		"api-secret",
		api.WithAdmin(adminService, "panel-secret"),
	)

	// Meta is public.
	req := httptest.NewRequest(http.MethodGet, "/api/admin/v1/system/meta", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("meta status=%d body=%s", rec.Code, rec.Body.String())
	}
	var meta struct {
		OK   bool `json:"ok"`
		Data struct {
			AuthRequired bool     `json:"auth_required"`
			APIVersion   string   `json:"api_version"`
			PanelPaths   []string `json:"panel_paths"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &meta); err != nil {
		t.Fatalf("meta decode: %v", err)
	}
	if !meta.OK || !meta.Data.AuthRequired || meta.Data.APIVersion != "v1" || len(meta.Data.PanelPaths) != 1 || meta.Data.PanelPaths[0] != "/" {
		t.Fatalf("meta=%#v", meta)
	}

	// Login.
	req = httptest.NewRequest(http.MethodPost, "/api/admin/v1/auth/login", strings.NewReader(`{"password":"panel-secret"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", rec.Code, rec.Body.String())
	}
	var login struct {
		OK   bool `json:"ok"`
		Data struct {
			Tokens struct {
				AccessToken string `json:"accessToken"`
			} `json:"tokens"`
			Admin struct {
				Username string `json:"username"`
			} `json:"admin"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &login); err != nil || !login.OK || login.Data.Tokens.AccessToken != "panel-secret" {
		t.Fatalf("login=%#v err=%v body=%s", login, err, rec.Body.String())
	}

	// Dashboard requires auth.
	req = httptest.NewRequest(http.MethodGet, "/api/admin/v1/dashboard", nil)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("dashboard unauth status=%d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/admin/v1/dashboard", nil)
	req.Header.Set("Authorization", "Bearer panel-secret")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("dashboard status=%d body=%s", rec.Code, rec.Body.String())
	}
	var dash struct {
		OK   bool `json:"ok"`
		Data struct {
			Summary map[string]any `json:"summary"`
			Pool    map[string]any `json:"account_pool"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &dash); err != nil || !dash.OK {
		t.Fatalf("dashboard=%#v err=%v body=%s", dash, err, rec.Body.String())
	}
	if dash.Data.Summary == nil {
		t.Fatalf("missing summary: %#v", dash.Data)
	}

	// Accounts list envelope.
	req = httptest.NewRequest(http.MethodGet, "/api/admin/v1/accounts?page=1&page_size=10", nil)
	req.Header.Set("Authorization", "Bearer panel-secret")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("accounts status=%d body=%s", rec.Code, rec.Body.String())
	}
	var list struct {
		OK   bool `json:"ok"`
		Data struct {
			Total    int              `json:"total"`
			Accounts []map[string]any `json:"accounts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || !list.OK {
		t.Fatalf("list=%#v err=%v", list, err)
	}
	if list.Data.Total != 2 || len(list.Data.Accounts) != 2 {
		t.Fatalf("list data=%#v", list.Data)
	}
	// no token leak
	if strings.Contains(rec.Body.String(), "secret-") {
		t.Fatalf("token leak: %s", rec.Body.String())
	}

	// Legacy path still works without envelope.
	req = httptest.NewRequest(http.MethodGet, "/admin/api/cli-accounts", nil)
	req.Header.Set("Authorization", "Bearer panel-secret")
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy status=%d", rec.Code)
	}
	var legacy map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &legacy); err != nil {
		t.Fatalf("legacy decode: %v", err)
	}
	if _, ok := legacy["ok"]; ok {
		t.Fatalf("legacy should not use v1 envelope: %#v", legacy)
	}
	if legacy["total"] != float64(2) {
		t.Fatalf("legacy total=%#v", legacy["total"])
	}

	// healthz / readyz aliases
	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz=%d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	// ready=1 in fakeStatus → 200
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminV1UnauthorizedEnvelope(t *testing.T) {
	server := api.NewServer(
		&fakeGateway{},
		fakeStatus{},
		"",
		api.WithAdmin(&fakeAdmin{}, "secret"),
	)
	req := httptest.NewRequest(http.MethodGet, "/api/admin/v1/pool", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d", rec.Code)
	}
	var body struct {
		OK    bool `json:"ok"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.OK || body.Error.Code != "unauthorized" {
		t.Fatalf("body=%#v", body)
	}
}
