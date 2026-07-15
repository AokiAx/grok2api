package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	service "github.com/AokiAx/grok2api/backend/internal/adminauth"
	domain "github.com/AokiAx/grok2api/backend/internal/domain/adminauth"
	"github.com/AokiAx/grok2api/backend/internal/infra/persistence/sqlite"
	"github.com/AokiAx/grok2api/backend/internal/security"
)

type authHTTPResult struct {
	Access string
	Cookie *http.Cookie
	Rec    *httptest.ResponseRecorder
}

func TestAdminAuthSQLiteRefreshRotationReplayAndCookies(t *testing.T) {
	now := time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	handler, repo, clock := newSQLiteAuthHandlerWithClock(t, now, true, true)
	defer repo.Close()

	sessionLogin := authLogin(t, handler, false)
	if !sessionLogin.Cookie.Expires.IsZero() || sessionLogin.Cookie.MaxAge != 0 {
		t.Fatalf("session cookie=%+v", sessionLogin.Cookie)
	}
	persistentLogin := authLogin(t, handler, true)
	if !persistentLogin.Cookie.Secure || !persistentLogin.Cookie.HttpOnly || persistentLogin.Cookie.SameSite != http.SameSiteStrictMode || persistentLogin.Cookie.Path != "/api/admin/v1/auth" {
		t.Fatalf("persistent cookie flags=%+v", persistentLogin.Cookie)
	}
	if persistentLogin.Cookie.MaxAge != int((30*24*time.Hour).Seconds()) || !persistentLogin.Cookie.Expires.Equal(now.Add(30*24*time.Hour)) {
		t.Fatalf("persistent cookie lifetime=%+v", persistentLogin.Cookie)
	}

	refresh := authRefresh(handler, persistentLogin.Cookie)
	assertAuthResponse(t, refresh, http.StatusOK, true, "")
	refreshed := decodeAuthHTTPResult(t, refresh)
	if refreshed.Access == persistentLogin.Access || refreshed.Cookie.Value == persistentLogin.Cookie.Value {
		t.Fatal("refresh did not rotate credentials")
	}
	if refreshed.Cookie.MaxAge != persistentLogin.Cookie.MaxAge || !refreshed.Cookie.Expires.Equal(persistentLogin.Cookie.Expires) {
		t.Fatalf("refresh did not inherit remember lifetime old=%+v new=%+v", persistentLogin.Cookie, refreshed.Cookie)
	}
	assertMe(t, handler, persistentLogin.Access, http.StatusUnauthorized)
	concurrentLoser := authRefresh(handler, persistentLogin.Cookie)
	assertAuthResponse(t, concurrentLoser, http.StatusConflict, false, "refresh_conflict")
	if concurrentLoser.Header().Get("Set-Cookie") != "" {
		t.Fatalf("conflict changed cookie: %q", concurrentLoser.Header().Get("Set-Cookie"))
	}
	assertMe(t, handler, refreshed.Access, http.StatusOK)

	clock.Advance(6 * time.Second)
	oldRefresh := authRefresh(handler, persistentLogin.Cookie)
	assertAuthResponse(t, oldRefresh, http.StatusUnauthorized, false, "invalid_refresh_session")
	assertMe(t, handler, refreshed.Access, http.StatusUnauthorized)
	assertAuthResponse(t, authRefresh(handler, refreshed.Cookie), http.StatusUnauthorized, false, "invalid_refresh_session")
}

func TestAdminAuthSQLiteConcurrentRefreshHasOneWinnerAndConflictKeepsCookie(t *testing.T) {
	now := time.Date(2026, 7, 15, 2, 0, 0, 0, time.UTC)
	handler, repo := newSQLiteAuthHandler(t, now, true, true)
	defer repo.Close()
	login := authLogin(t, handler, true)
	start := make(chan struct{})
	recorders := make([]*httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for i := range recorders {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			recorders[index] = authRefresh(handler, login.Cookie)
		}(i)
	}
	close(start)
	wg.Wait()
	statuses := map[int]int{}
	for _, rec := range recorders {
		statuses[rec.Code]++
		assertCommonAuthHeaders(t, rec)
		if rec.Code == http.StatusConflict && rec.Header().Get("Set-Cookie") != "" {
			t.Fatalf("CAS loser deleted/replaced cookie: %q", rec.Header().Get("Set-Cookie"))
		}
	}
	if statuses[http.StatusOK] != 1 || statuses[http.StatusConflict] != 1 {
		t.Fatalf("refresh statuses=%v", statuses)
	}
}

