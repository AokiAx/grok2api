package deviceauth_test

import (
	"context"
	"testing"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/deviceauth"
	"github.com/AokiAx/grok2api/backend/internal/domain/account"
	domain "github.com/AokiAx/grok2api/backend/internal/domain/deviceauth"
	"github.com/AokiAx/grok2api/backend/internal/upstream"
)

type memStore struct {
	items map[string]domain.Session
}

func (m *memStore) CreateDeviceAuthSession(_ context.Context, item domain.Session) error {
	if m.items == nil {
		m.items = map[string]domain.Session{}
	}
	m.items[item.ID] = item
	return nil
}
func (m *memStore) GetDeviceAuthSession(_ context.Context, id string) (domain.Session, bool, error) {
	item, ok := m.items[id]
	return item, ok, nil
}
func (m *memStore) UpdateDeviceAuthSession(_ context.Context, item domain.Session) error {
	m.items[item.ID] = item
	return nil
}
func (m *memStore) ListDeviceAuthSessions(_ context.Context, _ int) ([]domain.Session, error) {
	out := make([]domain.Session, 0, len(m.items))
	for _, item := range m.items {
		out = append(out, item)
	}
	return out, nil
}

type fakeOIDC struct {
	pollCount int
}

func (f *fakeOIDC) StartDeviceAuthorization(context.Context, string, string, string) (upstream.DeviceAuthorization, error) {
	return upstream.DeviceAuthorization{
		DeviceCode: "dev-code-secret", UserCode: "ABCD-EFGH",
		VerificationURI: "https://auth.example/device", ExpiresIn: time.Minute, Interval: 5 * time.Second,
	}, nil
}
func (f *fakeOIDC) PollDeviceToken(context.Context, string, string, string) (upstream.DeviceTokenResult, error) {
	f.pollCount++
	if f.pollCount < 2 {
		return upstream.DeviceTokenResult{Pending: true}, nil
	}
	return upstream.DeviceTokenResult{AccessToken: "access", RefreshToken: "refresh", ExpiresIn: time.Hour}, nil
}

type accountStore struct{ saved []account.Account }

func (a *accountStore) SaveAccount(_ context.Context, item account.Account) error {
	a.saved = append(a.saved, item)
	return nil
}

type sink struct{ items []account.Account }

func (s *sink) Upsert(item account.Account) { s.items = append(s.items, item) }

func TestDeviceAuthStartPollSucceedsWithoutLeakingDeviceCode(t *testing.T) {
	store := &memStore{}
	oidc := &fakeOIDC{}
	accounts := &accountStore{}
	pool := &sink{}
	svc := deviceauth.NewService(store, oidc, oidc, nil, accounts, pool)
	session, err := svc.Start(context.Background(), deviceauth.StartRequest{})
	if err != nil {
		t.Fatal(err)
	}
	pub := session.Public()
	if _, ok := pub["device_code"]; ok {
		t.Fatal("device_code leaked in public view")
	}
	if pub["user_code"] != "ABCD-EFGH" {
		t.Fatalf("user_code=%v", pub["user_code"])
	}
	// first poll pending
	session, err = svc.PollOnce(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if session.Status != domain.StatusPending {
		t.Fatalf("status=%s", session.Status)
	}
	// second poll success
	session, err = svc.PollOnce(context.Background(), session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if session.Status != domain.StatusSucceeded || session.AccountID == "" {
		t.Fatalf("session=%+v", session)
	}
	if session.DeviceCode != "" {
		t.Fatal("device code retained after success")
	}
	if len(accounts.saved) != 1 || accounts.saved[0].AccessToken != "access" {
		t.Fatalf("accounts=%+v", accounts.saved)
	}
	if len(pool.items) != 1 {
		t.Fatalf("pool upsert missing")
	}
}
