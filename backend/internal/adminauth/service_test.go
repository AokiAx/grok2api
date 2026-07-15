package adminauth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/adminauth"
	"github.com/AokiAx/grok2api/backend/internal/security"
)

type fakeRepo struct {
	users                map[string]adminauth.AdminUser
	sessions             map[string]adminauth.Session
	attempts             []adminauth.LoginAttempt
	rotateOK             bool
	createWithSuccessErr error
	recordAttemptErr     error
	getUserByIDErr       error
	getSessionErr        error
	rotateCalls          int
	revokeFamilyCalls    int
}

func (f *fakeRepo) CountAdminUsers(context.Context) (int, error)               { return len(f.users), nil }
func (f *fakeRepo) CreateAdminUser(context.Context, adminauth.AdminUser) error { return nil }
func (f *fakeRepo) GetAdminUserByID(_ context.Context, id string) (adminauth.AdminUser, bool, error) {
	if f.getUserByIDErr != nil {
		return adminauth.AdminUser{}, false, f.getUserByIDErr
	}
	u, ok := f.users[id]
	return u, ok, nil
}
func (f *fakeRepo) GetAdminUserByUsername(_ context.Context, n string) (adminauth.AdminUser, bool, error) {
	for _, u := range f.users {
		if u.Username == n {
			return u, true, nil
		}
	}
	return adminauth.AdminUser{}, false, nil
}
func (f *fakeRepo) CreateAdminSession(_ context.Context, s adminauth.Session) error {
	f.sessions[s.ID] = s
	return nil
}
func (f *fakeRepo) CreateAdminSessionWithLoginSuccess(_ context.Context, s adminauth.Session, a adminauth.LoginAttempt) error {
	if f.createWithSuccessErr != nil {
		return f.createWithSuccessErr
	}
	f.sessions[s.ID] = s
	f.attempts = append(f.attempts, a)
	return nil
}
func (f *fakeRepo) GetAdminSession(_ context.Context, id string) (adminauth.Session, bool, error) {
	if f.getSessionErr != nil {
		return adminauth.Session{}, false, f.getSessionErr
	}
	s, ok := f.sessions[id]
	return s, ok, nil
}

func TestLogoutReturnsSessionLookupFailure(t *testing.T) {
	repo := &fakeRepo{users: map[string]adminauth.AdminUser{}, sessions: map[string]adminauth.Session{}, getSessionErr: errors.New("lookup failed")}
	svc := NewService(repo)
	if err := svc.Logout(context.Background(), "session.secret"); err == nil || err.Error() != "lookup failed" {
		t.Fatalf("logout err=%v", err)
	}
}
func (f *fakeRepo) FindAdminSessionByAccessHash(_ context.Context, h [32]byte) (adminauth.Session, bool, error) {
	for _, s := range f.sessions {
		if s.MatchesAccessTokenHash(h) {
			return s, true, nil
		}
	}
	return adminauth.Session{}, false, nil
}
func (f *fakeRepo) RotateAdminSession(_ context.Context, id string, h [32]byte, r adminauth.Session, at time.Time) (bool, error) {
	f.rotateCalls++
	if !f.rotateOK {
		return false, nil
	}
	s, ok := f.sessions[id]
	if !ok || !s.MatchesRefreshSecretHash(h) {
		return false, nil
	}
	f.sessions[id] = s
	f.sessions[r.ID] = r
	return true, nil
}
func (f *fakeRepo) RevokeAdminSession(_ context.Context, id string, _ time.Time, _ adminauth.RevocationReason) error {
	delete(f.sessions, id)
	return nil
}
func (f *fakeRepo) RevokeAdminSessionFamily(context.Context, string, time.Time, adminauth.RevocationReason) error {
	f.revokeFamilyCalls++
	return nil
}
func (f *fakeRepo) RecordAdminLoginAttempt(_ context.Context, a adminauth.LoginAttempt) error {
	if f.recordAttemptErr != nil {
		return f.recordAttemptErr
	}
	f.attempts = append(f.attempts, a)
	return nil
}
func (f *fakeRepo) CountRecentAdminLoginFailures(_ context.Context, _ string, _ string, _ time.Time) (int, error) {
	return len(f.attempts), nil
}

