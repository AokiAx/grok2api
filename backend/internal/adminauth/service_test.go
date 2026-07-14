package adminauth

import (
	"context"
	"github.com/AokiAx/grok2api/backend/internal/domain/adminauth"
	"github.com/AokiAx/grok2api/backend/internal/security"
	"strings"
	"testing"
	"time"
)

type fakeRepo struct {
	users    map[string]adminauth.AdminUser
	sessions map[string]adminauth.Session
	attempts []adminauth.LoginAttempt
	rotateOK bool
}

func (f *fakeRepo) CountAdminUsers(context.Context) (int, error)               { return len(f.users), nil }
func (f *fakeRepo) CreateAdminUser(context.Context, adminauth.AdminUser) error { return nil }
func (f *fakeRepo) GetAdminUserByID(_ context.Context, id string) (adminauth.AdminUser, bool, error) {
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
func (f *fakeRepo) GetAdminSession(_ context.Context, id string) (adminauth.Session, bool, error) {
	s, ok := f.sessions[id]
	return s, ok, nil
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
	return nil
}
func (f *fakeRepo) RecordAdminLoginAttempt(_ context.Context, a adminauth.LoginAttempt) error {
	f.attempts = append(f.attempts, a)
	return nil
}
func (f *fakeRepo) CountRecentAdminLoginFailures(_ context.Context, _ string, _ string, _ time.Time) (int, error) {
	return len(f.attempts), nil
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
		if i == 5 && err != ErrRateLimited {
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
