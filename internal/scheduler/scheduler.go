package scheduler

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/AokiAx/grok2api/internal/account"
)

var ErrNoReadyAccount = errors.New("no ready account")

const defaultStickyTTL = 30 * time.Minute

type stickyEntry struct {
	accountID string
	expiresAt time.Time
}

type Scheduler struct {
	mu        sync.Mutex
	accounts  map[string]*account.Account
	ready     []string
	notify    chan struct{}
	revision  uint64
	sticky    map[string]stickyEntry
	stickyTTL time.Duration
	stickyOn  bool
}

func New(accounts []account.Account) *Scheduler {
	scheduler := &Scheduler{
		accounts:  make(map[string]*account.Account, len(accounts)),
		notify:    make(chan struct{}, 1),
		revision:  1,
		sticky:    make(map[string]stickyEntry),
		stickyTTL: defaultStickyTTL,
		stickyOn:  true,
	}
	for index := range accounts {
		item := accounts[index]
		if item.MaxActive <= 0 {
			item.MaxActive = 1
		}
		scheduler.accounts[item.ID] = &item
		if item.Pool == account.PoolReady {
			scheduler.ready = append(scheduler.ready, item.ID)
		}
	}
	return scheduler
}

// WithSticky configures session stickiness for prompt-cache affinity.
// key empty or ttl<=0 disables sticky selection (Acquire still works).
func (s *Scheduler) WithSticky(enabled bool, ttl time.Duration) *Scheduler {
	if s == nil {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stickyOn = enabled
	if ttl > 0 {
		s.stickyTTL = ttl
	}
	return s
}

// ApplyMaxActive sets MaxActive on every in-memory account (cli_pool_max_concurrent).
// Values <= 0 are treated as 1. Does not rewrite SQLite; call after New/load.
func (s *Scheduler) ApplyMaxActive(n int) *Scheduler {
	if s == nil {
		return s
	}
	if n <= 0 {
		n = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range s.accounts {
		if item == nil {
			continue
		}
		item.MaxActive = n
	}
	return s
}

func (s *Scheduler) Acquire(ctx context.Context) (*Lease, error) {
	return s.AcquireSticky(ctx, "")
}

// AcquireSticky prefers the account last used for stickyKey (when enabled),
// so repeated client sessions stay on a warm Grok credential for prefix cache.
func (s *Scheduler) AcquireSticky(ctx context.Context, stickyKey string) (*Lease, error) {
	for {
		s.mu.Lock()
		if len(s.ready) == 0 {
			s.mu.Unlock()
			return nil, ErrNoReadyAccount
		}
		now := time.Now()
		// 1) Prefer sticky account when free and still ready.
		if s.stickyOn && stickyKey != "" {
			if entry, ok := s.sticky[stickyKey]; ok {
				if entry.expiresAt.Before(now) {
					delete(s.sticky, stickyKey)
				} else if item := s.accounts[entry.accountID]; item != nil && item.Available(now) {
					item.Active++
					item.RequestCount++
					s.bumpStickyLocked(stickyKey, entry.accountID, now)
					lease := &Lease{scheduler: s, accountID: entry.accountID}
					s.mu.Unlock()
					return lease, nil
				} else if item == nil || item.Pool != account.PoolReady {
					// Account gone or not ready — drop sticky binding.
					delete(s.sticky, stickyKey)
				}
				// Busy sticky account: fall through to another ready account.
			}
		}
		// 2) Prefer least-used ready account so traffic does not spray evenly
		// across the whole pool (which burns every credential on failures).
		id, item := s.pickLeastUsedReadyLocked(now)
		if item != nil {
			item.Active++
			item.RequestCount++
			if s.stickyOn && stickyKey != "" {
				s.bumpStickyLocked(stickyKey, id, now)
			}
			lease := &Lease{scheduler: s, accountID: id}
			s.mu.Unlock()
			return lease, nil
		}
		s.mu.Unlock()

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.notify:
		}
	}
}

// pickLeastUsedReadyLocked returns the free ready account with the lowest
// RequestCount (tie-break by id). Avoids pure round-robin spraying that makes
// every credential share fatal quota/auth errors equally.
func (s *Scheduler) pickLeastUsedReadyLocked(now time.Time) (string, *account.Account) {
	var bestID string
	var best *account.Account
	for _, id := range s.ready {
		item := s.accounts[id]
		if item == nil || !item.Available(now) {
			continue
		}
		if best == nil ||
			item.RequestCount < best.RequestCount ||
			(item.RequestCount == best.RequestCount && id < bestID) {
			best = item
			bestID = id
		}
	}
	return bestID, best
}

func (s *Scheduler) bumpStickyLocked(key, accountID string, now time.Time) {
	ttl := s.stickyTTL
	if ttl <= 0 {
		ttl = defaultStickyTTL
	}
	s.sticky[key] = stickyEntry{accountID: accountID, expiresAt: now.Add(ttl)}
	// Opportunistic prune to keep the map bounded under many keys.
	if len(s.sticky) > 10000 {
		for k, e := range s.sticky {
			if e.expiresAt.Before(now) {
				delete(s.sticky, k)
			}
		}
	}
}

func (s *Scheduler) ReadyCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.ready)
}

