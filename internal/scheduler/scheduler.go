package scheduler

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/AokiAx/grok2api/internal/account"
)

var ErrNoReadyAccount = errors.New("no ready account")

const defaultStickyTTL = 30 * time.Minute

// Strategy selects among free ready accounts (CLIProxyAPI-style).
type Strategy string

const (
	// StrategyRoundRobin cycles through free ready accounts (default).
	StrategyRoundRobin Strategy = "round-robin"
	// StrategyFillFirst always prefers the first free ready account (by id),
	// burning one credential before opening the next.
	StrategyFillFirst Strategy = "fill-first"
)

func ParseStrategy(raw string) Strategy {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(StrategyFillFirst), "fill_first", "fillfirst":
		return StrategyFillFirst
	default:
		// Default round-robin *within the hot set* so concurrent load spreads
		// across the working set instead of stacking on one account.
		return StrategyRoundRobin
	}
}

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
	strategy  Strategy
	rrCursor  uint64
	// activeSize optionally caps how many distinct ready ids may serve
	// (0 = all ready, CLIProxyAPI default behavior).
	activeSize int
	hot        map[string]struct{}
}

func New(accounts []account.Account) *Scheduler {
	scheduler := &Scheduler{
		accounts:  make(map[string]*account.Account, len(accounts)),
		notify:    make(chan struct{}, 1),
		revision:  1,
		sticky:    make(map[string]stickyEntry),
		stickyTTL: defaultStickyTTL,
		stickyOn:  true,
		strategy:  StrategyRoundRobin,
		// Hot set: only this many ready accounts serve. Rest are cold reserve.
		// Concurrent capacity ≈ activeSize * MaxActive (see ApplyMaxActive).
		activeSize: 32,
		hot:        make(map[string]struct{}),
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

// WithStrategy sets round-robin or fill-first selection.
func (s *Scheduler) WithStrategy(strategy Strategy) *Scheduler {
	if s == nil {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.strategy = ParseStrategy(string(strategy))
	return s
}

// WithSticky configures session stickiness for prompt-cache affinity.
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

// ApplyActiveSize caps how many ready accounts may serve traffic (hot set).
// 0 disables the cap (not recommended for free-tier pools).
func (s *Scheduler) ApplyActiveSize(n int) *Scheduler {
	if s == nil {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if n < 0 {
		n = 0
	}
	s.activeSize = n
	if n == 0 {
		s.hot = make(map[string]struct{})
		return s
	}
	if len(s.hot) > n {
		kept := 0
		for id := range s.hot {
			if kept >= n {
				delete(s.hot, id)
				continue
			}
			kept++
		}
	}
	return s
}

func (s *Scheduler) Acquire(ctx context.Context) (*Lease, error) {
	return s.AcquireSticky(ctx, "")
}

// AcquireSticky prefers the account last used for stickyKey (when enabled).
func (s *Scheduler) AcquireSticky(ctx context.Context, stickyKey string) (*Lease, error) {
	for {
		s.mu.Lock()
		if len(s.ready) == 0 {
			s.mu.Unlock()
			return nil, ErrNoReadyAccount
		}
		now := time.Now()
		if s.stickyOn && stickyKey != "" {
			if entry, ok := s.sticky[stickyKey]; ok {
				if entry.expiresAt.Before(now) {
					delete(s.sticky, stickyKey)
				} else if item := s.accounts[entry.accountID]; item != nil && item.Available(now) && s.eligibleLocked(entry.accountID) {
					item.Active++
					item.RequestCount++
					s.ensureHotLocked(entry.accountID)
					s.bumpStickyLocked(stickyKey, entry.accountID, now)
					lease := &Lease{scheduler: s, accountID: entry.accountID}
					s.mu.Unlock()
					return lease, nil
				} else if item == nil || item.Pool != account.PoolReady {
					delete(s.sticky, stickyKey)
				}
			}
		}

		id, item := s.pickReadyLocked(now)
		if item != nil {
			item.Active++
			item.RequestCount++
			s.ensureHotLocked(id)
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

// pickReadyLocked selects among free *eligible* ready accounts.
// With activeSize>0, eligibility is the hot set only (cold ready never serve
// until a hot slot frees). Strategy only orders within that set.
func (s *Scheduler) pickReadyLocked(now time.Time) (string, *account.Account) {
	candidates := make([]string, 0, len(s.ready))
	for _, id := range s.ready {
		item := s.accounts[id]
		if item == nil || !item.Available(now) {
			continue
		}
		if !s.eligibleLocked(id) {
			continue
		}
		candidates = append(candidates, id)
	}
	if len(candidates) == 0 {
		return "", nil
	}

	strategy := s.strategy
	if strategy == "" {
		strategy = StrategyRoundRobin
	}

	var chosen string
	switch strategy {
	case StrategyFillFirst:
		// Deterministic: smallest id among free eligible accounts.
		chosen = candidates[0]
		for _, id := range candidates[1:] {
			if id < chosen {
				chosen = id
			}
		}
	default:
		// Round-robin over free eligible accounts.
		idx := int(s.rrCursor % uint64(len(candidates)))
		s.rrCursor++
		chosen = candidates[idx]
	}
	return chosen, s.accounts[chosen]
}

func (s *Scheduler) eligibleLocked(id string) bool {
	if s.activeSize <= 0 {
		return true
	}
	if _, inHot := s.hot[id]; inHot {
		return true
	}
	return len(s.hot) < s.activeSize
}

func (s *Scheduler) ensureHotLocked(id string) {
	if s.activeSize <= 0 {
		return
	}
	if s.hot == nil {
		s.hot = make(map[string]struct{})
	}
	if _, ok := s.hot[id]; ok {
		return
	}
	if len(s.hot) >= s.activeSize {
		for hid := range s.hot {
			item := s.accounts[hid]
			if item == nil || item.Pool != account.PoolReady || item.Active == 0 {
				delete(s.hot, hid)
				if len(s.hot) < s.activeSize {
					break
				}
			}
		}
	}
	if len(s.hot) < s.activeSize {
		s.hot[id] = struct{}{}
	}
}

func (s *Scheduler) dropHotLocked(id string) {
	delete(s.hot, id)
}

func (s *Scheduler) bumpStickyLocked(key, accountID string, now time.Time) {
	ttl := s.stickyTTL
	if ttl <= 0 {
		ttl = defaultStickyTTL
	}
	s.sticky[key] = stickyEntry{accountID: accountID, expiresAt: now.Add(ttl)}
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
	s.dropHotLocked(id)
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
