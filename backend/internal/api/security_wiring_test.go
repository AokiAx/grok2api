package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	authservice "github.com/AokiAx/grok2api/backend/internal/adminauth"
	"github.com/AokiAx/grok2api/backend/internal/api"
	"github.com/AokiAx/grok2api/backend/internal/clientkeys"
	adminauth "github.com/AokiAx/grok2api/backend/internal/domain/adminauth"
	"github.com/AokiAx/grok2api/backend/internal/domain/clientkey"
	"github.com/AokiAx/grok2api/backend/internal/infra/persistence/sqlite"
	"github.com/AokiAx/grok2api/backend/internal/security"
	"github.com/AokiAx/grok2api/backend/internal/service"
	"golang.org/x/crypto/bcrypt"
)

func TestServerWiresPersistentAdminAndClientAuthentication(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.OpenSQLite(ctx, t.TempDir()+"/security.db")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer repo.Close()

	now := time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC)
	password, err := security.HashAdminPassword("admin-password", bcrypt.MinCost)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	adminUser, err := adminauth.NewAdminUser("admin-1", "admin", password, now)
	if err != nil {
		t.Fatalf("new admin: %v", err)
	}
	if err := repo.CreateAdminUser(ctx, adminUser); err != nil {
		t.Fatalf("create admin: %v", err)
	}

	clientSecret := "g2a_managed_client_secret"
	clientCredential, err := clientkey.NewCredential(clientkey.ClientKey{
		ID: "client-1", Name: "managed", Origin: clientkey.OriginManaged,
		KeyHash: sha256.Sum256([]byte(clientSecret)), KeyPrefix: "g2a_managed",
		ModelPolicy: clientkey.ModelPolicyAll, CreatedAt: now, UpdatedAt: now,
	}, nil)
	if err != nil {
		t.Fatalf("new client credential: %v", err)
	}
	if err := repo.CreateClientKey(ctx, clientCredential); err != nil {
		t.Fatalf("create client key: %v", err)
	}

	adminAuth := authservice.NewService(repo, authservice.WithClock(func() time.Time { return now }))
	clientAccess := service.NewClientAccess(repo, service.WithClientAccessClock(func() time.Time { return now }))
	clientKeys := clientkeys.NewService(repo, clientkeys.WithClock(func() time.Time { return now }))
	gateway := &fakeGateway{requestResult: service.ChatResult{
		Status: http.StatusOK,
		Header: make(http.Header),
		Body:   []byte(`{"ok":true}`),
	}}
	server := api.NewServer(
		gateway,
		fakeStatus{},
		"legacy-config-api-key-must-be-ignored",
		api.WithAdmin(&fakeAdmin{}, "legacy-admin-key-must-be-ignored"),
		api.WithAdminAuth(adminAuth, api.AdminAuthHandlerOptions{Clock: func() time.Time { return now }}),
		api.WithClientAccess(clientAccess),
		api.WithClientKeys(clientKeys),
	)

	meta := httptest.NewRecorder()
	server.Handler().ServeHTTP(meta, httptest.NewRequest(http.MethodGet, "/api/admin/v1/system/meta", nil))
	if meta.Code != http.StatusOK || !strings.Contains(meta.Body.String(), `"setup_required":false`) {
		t.Fatalf("meta status=%d body=%s", meta.Code, meta.Body.String())
	}

	loginRequest := httptest.NewRequest(
		http.MethodPost,
		"/api/admin/v1/auth/login",
		strings.NewReader(`{"username":"admin","password":"admin-password","remember":false}`),
	)
	loginRequest.RemoteAddr = "127.0.0.1:1234"
	login := httptest.NewRecorder()
	server.Handler().ServeHTTP(login, loginRequest)
	if login.Code != http.StatusOK {
		t.Fatalf("login status=%d body=%s", login.Code, login.Body.String())
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
	legacyAdmin.Header.Set("Authorization", "Bearer legacy-admin-key-must-be-ignored")
	legacyAdminResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(legacyAdminResponse, legacyAdmin)
	if legacyAdminResponse.Code != http.StatusUnauthorized {
		t.Fatalf("legacy admin key status=%d body=%s", legacyAdminResponse.Code, legacyAdminResponse.Body.String())
	}

	authorizedAdmin := httptest.NewRequest(http.MethodGet, "/api/admin/v1/client-keys", nil)
	authorizedAdmin.Header.Set("Authorization", "Bearer "+loginEnvelope.Data.Tokens.AccessToken)
	authorizedAdminResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(authorizedAdminResponse, authorizedAdmin)
	if authorizedAdminResponse.Code != http.StatusOK {
		t.Fatalf("client key admin status=%d body=%s", authorizedAdminResponse.Code, authorizedAdminResponse.Body.String())
	}

	legacyClient := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	legacyClient.Header.Set("Authorization", "Bearer legacy-config-api-key-must-be-ignored")
	legacyClientResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(legacyClientResponse, legacyClient)
	if legacyClientResponse.Code != http.StatusUnauthorized {
		t.Fatalf("legacy constructor key status=%d body=%s", legacyClientResponse.Code, legacyClientResponse.Body.String())
	}

	managedClient := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	managedClient.Header.Set("Authorization", "Bearer "+clientSecret)
	managedClientResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(managedClientResponse, managedClient)
	if managedClientResponse.Code != http.StatusOK {
		t.Fatalf("managed client status=%d body=%s", managedClientResponse.Code, managedClientResponse.Body.String())
	}
}

func TestServerMetaRequiresAdminSetupWithoutLegacyOpenMode(t *testing.T) {
	ctx := context.Background()
	repo, err := sqlite.OpenSQLite(ctx, t.TempDir()+"/setup.db")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer repo.Close()

	server := api.NewServer(
		&fakeGateway{},
		fakeStatus{},
		"",
		api.WithAdminAuth(authservice.NewService(repo), api.AdminAuthHandlerOptions{}),
	)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/v1/system/meta", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"auth_required":true`) || !strings.Contains(recorder.Body.String(), `"setup_required":true`) {
		t.Fatalf("meta status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