func TestLoginPersistsSessionAndSuccessAtomically(t *testing.T) {
	cred, _ := security.HashAdminPassword("secret", 4)
	now := time.Unix(3000, 0).UTC()
	repo := &fakeRepo{
		users: map[string]adminauth.AdminUser{}, sessions: map[string]adminauth.Session{},
		createWithSuccessErr: errors.New("commit failed"),
	}
	u, _ := adminauth.NewAdminUser("u1", "admin", cred, now)
	repo.users[u.ID] = u
	svc := NewService(repo, WithClock(func() time.Time { return now }), WithRandom(func([]byte) error { return nil }))
	if _, err := svc.Login(context.Background(), LoginInput{Username: "admin", Password: "secret", SourceIP: "127.0.0.1"}); err == nil || err.Error() != "commit failed" {
		t.Fatalf("login err=%v", err)
	}
	if len(repo.sessions) != 0 || len(repo.attempts) != 0 {
		t.Fatalf("partial success persisted sessions=%d attempts=%d", len(repo.sessions), len(repo.attempts))
	}
}

func TestRefreshClassifiesRecentRotationAsConflictAndExpiredGraceAsReplay(t *testing.T) {
	now := time.Date(2026, 7, 15, 4, 0, 0, 0, time.UTC)
	credential, err := security.HashAdminPassword("secret", 4)
	if err != nil {
		t.Fatal(err)
	}
	user, err := adminauth.NewAdminUser("admin-1", "admin", credential, now)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name          string
		rotatedAt     time.Time
		wantError     error
		wantRevokes   int
		wantRotations int
	}{
		{name: "inside grace", rotatedAt: now.Add(-2 * time.Second), wantError: ErrConflict},
		{name: "at grace deadline", rotatedAt: now.Add(-5 * time.Second), wantError: ErrInvalidRefresh, wantRevokes: 1},
		{name: "after grace deadline", rotatedAt: now.Add(-5*time.Second - time.Nanosecond), wantError: ErrInvalidRefresh, wantRevokes: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &fakeRepo{
				users:    map[string]adminauth.AdminUser{user.ID: user},
				sessions: map[string]adminauth.Session{},
				rotateOK: true,
			}
			service := NewService(repo, WithClock(func() time.Time { return now }))
			login, err := service.Login(context.Background(), LoginInput{
				Username: "admin", Password: "secret", SourceIP: "127.0.0.1",
			})
			if err != nil {
				t.Fatalf("login: %v", err)
			}
			sessionID := strings.SplitN(login.RefreshCookieValue, ".", 2)[0]
			session := repo.sessions[sessionID]
			session.RotatedAt = test.rotatedAt
			session.RevokedAt = test.rotatedAt
			session.RevocationReason = adminauth.RevocationRotated
			repo.sessions[sessionID] = session

			_, err = service.Refresh(context.Background(), login.RefreshCookieValue, "127.0.0.1", "ua")
			if !errors.Is(err, test.wantError) {
				t.Fatalf("refresh err=%v want=%v", err, test.wantError)
			}
			if repo.revokeFamilyCalls != test.wantRevokes || repo.rotateCalls != test.wantRotations {
				t.Fatalf("family revokes=%d rotations=%d", repo.revokeFamilyCalls, repo.rotateCalls)
			}
		})
	}
}

func TestLoginReturnsAttemptPersistenceFailure(t *testing.T) {
	cred, _ := security.HashAdminPassword("secret", 4)
	now := time.Unix(3000, 0).UTC()
	repo := &fakeRepo{users: map[string]adminauth.AdminUser{}, sessions: map[string]adminauth.Session{}, recordAttemptErr: errors.New("attempt write failed")}
	u, _ := adminauth.NewAdminUser("u1", "admin", cred, now)
	repo.users[u.ID] = u
	svc := NewService(repo, WithClock(func() time.Time { return now }))
	if _, err := svc.Login(context.Background(), LoginInput{Username: "admin", Password: "bad", SourceIP: "127.0.0.1"}); err == nil || err.Error() != "attempt write failed" {
		t.Fatalf("login err=%v", err)
	}
}

