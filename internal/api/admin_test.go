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
	adminService := &fakeAdmin{accounts: []account.Account{{
		ID:           "account-1",
		Email:        "user@example.com",
		AccessToken:  "secret-access-token",
		RefreshToken: "secret-refresh-token",
		Pool:         account.PoolReady,
	}}}
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
