package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/internal/account"
	"github.com/AokiAx/grok2api/internal/admin"
	"github.com/AokiAx/grok2api/internal/api"
)

type fakeAdmin struct {
	accounts  []account.Account
	request   admin.ImportRequest
	deleted   string
	recovered string
	lastQuery admin.ListAccountsQuery
}

func (a *fakeAdmin) ListPage(_ context.Context, query admin.ListAccountsQuery) (admin.ListAccountsPage, error) {
	a.lastQuery = query
	page := query.Page
	if page < 1 {
		page = 1
	}
	pageSize := query.PageSize
	if pageSize <= 0 {
		pageSize = 50
	}
	if pageSize > 200 {
		pageSize = 200
	}
	pool := strings.ToLower(strings.TrimSpace(query.Pool))
	q := strings.ToLower(strings.TrimSpace(query.Q))
	filtered := make([]account.Account, 0, len(a.accounts))
	for _, item := range a.accounts {
		if pool == "ready" || pool == "unavailable" {
			if string(item.Pool) != pool {
				continue
			}
		}
		if q != "" {
			hay := strings.ToLower(item.ID + " " + item.Email + " " + string(item.UnavailableReason) + " " + item.LastErrorCode)
			if !strings.Contains(hay, q) {
				continue
			}
		}
		filtered = append(filtered, item)
	}
	start := (page - 1) * pageSize
	if start > len(filtered) {
		start = len(filtered)
	}
	end := start + pageSize
	if end > len(filtered) {
		end = len(filtered)
	}
	return admin.ListAccountsPage{
		Accounts: filtered[start:end],
		Total:    len(filtered),
		Page:     page,
		PageSize: pageSize,
	}, nil
}

