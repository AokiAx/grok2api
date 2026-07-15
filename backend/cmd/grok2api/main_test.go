package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/admin"
	"github.com/AokiAx/grok2api/backend/internal/api"
	"github.com/AokiAx/grok2api/backend/internal/bootstrap"
	"github.com/AokiAx/grok2api/backend/internal/config"
	"github.com/AokiAx/grok2api/backend/internal/domain/account"
	"github.com/AokiAx/grok2api/backend/internal/infra/persistence/sqlite"
	"github.com/AokiAx/grok2api/backend/internal/security"
	"github.com/AokiAx/grok2api/backend/internal/service"
	_ "modernc.org/sqlite"
)

func TestRunBootstrapAdminFromStdinIsExplicitAndIsolatedFromLegacyMigration(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	configData, err := json.Marshal(map[string]any{
		"data_dir":       dir,
		"panel_password": "legacy-panel-password",
		"api_key":        "legacy-client-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cli_accounts.json"), []byte(`{"accounts":[{"id":"legacy-account","key":"secret","enabled":true}]}`), 0o600); err != nil {
		t.Fatalf("write legacy accounts: %v", err)
	}

	const password = "  explicit-bootstrap-password  "
	var output bytes.Buffer
	if err := runWithIO(
		context.Background(),
		[]string{"bootstrap-admin", "--config", configPath, "--password-stdin"},
		strings.NewReader(password+"\r\n"),
		&output,
	); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}

	repo, err := sqlite.OpenSQLite(context.Background(), filepath.Join(dir, "grok2api.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer repo.Close()
	user, found, err := repo.GetAdminUserByUsername(context.Background(), "admin")
	if err != nil || !found || !security.VerifyAdminPassword(user.Password, password) {
		t.Fatalf("admin found=%v err=%v user=%+v", found, err, user)
	}
	if security.VerifyAdminPassword(user.Password, strings.TrimSpace(password)) {
		t.Fatal("bootstrap password whitespace was trimmed")
	}
	if count, err := repo.AccountCount(context.Background()); err != nil || count != 0 {
		t.Fatalf("legacy account count=%d err=%v", count, err)
	}
	legacyHash := sha256.Sum256([]byte("legacy-client-secret"))
	if _, found, err := repo.FindClientKeyByHash(context.Background(), legacyHash); err != nil || found {
		t.Fatalf("legacy client key found=%v err=%v", found, err)
	}

	raw, err := sql.Open("sqlite", filepath.Join(dir, "grok2api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var marker string
	if err := raw.QueryRow(`SELECT value FROM app_meta WHERE key='admin_bootstrap_v1'`).Scan(&marker); err != nil || marker != "1" {
		t.Fatalf("admin marker=%q err=%v", marker, err)
	}
	if err := raw.QueryRow(`SELECT value FROM app_meta WHERE key='legacy_admin_bootstrap_v1'`).Scan(&marker); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("legacy marker=%q err=%v, want absent", marker, err)
	}

	stdout := output.String()
	if !strings.Contains(stdout, `"status":"created"`) || !strings.Contains(stdout, `"username":"admin"`) {
		t.Fatalf("output=%s", stdout)
	}
	for _, secret := range []string{password, "explicit-bootstrap-password", "legacy-panel-password", "legacy-client-secret", user.Password.Hash} {
		if strings.Contains(stdout, secret) {
			t.Fatalf("bootstrap output leaked secret %q: %s", secret, stdout)
		}
	}

	err = runWithIO(
		context.Background(),
		[]string{"bootstrap-admin", "--config", configPath, "--password-stdin"},
		strings.NewReader("different-bootstrap-password\n"),
		&bytes.Buffer{},
	)
	if !errors.Is(err, bootstrap.ErrBootstrapAlreadyCompleted) {
		t.Fatalf("second bootstrap err=%v", err)
	}
	unchanged, found, err := repo.GetAdminUserByUsername(context.Background(), "admin")
	if err != nil || !found || unchanged.Password.Hash != user.Password.Hash {
		t.Fatalf("admin changed found=%v err=%v user=%+v", found, err, unchanged)
	}
}

func TestRunBootstrapAdminRequiresPasswordStdinAndRejectsWeakPassword(t *testing.T) {
	for _, test := range []struct {
		name      string
		arguments []string
		input     string
		wantError error
		wantText  string
	}{
		{name: "missing flag", arguments: nil, input: "long-enough-password\n", wantText: "--password-stdin"},
		{name: "weak password", arguments: []string{"--password-stdin"}, input: "short\n", wantError: bootstrap.ErrWeakPassword},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.json")
			configData, err := json.Marshal(map[string]any{"data_dir": dir, "panel_password": "must-not-be-used"})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(configPath, configData, 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			arguments := append([]string{"bootstrap-admin", "--config", configPath}, test.arguments...)
			err = runWithIO(context.Background(), arguments, strings.NewReader(test.input), &bytes.Buffer{})
			if test.wantError != nil && !errors.Is(err, test.wantError) {
				t.Fatalf("err=%v want=%v", err, test.wantError)
			}
			if test.wantText != "" && (err == nil || !strings.Contains(err.Error(), test.wantText)) {
				t.Fatalf("err=%v want text %q", err, test.wantText)
			}

			repo, openErr := sqlite.OpenSQLite(context.Background(), filepath.Join(dir, "grok2api.db"))
			if openErr != nil {
				t.Fatalf("open sqlite: %v", openErr)
			}
			defer repo.Close()
			if count, countErr := repo.CountAdminUsers(context.Background()); countErr != nil || count != 0 {
				t.Fatalf("admin count=%d err=%v", count, countErr)
			}
		})
	}
}

func TestRunBootstrapsLegacySecurityBeforeServeAndClearsSecrets(t *testing.T) {
	tests := []struct {
		name             string
		config           map[string]any
		wantAdminCount   int
		wantAdminSecret  string
		wantClientSecret string
	}{
		{
			name: "panel password takes precedence and api key becomes a client key",
			config: map[string]any{
				"panel_password": "panel-admin-secret",
				"app_key":        "app-admin-secret",
				"api_key":        "legacy-client-secret",
			},
			wantAdminCount:   1,
			wantAdminSecret:  "panel-admin-secret",
			wantClientSecret: "legacy-client-secret",
		},
		{
			name: "api key alone does not create an administrator",
			config: map[string]any{
				"api_key": "client-only-secret",
			},
			wantAdminCount:   0,
			wantClientSecret: "client-only-secret",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.json")
			test.config["data_dir"] = dir
			configData, err := json.Marshal(test.config)
			if err != nil {
				t.Fatalf("marshal config: %v", err)
			}
			if err := os.WriteFile(configPath, configData, 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}

			serveCalled := false
			err = runWithServe(context.Background(), []string{"serve", "--config", configPath}, &bytes.Buffer{}, func(
				ctx context.Context,
				settings config.Config,
				repo runtimeRepository,
			) error {
				serveCalled = true
				if settings.PanelPassword != "" || settings.AppKey != "" || settings.APIKey != "" {
					t.Fatalf("serve received uncleared secrets: panel=%q app=%q api=%q", settings.PanelPassword, settings.AppKey, settings.APIKey)
				}
				adminCount, err := repo.CountAdminUsers(ctx)
				if err != nil {
					t.Fatalf("count admins: %v", err)
				}
				if adminCount != test.wantAdminCount {
					t.Fatalf("admin count = %d; want %d", adminCount, test.wantAdminCount)
				}
				if test.wantAdminSecret != "" {
					user, found, err := repo.GetAdminUserByUsername(ctx, "admin")
					if err != nil || !found || !security.VerifyAdminPassword(user.Password, test.wantAdminSecret) {
						t.Fatalf("bootstrapped admin found=%v err=%v user=%+v", found, err, user)
					}
				}
				clientHash := sha256.Sum256([]byte(test.wantClientSecret))
				credential, found, err := repo.FindClientKeyByHash(ctx, clientHash)
				if err != nil || !found || credential.Key.Name != "legacy" {
					t.Fatalf("bootstrapped client key found=%v err=%v credential=%+v", found, err, credential.Key)
				}
				return nil
			})
			if err != nil {
				t.Fatalf("run serve: %v", err)
			}
			if !serveCalled {
				t.Fatal("serve was not called after security bootstrap")
			}
		})
	}
}

