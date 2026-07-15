package deviceauth

import (
	"context"
	"sync"
	"testing"
	"time"

	domain "github.com/AokiAx/grok2api/backend/internal/domain/deviceauth"
	"github.com/AokiAx/grok2api/backend/internal/upstream"
)

// redStore is deliberately concurrency-safe so these tests expose service
// coordination bugs rather than races in the test double.
type redStore struct {
	mu    sync.Mutex
	items map[string]domain.Session
}

func (s *redStore) CreateDeviceAuthSession(_ context.Context, item domain.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == nil {
		s.items = make(map[string]domain.Session)
	}
	s.items[item.ID] = item
	return nil
}

func (s *redStore) GetDeviceAuthSession(_ context.Context, id string) (domain.Session, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	item, ok := s.items[id]
	return item, ok, nil
}

func (s *redStore) UpdateDeviceAuthSession(_ context.Context, item domain.Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[item.ID] = item
	return nil
}

func (s *redStore) ListDeviceAuthSessions(_ context.Context, _ int) ([]domain.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.Session, 0, len(s.items))
	for _, item := range s.items {
		out = append(out, item)
	}
	return out, nil
}

type redPoller struct {
	mu      sync.Mutex
	calls   int
	results []upstream.DeviceTokenResult

	firstEntered  chan struct{}
	secondEntered chan struct{}
	releaseFirst  chan struct{}
}

func (p *redPoller) PollDeviceToken(context.Context, string, string, string) (upstream.DeviceTokenResult, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	if call == 1 {
		close(p.firstEntered)
		<-p.releaseFirst
	} else if call == 2 {
		close(p.secondEntered)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.results) == 0 {
		return upstream.DeviceTokenResult{}, nil
	}
	if call > len(p.results) {
		return p.results[len(p.results)-1], nil
	}
	return p.results[call-1], nil
}

func (p *redPoller) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func TestTickHonorsIntervalAndSlowDown(t *testing.T) {
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	const id = "das-interval"
	store := &redStore{items: map[string]domain.Session{
		id: {
			ID:          id,
			Status:      domain.StatusPending,
			Issuer:      "https://issuer.example",
			ClientID:    "client",
			DeviceCode:  "device-code",
			IntervalSec: 5,
			ExpiresAt:   now.Add(time.Minute),
			UpdatedAt:   now.Add(-6 * time.Second),
		},
	}}
	poller := &redPoller{
		results:       []upstream.DeviceTokenResult{{Pending: true, SlowDown: true}, {AccessToken: "must-not-be-polled"}},
		firstEntered:  make(chan struct{}),
		secondEntered: make(chan struct{}),
		releaseFirst:  make(chan struct{}),
	}
	close(poller.releaseFirst)
	svc := NewService(store, nil, poller, nil, nil, nil, WithNow(func() time.Time { return now }))

	svc.tick(context.Background())
	store.mu.Lock()
	updated := store.items[id]
	store.mu.Unlock()
	if updated.Status != domain.StatusSlowDown || updated.IntervalSec != 10 {
		t.Fatalf("after slow_down: status=%s interval=%d", updated.Status, updated.IntervalSec)
	}

	// The second tick is immediate. It must wait for the server-provided
	// interval (including the RFC 8628 slow_down increment).
	svc.tick(context.Background())
	if got := poller.callCount(); got != 1 {
		t.Fatalf("poll count=%d, want 1 before interval elapses", got)
	}
}

func TestPollOnceConcurrentSessionExchangesOnlyOnce(t *testing.T) {
	now := time.Date(2026, time.July, 15, 12, 0, 0, 0, time.UTC)
	const id = "das-concurrent"
	store := &redStore{items: map[string]domain.Session{
		id: {
			ID:          id,
			Status:      domain.StatusPending,
			Issuer:      "https://issuer.example",
			ClientID:    "client",
			DeviceCode:  "device-code",
			IntervalSec: 5,
			ExpiresAt:   now.Add(time.Minute),
			UpdatedAt:   now,
		},
	}}
	poller := &redPoller{
		results:       []upstream.DeviceTokenResult{{AccessToken: "access", RefreshToken: "refresh"}, {AccessToken: "duplicate"}},
		firstEntered:  make(chan struct{}),
		secondEntered: make(chan struct{}),
		releaseFirst:  make(chan struct{}),
	}
	svc := NewService(store, nil, poller, nil, nil, nil, WithNow(func() time.Time { return now }))

	firstResult := make(chan domain.Session, 1)
	firstErr := make(chan error, 1)
	go func() {
		item, err := svc.PollOnce(context.Background(), id)
		firstResult <- item
		firstErr <- err
	}()
	select {
	case <-poller.firstEntered:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("first poll did not reach token endpoint")
	}

	secondResult := make(chan domain.Session, 1)
	secondErr := make(chan error, 1)
	go func() {
		item, err := svc.PollOnce(context.Background(), id)
		secondResult <- item
		secondErr <- err
	}()
	// While the first exchange is in flight, a second exchange must not be
	// started. The current implementation signals secondEntered here.
	select {
	case <-poller.secondEntered:
		t.Fatal("concurrent PollOnce started a second token exchange")
	case <-time.After(200 * time.Millisecond):
	}

	close(poller.releaseFirst)
	select {
	case <-firstResult:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("first poll did not finish")
	}
	if err := <-firstErr; err != nil {
		t.Fatal(err)
	}
	select {
	case item := <-secondResult:
		if item.Status != domain.StatusSucceeded {
			t.Fatalf("second poll status=%s, want terminal succeeded state", item.Status)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("second poll did not finish")
	}
	if err := <-secondErr; err != nil {
		t.Fatal(err)
	}
	if got := poller.callCount(); got != 1 {
		t.Fatalf("token exchange count=%d, want 1", got)
	}
}