func TestRefreshChecksAdminBeforeRotation(t *testing.T) {
	cred, _ := security.HashAdminPassword("secret", 4)
	now := time.Unix(4000, 0).UTC()
	for _, tc := range []struct {
		name      string
		disabled  bool
		lookupErr error
	}{
		{name: "disabled", disabled: true},
		{name: "lookup error", lookupErr: errors.New("lookup failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepo{users: map[string]adminauth.AdminUser{}, sessions: map[string]adminauth.Session{}, rotateOK: true, getUserByIDErr: tc.lookupErr}
			u, _ := adminauth.NewAdminUser("u1", "admin", cred, now)
			if tc.disabled {
				u.Enabled = false
			}
			repo.users[u.ID] = u
			svc := NewService(repo, WithClock(func() time.Time { return now }))
			if tc.lookupErr != nil {
				repo.getUserByIDErr = nil
			}
			login, err := svc.Login(context.Background(), LoginInput{Username: "admin", Password: "secret", SourceIP: "127.0.0.1"})
			if tc.disabled {
				u.Enabled = true
				repo.users[u.ID] = u
				login, err = svc.Login(context.Background(), LoginInput{Username: "admin", Password: "secret", SourceIP: "127.0.0.1"})
			}
			if err != nil {
				t.Fatalf("seed login: %v", err)
			}
			if tc.disabled {
				u.Enabled = false
				repo.users[u.ID] = u
			}
			repo.getUserByIDErr = tc.lookupErr
			_, err = svc.Refresh(context.Background(), login.RefreshCookieValue, "127.0.0.1", "ua")
			if tc.lookupErr != nil && (err == nil || err.Error() != tc.lookupErr.Error()) {
				t.Fatalf("refresh err=%v", err)
			}
			if tc.disabled && !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("refresh err=%v", err)
			}
			if repo.rotateCalls != 0 {
				t.Fatalf("rotate calls=%d", repo.rotateCalls)
			}
		})
	}
}
func (f *fakeRepo) OldestRecentAdminLoginFailure(_ context.Context, _ string, _ string, _ time.Time) (time.Time, bool, error) {
	if len(f.attempts) == 0 {
		return time.Time{}, false, nil
	}
	return f.attempts[0].CreatedAt, true, nil
}

func TestLoginIssuesOpaqueTokensAndRejectsBadPassword(t *testing.T) {
	cred, _ := security.HashAdminPassword("secret", 4)
	now := time.Unix(1000, 0).UTC()
	repo := &fakeRepo{users: map[string]adminauth.AdminUser{}, sessions: map[string]adminauth.Session{}}
	u, _ := adminauth.NewAdminUser("u1", "admin", cred, now)
	repo.users[u.ID] = u
	svc := NewService(repo, WithClock(func() time.Time { return now }), WithRandom(func([]byte) error { return nil }), WithBcryptCost(4))
	out, err := svc.Login(context.Background(), LoginInput{Username: "admin", Password: "secret", SourceIP: "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.AccessToken == "" || out.RefreshCookieValue == "" {
		t.Fatalf("tokens missing %#v", out)
	}
	if out.AccessToken == out.RefreshCookieValue {
		t.Fatal("tokens must differ")
	}
	if _, err := svc.Login(context.Background(), LoginInput{Username: "admin", Password: "bad", SourceIP: "127.0.0.1"}); err != ErrInvalidCredentials {
		t.Fatalf("err=%v", err)
	}
}

func TestLoginSetupRequiredAndThrottle(t *testing.T) {
	repo := &fakeRepo{users: map[string]adminauth.AdminUser{}, sessions: map[string]adminauth.Session{}}
	svc := NewService(repo)
	if _, err := svc.Login(context.Background(), LoginInput{Username: "a", Password: "b", SourceIP: "127.0.0.1"}); err != ErrSetupRequired {
		t.Fatalf("%v", err)
	}
	cred, _ := security.HashAdminPassword("secret", 4)
	now := time.Now()
	u, _ := adminauth.NewAdminUser("u", "a", cred, now)
	repo.users[u.ID] = u
	for i := 0; i < 6; i++ {
		_, err := svc.Login(context.Background(), LoginInput{Username: "a", Password: "bad", SourceIP: "127.0.0.1"})
		if i < 5 && err != ErrInvalidCredentials {
			t.Fatalf("attempt %d err %v", i, err)
		}
		if i == 5 && !errors.Is(err, ErrRateLimited) {
			t.Fatalf("attempt 6 err %v", err)
		}
	}
}

func TestRememberPersistsAndSurvivesRefresh(t *testing.T) {
	cred, _ := security.HashAdminPassword("secret", 4)
	now := time.Unix(2000, 0).UTC()
	repo := &fakeRepo{users: map[string]adminauth.AdminUser{}, sessions: map[string]adminauth.Session{}, rotateOK: true}
	u, _ := adminauth.NewAdminUser("u1", "admin", cred, now)
	repo.users[u.ID] = u
	svc := NewService(repo, WithClock(func() time.Time { return now }))
	out, err := svc.Login(context.Background(), LoginInput{Username: "admin", Password: "secret", SourceIP: "127.0.0.1", Remember: true})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Remember {
		t.Fatal("login output lost remember")
	}
	parts := strings.SplitN(out.RefreshCookieValue, ".", 2)
	stored := repo.sessions[parts[0]]
	if !stored.Remember {
		t.Fatal("session did not persist remember")
	}
	refreshed, err := svc.Refresh(context.Background(), out.RefreshCookieValue, "127.0.0.1", "ua")
	if err != nil {
		t.Fatal(err)
	}
	if !refreshed.Remember {
		t.Fatal("refresh did not inherit remember")
	}
}