func TestAdminAuthSQLiteStatusHeadersThrottleAndLogout(t *testing.T) {
	now := time.Date(2026, 7, 15, 3, 0, 0, 0, time.UTC)
	t.Run("setup required", func(t *testing.T) {
		handler, repo := newSQLiteAuthHandler(t, now, false, false)
		defer repo.Close()
		rec := rawLogin(handler, `{"username":"admin","password":"secret"}`)
		assertAuthResponse(t, rec, http.StatusServiceUnavailable, false, "setup_required")
	})
	t.Run("invalid and exact throttle", func(t *testing.T) {
		handler, repo := newSQLiteAuthHandler(t, now, true, false)
		defer repo.Close()
		for i := 0; i < 6; i++ {
			rec := rawLogin(handler, `{"username":"admin","password":"bad"}`)
			if i < 5 {
				assertAuthResponse(t, rec, http.StatusUnauthorized, false, "invalid_credentials")
				if rec.Header().Get("WWW-Authenticate") != `Bearer realm="admin"` {
					t.Fatalf("challenge=%q", rec.Header().Get("WWW-Authenticate"))
				}
			} else {
				assertAuthResponse(t, rec, http.StatusTooManyRequests, false, "login_rate_limited")
				if rec.Header().Get("Retry-After") != "900" {
					t.Fatalf("Retry-After=%q", rec.Header().Get("Retry-After"))
				}
			}
		}
	})
	t.Run("internal", func(t *testing.T) {
		handler, repo := newSQLiteAuthHandler(t, now, true, false)
		if err := repo.Close(); err != nil {
			t.Fatal(err)
		}
		assertAuthResponse(t, rawLogin(handler, `{"username":"admin","password":"secret"}`), http.StatusInternalServerError, false, "internal_error")
	})
	t.Run("bearer and cookie logout", func(t *testing.T) {
		for _, credential := range []string{"bearer", "cookie"} {
			t.Run(credential, func(t *testing.T) {
				handler, repo := newSQLiteAuthHandler(t, now, true, true)
				defer repo.Close()
				login := authLogin(t, handler, true)
				req := httptest.NewRequest(http.MethodPost, "/api/admin/v1/auth/logout", nil)
				if credential == "bearer" {
					req.Header.Set("Authorization", "Bearer "+login.Access)
				} else {
					req.AddCookie(login.Cookie)
				}
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				assertAuthResponse(t, rec, http.StatusOK, true, "")
				if !strings.Contains(rec.Header().Get("Set-Cookie"), "Max-Age=0") {
					t.Fatalf("delete cookie=%q", rec.Header().Get("Set-Cookie"))
				}
				assertMe(t, handler, login.Access, http.StatusUnauthorized)
			})
		}
	})
}

func newSQLiteAuthHandler(t *testing.T, now time.Time, withAdmin, secure bool) (http.Handler, *sqlite.SQLite) {
	handler, repo, _ := newSQLiteAuthHandlerWithClock(t, now, withAdmin, secure)
	return handler, repo
}

type authTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *authTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *authTestClock) Advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(duration)
}

func newSQLiteAuthHandlerWithClock(t *testing.T, now time.Time, withAdmin, secure bool) (http.Handler, *sqlite.SQLite, *authTestClock) {
	t.Helper()
	repo, err := sqlite.OpenSQLite(context.Background(), t.TempDir()+"/auth-integration.db")
	if err != nil {
		t.Fatal(err)
	}
	if withAdmin {
		cred, err := security.HashAdminPassword("secret", 4)
		if err != nil {
			t.Fatal(err)
		}
		user, err := domain.NewAdminUser("admin-1", "admin", cred, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := repo.CreateAdminUser(context.Background(), user); err != nil {
			t.Fatal(err)
		}
	}
	clock := &authTestClock{now: now}
	svc := service.NewService(repo, service.WithClock(clock.Now))
	return NewAdminAuthHandler(svc, AdminAuthHandlerOptions{SecureCookies: secure, Clock: clock.Now}), repo, clock
}

func rawLogin(handler http.Handler, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/admin/v1/auth/login", strings.NewReader(body))
	req.RemoteAddr = "[2001:db8::1]:1234"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func authLogin(t *testing.T, handler http.Handler, remember bool) authHTTPResult {
	t.Helper()
	rec := rawLogin(handler, `{"username":"admin","password":"secret","remember":`+map[bool]string{true: "true", false: "false"}[remember]+`}`)
	assertAuthResponse(t, rec, http.StatusOK, true, "")
	return decodeAuthHTTPResult(t, rec)
}

func authRefresh(handler http.Handler, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/admin/v1/auth/refresh", nil)
	req.RemoteAddr = "127.0.0.1:1234"
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func decodeAuthHTTPResult(t *testing.T, rec *httptest.ResponseRecorder) authHTTPResult {
	t.Helper()
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
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies=%v", cookies)
	}
	return authHTTPResult{Access: body.Data.Tokens.Access, Cookie: cookies[0], Rec: rec}
}

func assertMe(t *testing.T, handler http.Handler, access string, status int) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/v1/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assertAuthResponse(t, rec, status, status == http.StatusOK, map[bool]string{true: "", false: "unauthorized"}[status == http.StatusOK])
}

func assertCommonAuthHeaders(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("headers=%v", rec.Header())
	}
}

func assertAuthResponse(t *testing.T, rec *httptest.ResponseRecorder, status int, ok bool, code string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status=%d want=%d body=%s", rec.Code, status, rec.Body.String())
	}
	assertCommonAuthHeaders(t, rec)
	var envelope struct {
		OK    bool            `json:"ok"`
		Data  json.RawMessage `json:"data"`
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("invalid envelope: %v body=%s", err, rec.Body.String())
	}
	if envelope.OK != ok {
		t.Fatalf("envelope=%s", rec.Body.String())
	}
	if ok && len(envelope.Data) == 0 {
		t.Fatalf("missing data: %s", rec.Body.String())
	}
	if !ok && (envelope.Error == nil || envelope.Error.Code != code) {
		t.Fatalf("error envelope=%s", rec.Body.String())
	}
}
