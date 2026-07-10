package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AokiAx/grok2api/internal/account"
	"github.com/AokiAx/grok2api/internal/admin"
	"github.com/AokiAx/grok2api/internal/api"
	"github.com/AokiAx/grok2api/internal/register"
)

type fakeAdmin struct {
	accounts  []account.Account
	request   admin.ImportRequest
	deleted   string
	recovered string
}

func (a *fakeAdmin) List(context.Context) ([]account.Account, error) {
	return a.accounts, nil
}

func (a *fakeAdmin) Import(_ context.Context, request admin.ImportRequest) (admin.ImportResult, error) {
	a.request = request
	return admin.ImportResult{Added: len(request.Accounts), Applied: !request.DryRun}, nil
}

func (a *fakeAdmin) Delete(_ context.Context, id string) error {
	a.deleted = id
	return nil
}

func (a *fakeAdmin) Recover(_ context.Context, id string) (account.Account, error) {
	a.recovered = id
	return account.Account{ID: id, Pool: account.PoolReady}, nil
}

func TestAdminListNeverReturnsTokens(t *testing.T) {
	adminService := &fakeAdmin{accounts: []account.Account{
		{
			ID: "account-1", Email: "user@example.com",
			AccessToken: "secret-access-token", RefreshToken: "secret-refresh-token",
			Pool: account.PoolReady, Active: 1, MaxActive: 2,
			RequestCount: 10, QuotaActual: 20, QuotaLimit: 100,
		},
		{
			ID: "account-2", RefreshToken: "refresh-2",
			Pool: account.PoolUnavailable, UnavailableReason: account.ReasonQuota,
			MaxActive: 1, RequestCount: 5, QuotaActual: 90, QuotaLimit: 100,
		},
		{
			ID: "account-3", Pool: account.PoolUnavailable,
			UnavailableReason: account.ReasonAuth, MaxActive: 1, RequestCount: 2,
		},
	}}
	server := api.NewServer(
		&fakeGateway{},
		fakeStatus{},
		"",
		api.WithAdmin(adminService, "panel-secret"),
	)
	request := httptest.NewRequest(http.MethodGet, "/admin/api/cli-accounts", nil)
	request.Header.Set("Authorization", "Bearer panel-secret")
	recorder := httptest.NewRecorder()

	server.Handler().ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	body := recorder.Body.String()
	if strings.Contains(body, "secret-access-token") || strings.Contains(body, "secret-refresh-token") {
		t.Fatalf("response leaked token: %s", body)
	}
	var payload struct {
		Summary struct {
			TotalAccounts       int            `json:"total_accounts"`
			ReadyAccounts       int            `json:"ready_accounts"`
			UnavailableAccounts int            `json:"unavailable_accounts"`
			ActiveLeases        int            `json:"active_leases"`
			MaxActive           int            `json:"max_active"`
			TotalRequests       int64          `json:"total_requests"`
			RefreshableAccounts int            `json:"refreshable_accounts"`
			QuotaActual         int64          `json:"quota_actual"`
			QuotaLimit          int64          `json:"quota_limit"`
			Reasons             map[string]int `json:"reasons"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	summary := payload.Summary
	if summary.TotalAccounts != 3 || summary.ReadyAccounts != 1 || summary.UnavailableAccounts != 2 {
		t.Fatalf("pool summary = %#v", summary)
	}
	if summary.ActiveLeases != 1 || summary.MaxActive != 4 || summary.TotalRequests != 17 {
		t.Fatalf("runtime summary = %#v", summary)
	}
	if summary.RefreshableAccounts != 2 || summary.QuotaActual != 110 || summary.QuotaLimit != 200 {
		t.Fatalf("credential summary = %#v", summary)
	}
	if summary.Reasons["quota"] != 1 || summary.Reasons["auth"] != 1 {
		t.Fatalf("reasons = %#v", summary.Reasons)
	}
}

func TestAdminImportPreviewAndApply(t *testing.T) {
	adminService := &fakeAdmin{}
	server := api.NewServer(
		&fakeGateway{},
		fakeStatus{},
		"",
		api.WithAdmin(adminService, "panel-secret"),
	)
	body := `{"accounts":[{"access_token":"token","email":"user@example.com"}]}`

	preview := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/import/preview", strings.NewReader(body))
	preview.Header.Set("x-api-key", "panel-secret")
	previewRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(previewRecorder, preview)
	if previewRecorder.Code != http.StatusOK || !adminService.request.DryRun {
		t.Fatalf("preview status=%d request=%#v", previewRecorder.Code, adminService.request)
	}

	apply := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/import", strings.NewReader(body))
	apply.Header.Set("x-api-key", "panel-secret")
	applyRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(applyRecorder, apply)
	if applyRecorder.Code != http.StatusOK || adminService.request.DryRun {
		t.Fatalf("apply status=%d request=%#v", applyRecorder.Code, adminService.request)
	}
	var result admin.ImportResult
	if err := json.Unmarshal(applyRecorder.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.Added != 1 || !result.Applied {
		t.Fatalf("result = %#v", result)
	}
}

func TestPanelRouteIsEmbedded(t *testing.T) {
	server := api.NewServer(&fakeGateway{}, fakeStatus{}, "")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/panel", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "Ready Pool") {
		t.Fatal("Go panel content missing")
	}
	if !strings.Contains(recorder.Body.String(), "恢复验证") || strings.Contains(recorder.Body.String(), "accounts.innerHTML") {
		t.Fatal("account actions or safe table rendering missing")
	}
	for _, marker := range []string{
		`id="totalAccounts"`, `id="totalRequests"`, `id="activeLeases"`,
		`id="refreshableAccounts"`, `id="quotaUsage"`, `id="reasonBars"`,
		`id="lastUpdated"`, "renderSummary", "setInterval(refresh,15000)",
	} {
		if !strings.Contains(recorder.Body.String(), marker) {
			t.Fatalf("panel statistics marker missing: %s", marker)
		}
	}
}

func TestAdminDeleteAndRecoverRoutes(t *testing.T) {
	adminService := &fakeAdmin{}
	server := api.NewServer(&fakeGateway{}, fakeStatus{}, "", api.WithAdmin(adminService, "secret"))

	deleteRequest := httptest.NewRequest(http.MethodDelete, "/admin/api/cli-accounts/account-1", nil)
	deleteRequest.Header.Set("Authorization", "Bearer secret")
	deleteRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(deleteRecorder, deleteRequest)
	if deleteRecorder.Code != http.StatusOK || adminService.deleted != "account-1" {
		t.Fatalf("delete status=%d id=%q", deleteRecorder.Code, adminService.deleted)
	}

	recoverRequest := httptest.NewRequest(http.MethodPost, "/admin/api/cli-accounts/account-1/recover", nil)
	recoverRequest.Header.Set("Authorization", "Bearer secret")
	recoverRecorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recoverRecorder, recoverRequest)
	if recoverRecorder.Code != http.StatusOK || adminService.recovered != "account-1" {
		t.Fatalf("recover status=%d id=%q", recoverRecorder.Code, adminService.recovered)
	}
}

type fakeRegisterJobs struct {
	started register.RunConfig
	stopped bool
}

func (f *fakeRegisterJobs) Start(cfg register.RunConfig) (string, error) {
	f.started = cfg
	return "reg-1", nil
}
func (f *fakeRegisterJobs) Stop() error { f.stopped = true; return nil }
func (f *fakeRegisterJobs) Status() register.JobStatus {
	return register.JobStatus{State: register.JobIdle, Logs: []string{"ready"}}
}
func (f *fakeRegisterJobs) Health(context.Context) register.HealthReport {
	return register.HealthReport{Turnstile: "auto", Email: "cfmail", Proxy: "direct"}
}

func TestRegisterJobRoutes(t *testing.T) {
	jobs := &fakeRegisterJobs{}
	server := api.NewServer(&fakeGateway{}, fakeStatus{}, "", api.WithAdmin(&fakeAdmin{}, "secret"), api.WithRegisterJobs(jobs))

	start := httptest.NewRequest(http.MethodPost, "/admin/api/register/start", strings.NewReader(`{"count":2,"workers":1,"dry_run":true}`))
	start.Header.Set("Authorization", "Bearer secret")
	start.Header.Set("Content-Type", "application/json")
	startRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(startRec, start)
	if startRec.Code != http.StatusOK || jobs.started.Count != 2 || !jobs.started.DryRun {
		t.Fatalf("start status=%d cfg=%#v body=%s", startRec.Code, jobs.started, startRec.Body.String())
	}

	status := httptest.NewRequest(http.MethodGet, "/admin/api/register/status", nil)
	status.Header.Set("Authorization", "Bearer secret")
	statusRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(statusRec, status)
	if statusRec.Code != http.StatusOK || !strings.Contains(statusRec.Body.String(), "ready") {
		t.Fatalf("status=%d body=%s", statusRec.Code, statusRec.Body.String())
	}

	stop := httptest.NewRequest(http.MethodPost, "/admin/api/register/stop", nil)
	stop.Header.Set("Authorization", "Bearer secret")
	stopRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(stopRec, stop)
	if stopRec.Code != http.StatusOK || !jobs.stopped {
		t.Fatalf("stop status=%d stopped=%v", stopRec.Code, jobs.stopped)
	}
}