func TestNewAPIHandlerWiresPersistentSecurityAndSecureCookies(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.OpenSQLite(ctx, filepath.Join(t.TempDir(), "runtime-security.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer repo.Close()

	settings := config.Config{
		PanelPassword:      "persisted-admin-password",
		APIKey:             "persisted-client-key",
		AdminSecureCookies: true,
		DefaultModel:       "grok-4.5",
	}
	if err := bootstrapServeSecurity(ctx, &settings, repo); err != nil {
		t.Fatalf("bootstrap security: %v", err)
	}
	settings.PanelPassword = "constructor-admin-key-must-be-ignored"
	settings.APIKey = "constructor-client-key-must-be-ignored"

	adminService := admin.NewService(repo, mainTestValidator{})
	handler := newAPIHandler(
		settings,
		repo,
		mainTestGateway{},
		mainTestStatus{},
		adminService,
		nil,
		nil,
		nil,
	)

	meta := httptest.NewRecorder()
	handler.ServeHTTP(meta, httptest.NewRequest(http.MethodGet, "/api/admin/v1/system/meta", nil))
	if meta.Code != http.StatusOK || !strings.Contains(meta.Body.String(), `"setup_required":false`) {
		t.Fatalf("meta status=%d body=%s", meta.Code, meta.Body.String())
	}

	loginRequest := httptest.NewRequest(
		http.MethodPost,
		"/api/admin/v1/auth/login",
		strings.NewReader(`{"username":"admin","password":"persisted-admin-password","remember":true}`),
	)
	loginRequest.RemoteAddr = "127.0.0.1:1234"
	login := httptest.NewRecorder()
	handler.ServeHTTP(login, loginRequest)
	if login.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", login.Code, login.Body.String())
	}
	refreshCookie := responseCookie(t, login.Result(), "grok2api_admin_refresh")
	if !refreshCookie.HttpOnly || !refreshCookie.Secure || refreshCookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("refresh cookie = %+v", refreshCookie)
	}
	var loginEnvelope struct {
		Data struct {
			Tokens struct {
				AccessToken string `json:"accessToken"`
			} `json:"tokens"`
		} `json:"data"`
	}
	if err := json.Unmarshal(login.Body.Bytes(), &loginEnvelope); err != nil || loginEnvelope.Data.Tokens.AccessToken == "" {
		t.Fatalf("login envelope=%+v err=%v", loginEnvelope, err)
	}

	legacyAdmin := httptest.NewRequest(http.MethodGet, "/api/admin/v1/dashboard", nil)
	legacyAdmin.Header.Set("Authorization", "Bearer constructor-admin-key-must-be-ignored")
	legacyAdminResponse := httptest.NewRecorder()
	handler.ServeHTTP(legacyAdminResponse, legacyAdmin)
	if legacyAdminResponse.Code != http.StatusUnauthorized {
		t.Fatalf("constructor admin key status=%d body=%s", legacyAdminResponse.Code, legacyAdminResponse.Body.String())
	}

	managedAdmin := httptest.NewRequest(http.MethodGet, "/api/admin/v1/client-keys", nil)
	managedAdmin.Header.Set("Authorization", "Bearer "+loginEnvelope.Data.Tokens.AccessToken)
	managedAdminResponse := httptest.NewRecorder()
	handler.ServeHTTP(managedAdminResponse, managedAdmin)
	if managedAdminResponse.Code != http.StatusOK {
		t.Fatalf("persistent admin status=%d body=%s", managedAdminResponse.Code, managedAdminResponse.Body.String())
	}

	constructorClient := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	constructorClient.Header.Set("Authorization", "Bearer constructor-client-key-must-be-ignored")
	constructorClientResponse := httptest.NewRecorder()
	handler.ServeHTTP(constructorClientResponse, constructorClient)
	if constructorClientResponse.Code != http.StatusUnauthorized {
		t.Fatalf("constructor client key status=%d body=%s", constructorClientResponse.Code, constructorClientResponse.Body.String())
	}

	persistedClient := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	persistedClient.Header.Set("Authorization", "Bearer persisted-client-key")
	persistedClientResponse := httptest.NewRecorder()
	handler.ServeHTTP(persistedClientResponse, persistedClient)
	if persistedClientResponse.Code != http.StatusOK {
		t.Fatalf("persistent client key status=%d body=%s", persistedClientResponse.Code, persistedClientResponse.Body.String())
	}
}