func (s *Scheduler) Status() (int, int, map[account.UnavailableReason]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reasons := make(map[account.UnavailableReason]int)
	ready := 0
	unavailable := 0
	for _, item := range s.accounts {
		if item.Pool == account.PoolReady {
			ready++
			continue
		}
		unavailable++
		reasons[item.UnavailableReason]++
	}
	return ready, unavailable, reasons
}

// ActiveByID returns in-memory lease counts keyed by account ID.
// Active is intentionally not persisted; admin views must merge this snapshot.
func (s *Scheduler) ActiveByID() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.accounts))
	for id, item := range s.accounts {
		if item == nil || item.Active <= 0 {
			continue
		}
		out[id] = item.Active
	}
	return out
}

func (s *Scheduler) EarliestRetry() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	var earliest time.Time
	for _, item := range s.accounts {
		if item.Pool != account.PoolUnavailable || item.RetryAt.IsZero() {
			continue
		}
		if earliest.IsZero() || item.RetryAt.Before(earliest) {
			earliest = item.RetryAt
		}
	}
	return earliest
}

func (s *Scheduler) PromoteDue(now time.Time) []account.Account {
	s.mu.Lock()
	var promoted []account.Account
	for _, item := range s.accounts {
		if item.Pool != account.PoolUnavailable {
			continue
		}
		// Quota accounts require a real probe before re-entry; only cooldown
		// is safe to promote purely by retry_at.
		if item.UnavailableReason != account.ReasonCooldown {
			continue
		}
		if item.RetryAt.IsZero() || item.RetryAt.After(now) {
			continue
		}
		item.Pool = account.PoolReady
		item.UnavailableReason = ""
		item.RetryAt = time.Time{}
		item.LastErrorCode = ""
		item.UpdatedAt = now.UTC()
		s.ready = append(s.ready, item.ID)
		promoted = append(promoted, *item)
	}
	if len(promoted) > 0 {
		s.revision++
	}
	s.mu.Unlock()
	if len(promoted) > 0 {
		s.signal()
	}
	return promoted
}

func (s *Scheduler) Upsert(item account.Account) {
	if item.MaxActive <= 0 {
		item.MaxActive = 1
	}
	s.mu.Lock()
	s.removeReadyLocked(item.ID)
	if existing := s.accounts[item.ID]; existing != nil {
		item.Active = existing.Active
	}
	copy := item
	s.accounts[item.ID] = &copy
	if item.Pool == account.PoolReady {
		s.ready = append(s.ready, item.ID)
	}
	s.revision++
	s.mu.Unlock()
	s.signal()
}

func (s *Scheduler) Delete(id string) bool {
	s.mu.Lock()
	if _, exists := s.accounts[id]; !exists {
		s.mu.Unlock()
		return false
	}
	s.removeReadyLocked(id)
	delete(s.accounts, id)
	s.revision++
	s.mu.Unlock()
	s.signal()
	return true
}

func (s *Scheduler) Revision() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision
}

func (s *Scheduler) signal() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *Scheduler) removeReadyLocked(id string) {
	filtered := s.ready[:0]
	for _, candidate := range s.ready {
		if candidate != id {
			filtered = append(filtered, candidate)
		}
	}
	s.ready = filtered
}

type Lease struct {
	scheduler *Scheduler
	accountID string
	once      sync.Once
}

func (l *Lease) Account() account.Account {
	l.scheduler.mu.Lock()
	defer l.scheduler.mu.Unlock()
	item := l.scheduler.accounts[l.accountID]
	if item == nil {
		return account.Account{}
	}
	return *item
}

func (l *Lease) MoveUnavailable(reason account.UnavailableReason, retryAt time.Time, errorCode string) {
	l.scheduler.mu.Lock()
	defer l.scheduler.mu.Unlock()
	item := l.scheduler.accounts[l.accountID]
	if item == nil {
		return
	}
	changed := item.Pool != account.PoolUnavailable ||
		item.UnavailableReason != reason ||
		!item.RetryAt.Equal(retryAt) ||
		item.LastErrorCode != errorCode
	item.Pool = account.PoolUnavailable
	item.UnavailableReason = reason
	item.RetryAt = retryAt
	item.LastErrorCode = errorCode
	item.UpdatedAt = time.Now().UTC()
	l.scheduler.removeReadyLocked(l.accountID)
	if changed {
		l.scheduler.revision++
	}
}

// RecordUsage updates free-tier quota counters observed from response headers.
func (l *Lease) RecordUsage(actual, limit int64, at time.Time) {
	l.scheduler.mu.Lock()
	defer l.scheduler.mu.Unlock()
	item := l.scheduler.accounts[l.accountID]
	if item == nil {
		return
	}
	if at.IsZero() {
		at = time.Now().UTC()
	} else {
		at = at.UTC()
	}
	item.QuotaActual = actual
	item.QuotaLimit = limit
	item.LastSuccessAt = at
	item.UpdatedAt = at
}

func (l *Lease) Release() {
	l.once.Do(func() {
		l.scheduler.mu.Lock()
		if item := l.scheduler.accounts[l.accountID]; item != nil && item.Active > 0 {
			item.Active--
		}
		l.scheduler.mu.Unlock()
		l.scheduler.signal()
	})
}