func (a *fakeAdmin) Stats(context.Context) (admin.AccountStats, error) {
	now := time.Now().UTC()
	soon := now.Add(time.Hour)
	stats := admin.AccountStats{
		Reasons:    map[string]int{},
		ErrorCodes: map[string]int{},
	}
	for _, item := range a.accounts {
		stats.TotalAccounts++
		if item.Pool == account.PoolReady {
			stats.ReadyAccounts++
		} else {
			stats.UnavailableAccounts++
			if item.UnavailableReason != "" {
				stats.Reasons[string(item.UnavailableReason)]++
			}
			if !item.RetryAt.IsZero() && !item.RetryAt.After(now) {
				stats.RetryDue++
			}
		}
		stats.ActiveLeases += item.Active
		maxActive := item.MaxActive
		if maxActive <= 0 {
			maxActive = 1
		}
		stats.MaxActive += maxActive
		stats.TotalRequests += item.RequestCount
		if item.RefreshToken != "" {
			stats.RefreshableAccounts++
		} else {
			stats.NoRefreshToken++
		}
		if item.AuthenticationFails > 0 {
			stats.AuthFailAccounts++
			stats.TotalAuthFails += int64(item.AuthenticationFails)
		}
		if !item.ExpiresAt.IsZero() {
			if item.ExpiresAt.Before(now) {
				stats.AccessExpired++
			} else if item.ExpiresAt.Before(soon) {
				stats.AccessExpiringSoon++
			}
		}
		if item.LastErrorCode != "" {
			stats.ErrorCodes[item.LastErrorCode]++
		}
		if item.QuotaLimit > 0 {
			used := item.QuotaActual
			if used < 0 {
				used = 0
			}
			if used > item.QuotaLimit {
				used = item.QuotaLimit
			}
			remaining := item.QuotaLimit - used
			stats.QuotaActual += used
			stats.QuotaLimit += item.QuotaLimit
			stats.QuotaRemaining += remaining
			stats.QuotaObserved++
			if item.Pool == account.PoolReady {
				stats.ReadyQuotaRemaining += remaining
				stats.ReadyQuotaObserved++
			}
		}
	}
	return stats, nil
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
			QuotaRemaining      int64          `json:"quota_remaining"`
			ReadyQuotaRemaining int64          `json:"ready_quota_remaining"`
			QuotaObserved       int            `json:"quota_observed_accounts"`
			ReadyQuotaObserved  int            `json:"ready_quota_observed_accounts"`
			NoRefreshToken      int            `json:"no_refresh_token"`
			Reasons             map[string]int `json:"reasons"`
			ErrorCodes          map[string]int `json:"error_codes"`
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
	// remaining = (100-20) + (100-90) = 90; ready remaining = 80
	if summary.QuotaRemaining != 90 || summary.ReadyQuotaRemaining != 80 {
		t.Fatalf("remaining summary = %#v", summary)
	}
	if summary.QuotaObserved != 2 || summary.ReadyQuotaObserved != 1 {
		t.Fatalf("observed summary = %#v", summary)
	}
	if summary.Reasons["quota"] != 1 || summary.Reasons["auth"] != 1 {
		t.Fatalf("reasons = %#v", summary.Reasons)
	}
	if summary.NoRefreshToken != 1 {
		t.Fatalf("no_refresh_token = %d", summary.NoRefreshToken)
	}
	var pageMeta struct {
		Total      int `json:"total"`
		Page       int `json:"page"`
		PageSize   int `json:"page_size"`
		TotalPages int `json:"total_pages"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &pageMeta); err != nil {
		t.Fatalf("decode page meta: %v", err)
	}
	if pageMeta.Total != 3 || pageMeta.Page != 1 || pageMeta.PageSize != 50 || pageMeta.TotalPages != 1 {
		t.Fatalf("page meta = %#v", pageMeta)
	}
}

func TestAdminListPaginationAndFilter(t *testing.T) {
	accounts := make([]account.Account, 0, 12)
	for i := 0; i < 10; i++ {
		id := "ready-" + strconv.Itoa(i)
		accounts = append(accounts, account.Account{
			ID:    id,
			Email: id + "@example.com",
			Pool:  account.PoolReady,
		})
	}
	accounts = append(accounts,
		account.Account{ID: "quota-1", Email: "quota@example.com", Pool: account.PoolUnavailable, UnavailableReason: account.ReasonQuota},
		account.Account{ID: "auth-1", Email: "auth@example.com", Pool: account.PoolUnavailable, UnavailableReason: account.ReasonAuth, LastErrorCode: "refresh-failed"},
	)
	adminService := &fakeAdmin{accounts: accounts}
	server := api.NewServer(&fakeGateway{}, fakeStatus{}, "", api.WithAdmin(adminService, "panel-secret"))

	request := httptest.NewRequest(http.MethodGet, "/admin/api/cli-accounts?page=2&page_size=5&pool=ready", nil)
	request.Header.Set("Authorization", "Bearer panel-secret")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Total      int              `json:"total"`
		Page       int              `json:"page"`
		PageSize   int              `json:"page_size"`
		TotalPages int              `json:"total_pages"`
		Accounts   []map[string]any `json:"accounts"`
		Summary    struct {
			TotalAccounts int `json:"total_accounts"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Total != 10 || payload.Page != 2 || payload.PageSize != 5 || payload.TotalPages != 2 {
		t.Fatalf("pagination = total=%d page=%d size=%d pages=%d", payload.Total, payload.Page, payload.PageSize, payload.TotalPages)
	}
	if len(payload.Accounts) != 5 {
		t.Fatalf("page len = %d", len(payload.Accounts))
	}
	// Summary stays global, not filtered to ready-only.
	if payload.Summary.TotalAccounts != 12 {
		t.Fatalf("summary total = %d", payload.Summary.TotalAccounts)
	}
	if adminService.lastQuery.Pool != "ready" || adminService.lastQuery.Page != 2 || adminService.lastQuery.PageSize != 5 {
		t.Fatalf("last query = %#v", adminService.lastQuery)
	}

	searchReq := httptest.NewRequest(http.MethodGet, "/admin/api/cli-accounts?q=refresh-failed&page_size=10", nil)
	searchReq.Header.Set("Authorization", "Bearer panel-secret")
	searchRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(searchRec, searchReq)
	var searchPayload struct {
		Total    int              `json:"total"`
		Accounts []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(searchRec.Body.Bytes(), &searchPayload); err != nil {
		t.Fatalf("decode search: %v", err)
	}
	if searchPayload.Total != 1 || len(searchPayload.Accounts) != 1 {
		t.Fatalf("search payload = %#v", searchPayload)
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
	body := `{"accounts":[{"key":"token","email":"user@example.com"}]}`

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
	body := recorder.Body.String()
	if !strings.Contains(body, "恢复验证") || !strings.Contains(body, "importFile") || !strings.Contains(body, "importOutput") || strings.Contains(body, "accounts.innerHTML") {
		t.Fatal("account actions or safe table rendering missing")
	}
	for _, marker := range []string{
		`id="totalAccounts"`, `id="totalRequests"`, `id="activeLeases"`,
		`id="refreshableAccounts"`, `id="quotaUsage"`, `id="reasonBars"`,
		`id="errorCodeBars"`, `id="accessExpiry"`, `id="retryDue"`,
		`id="authFailAccounts"`, `id="noRefreshToken"`, `id="rememberLogin"`,
		`id="lastUpdated"`, "renderSummary", "setInterval(refresh,15000)",
		`id="pagePrev"`, `id="pageNext"`, `id="pageSize"`, "accountsQuery",
		"localStorage", "persistPassword",
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

type liveStatus struct {
	fakeStatus
	active map[string]int
}

func (s liveStatus) ActiveByID() map[string]int {
	return s.active
}

func TestAdminListMergesLiveActiveLeases(t *testing.T) {
	adminService := &fakeAdmin{accounts: []account.Account{
		{ID: "a", Email: "a@example.com", Pool: account.PoolReady, MaxActive: 1, Active: 0},
		{ID: "b", Email: "b@example.com", Pool: account.PoolReady, MaxActive: 1, Active: 0},
	}}
	server := api.NewServer(
		&fakeGateway{},
		liveStatus{active: map[string]int{"a": 1}},
		"",
		api.WithAdmin(adminService, "panel-secret"),
	)
	request := httptest.NewRequest(http.MethodGet, "/admin/api/cli-accounts", nil)
	request.Header.Set("Authorization", "Bearer panel-secret")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Accounts []struct {
			ID     string `json:"id"`
			Active int    `json:"active"`
		} `json:"accounts"`
		Summary struct {
			ActiveLeases int `json:"active_leases"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := map[string]int{}
	for _, item := range payload.Accounts {
		byID[item.ID] = item.Active
	}
	if byID["a"] != 1 || byID["b"] != 0 {
		t.Fatalf("active by id = %#v", byID)
	}
	if payload.Summary.ActiveLeases != 1 {
		t.Fatalf("active_leases = %d", payload.Summary.ActiveLeases)
	}
}