type mainTestGateway struct{}

func (mainTestGateway) Chat(context.Context, []byte, bool) (service.ChatResult, error) {
	return service.ChatResult{Status: http.StatusOK, Header: make(http.Header), Body: []byte(`{"ok":true}`)}, nil
}

func (mainTestGateway) Request(context.Context, string, string, []byte, bool) (service.ChatResult, error) {
	return service.ChatResult{Status: http.StatusOK, Header: make(http.Header), Body: []byte(`{"ok":true}`)}, nil
}

type mainTestStatus struct{}

func (mainTestStatus) PoolStatus() api.PoolStatus {
	return api.PoolStatus{Reasons: map[string]int{}}
}

type mainTestValidator struct{}

func (mainTestValidator) Validate(context.Context, account.Account) (account.UnavailableReason, string, error) {
	return "", "", nil
}

func responseCookie(t *testing.T, response *http.Response, name string) *http.Cookie {
	t.Helper()
	for _, cookie := range response.Cookies() {
		if cookie.Name == name {
			return cookie
		}
	}
	t.Fatalf("cookie %q not found", name)
	return nil
}

func TestRunMigrateImportsLegacyAccountsAndPrintsPoolCounts(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	legacyPath := filepath.Join(dir, "cli_accounts.json")
	configData, _ := json.Marshal(map[string]any{
		"data_dir": dir,
	})
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	legacyData, _ := json.Marshal(map[string]any{
		"accounts": []map[string]any{
			{"id": "ready", "key": "ready-token", "enabled": true},
			{"id": "quota", "key": "quota-token", "enabled": false, "fail_count": 5, "expires_at": future},
		},
	})
	if err := os.WriteFile(legacyPath, legacyData, 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	var output bytes.Buffer
	err := run(
		context.Background(),
		[]string{"migrate", "--config", configPath},
		&output,
	)
	if err != nil {
		t.Fatalf("run migrate: %v", err)
	}
	if !strings.Contains(output.String(), `"ready":1`) {
		t.Fatalf("output = %s", output.String())
	}
	if !strings.Contains(output.String(), `"unavailable":1`) {
		t.Fatalf("output = %s", output.String())
	}
}

func TestFrontendFileSystemDisabledWhenPathEmpty(t *testing.T) {
	frontendFS, err := frontendFileSystem("   ")
	if err != nil {
		t.Fatalf("frontend filesystem: %v", err)
	}
	if frontendFS != nil {
		t.Fatal("expected nil filesystem when frontend is disabled")
	}
}

func TestFrontendFileSystemValidatesIndexAndReturnsDistRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("panel"), 0o600); err != nil {
		t.Fatalf("write index: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "assets"), 0o700); err != nil {
		t.Fatalf("mkdir assets: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "app.js"), []byte("app"), 0o600); err != nil {
		t.Fatalf("write asset: %v", err)
	}

	frontendFS, err := frontendFileSystem(dir)
	if err != nil {
		t.Fatalf("frontend filesystem: %v", err)
	}
	data, err := fs.ReadFile(frontendFS, "assets/app.js")
	if err != nil {
		t.Fatalf("read injected asset: %v", err)
	}
	if string(data) != "app" {
		t.Fatalf("asset=%q", data)
	}
}

func TestFrontendFileSystemRejectsMissingIndex(t *testing.T) {
	_, err := frontendFileSystem(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "index.html") {
		t.Fatalf("error=%v, want missing index.html", err)
	}
}

func TestRunMigrateIsIdempotentAndEncryptsImportedCredentials(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	legacyPath := filepath.Join(dir, "cli_accounts.json")
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x37}, 32))
	configData, err := json.Marshal(map[string]any{
		"data_dir":       dir,
		"credential_key": key,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	legacyData, err := json.Marshal(map[string]any{
		"accounts": []map[string]any{
			{"id": "encrypted-ready", "key": "ready-secret", "refresh_token": "ready-refresh", "enabled": true},
			{"id": "encrypted-quota", "key": "quota-secret", "enabled": false, "fail_count": 5},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, legacyData, 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		var output bytes.Buffer
		if err := run(context.Background(), []string{"migrate", "--config", configPath}, &output); err != nil {
			t.Fatalf("migrate attempt %d: %v", attempt, err)
		}
		if !strings.Contains(output.String(), `"ready":1`) || !strings.Contains(output.String(), `"unavailable":1`) {
			t.Fatalf("attempt %d output = %s", attempt, output.String())
		}
	}

	db, err := sql.Open("sqlite", filepath.Join(dir, "grok2api.db"))
	if err != nil {
		t.Fatalf("open migrated database: %v", err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT access_token, refresh_token FROM accounts ORDER BY id`)
	if err != nil {
		t.Fatalf("query credentials: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var access, refresh string
		if err := rows.Scan(&access, &refresh); err != nil {
			t.Fatalf("scan credentials: %v", err)
		}
		if !security.IsEncrypted(access) {
			t.Fatalf("access token stored as plaintext: %q", access)
		}
		if refresh != "" && !security.IsEncrypted(refresh) {
			t.Fatalf("refresh token stored as plaintext: %q", refresh)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate credentials: %v", err)
	}
	if count != 2 {
		t.Fatalf("stored account count = %d; want 2", count)
	}
}
