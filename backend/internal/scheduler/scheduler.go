package scheduler

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/account"
)

var ErrNoReadyAccount = errors.New("no ready account")

// SelectionReason explains why Acquire could not hand out a lease.
// SelectionError describes why the CLI account pool could not provide a lease.
type SelectionReason string

const (
	SelectionNoReady    SelectionReason = "no_ready"
	SelectionSaturated  SelectionReason = "saturated"
	SelectionQuota      SelectionReason = "quota"
	SelectionCooling    SelectionReason = "cooling"
	SelectionAuth       SelectionReason = "auth"
	SelectionValidating SelectionReason = "validating"
)

// SelectionError is returned when no account can be leased right now.
// errors.Is(err, ErrNoReadyAccount) remains true for callers that only check emptiness.
type SelectionError struct {
	Reason     SelectionReason
	RetryAfter time.Duration
	// Ready is the ready-pool size at failure time (may be >0 when saturated).
	Ready int
	// Unavailable is the unavailable-pool size at failure time.
	Unavailable int
}

func (e *SelectionError) Error() string {
	if e == nil {
		return "no ready account"
	}
	switch e.Reason {
	case SelectionSaturated:
		return "ready accounts are at concurrency capacity"
	case SelectionQuota:
		return "ready account pool exhausted by quota"
	case SelectionCooling:
		return "ready account pool cooling down"
	case SelectionAuth:
		return "ready account pool blocked by auth failures"
	case SelectionValidating:
		return "ready account pool validating"
	default:
		return "no ready account"
	}
}

func (e *SelectionError) Is(target error) bool {
	return target == ErrNoReadyAccount
}

// AsSelectionError unwraps a SelectionError.
func AsSelectionError(err error) (*SelectionError, bool) {
	var target *SelectionError
	if errors.As(err, &target) {
		return target, true
	}
	return nil, false
}

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

const (
	// maxFailStreakCap bounds exponential cooldown growth (base * 2^(streak-1)).
	maxFailStreakCap = 6
)

type Scheduler struct {
	mu        sync.Mutex
	accounts  map[string]*account.Account
	ready     []string
	notify    chan struct{}
	revision  uint64
	sticky    map[string]stickyEntry
	stickyTTL time.Duration
	stickyOn  bool
	quotaRetry time.Duration
	strategy  Strategy
	rrCursor  uint64
	// failStreak tracks consecutive account-scoped failures for exponential cooldown.
	// Process-local only; resets on success or process restart.
	failStreak map[string]int
	// maxActiveCap is the process-wide concurrency ceiling. A positive value
	// limits per-account settings without replacing a lower persisted limit.
	maxActiveCap int
	// activeSize optionally caps how many distinct ready ids may serve
	// (0 = all ready, CLIProxyAPI default behavior).
	activeSize int
	hot        map[string]struct{}
}

func New(accounts []account.Account) *Scheduler {
	scheduler := &Scheduler{
		accounts:   make(map[string]*account.Account, len(accounts)),
		notify:     make(chan struct{}, 1),
		revision:   1,
		sticky:     make(map[string]stickyEntry),
		stickyTTL:  defaultStickyTTL,
		stickyOn:   true,
		quotaRetry: 24 * time.Hour,
		strategy:   StrategyRoundRobin,
		failStreak: make(map[string]int),
		// Hot set: only this many ready accounts serve. Rest are cold reserve.
		// Concurrent capacity ≈ activeSize * MaxActive (see ApplyMaxActive).
		activeSize: 0,
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

// ApplyMaxActive sets the process-wide per-account concurrency ceiling
// (cli_pool_max_concurrent). Persisted account-specific limits below the
// ceiling are preserved; limits above it are clamped. The ceiling also applies
// to accounts upserted after startup.
func (s *Scheduler) ApplyMaxActive(n int) *Scheduler {
	if s == nil {
		return s
	}
	if n <= 0 {
		n = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxActiveCap = n
	for _, item := range s.accounts {
		if item == nil {
			continue
		}
		if item.MaxActive <= 0 || item.MaxActive > n {
			item.MaxActive = n
		}
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
			err := s.selectionFailureLocked(time.Now())
			s.mu.Unlock()
			return nil, err
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
		// No free eligible account right now (concurrency full and/or hot set full).
		// Snapshot a saturated failure in case the waiter times out.
		saturated := s.selectionSaturatedLocked(now)
		s.mu.Unlock()

		select {
		case <-ctx.Done():
			if saturated != nil {
				// Prefer structured capacity signal over bare context deadline.
				return nil, saturated
			}
			return nil, ctx.Err()
		case <-s.notify:
		}
	}
}

// pickReadyLocked selects from the highest-priority tier among free *eligible*
// ready accounts.
// With activeSize>0, eligibility is the hot set only (cold ready never serve
// until a hot slot frees). Within a priority tier, lower in-flight (Active)
// is preferred; strategy only orders among that lowest-Active subset.
func (s *Scheduler) pickReadyLocked(now time.Time) (string, *account.Account) {
	candidates := make([]string, 0, len(s.ready))
	highestPriority := 0
	hasCandidate := false
	// Copy ids first: parking exhausted accounts mutates s.ready.
	readyIDs := append([]string(nil), s.ready...)
	for _, id := range readyIDs {
		item := s.accounts[id]
		if item == nil {
			continue
		}
		// Known-empty free quotas must leave ready immediately so Status() and
		// acquire waiters do not treat them as capacity.
		retry := s.quotaRetry
		if retry <= 0 {
			retry = 24 * time.Hour
		}
		if item.ParkKnownExhausted(now, retry) {
			s.removeReadyLocked(id)
			s.revision++
			continue
		}
		if !item.Available(now) {
			continue
		}
		if !s.eligibleLocked(id) {
			continue
		}
		if !hasCandidate || item.Priority > highestPriority {
			highestPriority = item.Priority
			candidates = candidates[:0]
			hasCandidate = true
		}
		if item.Priority == highestPriority {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 {
		return "", nil
	}

	// Prefer lower in-flight within the priority tier (balanced load).
	lowestActive := -1
	loadBalanced := make([]string, 0, len(candidates))
	for _, id := range candidates {
		item := s.accounts[id]
		if item == nil {
			continue
		}
		if lowestActive < 0 || item.Active < lowestActive {
			lowestActive = item.Active
			loadBalanced = loadBalanced[:0]
			loadBalanced = append(loadBalanced, id)
			continue
		}
		if item.Active == lowestActive {
			loadBalanced = append(loadBalanced, id)
		}
	}
	if len(loadBalanced) == 0 {
		return "", nil
	}
	candidates = loadBalanced

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
		// Round-robin over free eligible accounts at the lowest Active.
		idx := int(s.rrCursor % uint64(len(candidates)))
		s.rrCursor++
		chosen = candidates[idx]
	}
	return chosen, s.accounts[chosen]
}

// ClearStickyByAccount drops session affinity entries bound to accountID.
// Used when an account is parked so the next request does not stick to a dead credential.
func (s *Scheduler) ClearStickyByAccount(accountID string) {
	if s == nil || strings.TrimSpace(accountID) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, entry := range s.sticky {
		if entry.accountID == accountID {
			delete(s.sticky, key)
		}
	}
}

// NoteFailure increments the process-local failure streak for accountID and
// returns the streak after increment (1 on first failure).
func (s *Scheduler) NoteFailure(accountID string) int {
	if s == nil || strings.TrimSpace(accountID) == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failStreak == nil {
		s.failStreak = make(map[string]int)
	}
	s.failStreak[accountID]++
	return s.failStreak[accountID]
}

// NoteSuccess clears the process-local failure streak for accountID.
func (s *Scheduler) NoteSuccess(accountID string) {
	if s == nil || strings.TrimSpace(accountID) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.failStreak, accountID)
}

// FailStreak returns the current process-local failure streak (0 if none).
func (s *Scheduler) FailStreak(accountID string) int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failStreak[accountID]
}

// CooldownForStreak returns base * 2^(streak-1), capped, for streak >= 1.
// streak <= 0 yields base. Used for rate-limit / validating style cooldowns.
func CooldownForStreak(base time.Duration, streak int) time.Duration {
	if base <= 0 {
		base = time.Second
	}
	if streak <= 1 {
		return base
	}
	if streak > maxFailStreakCap {
		streak = maxFailStreakCap
	}
	// streak=2 → base*2, streak=3 → base*4, …
	cooldown := base
	for i := 1; i < streak; i++ {
		if cooldown > time.Hour {
			return cooldown
		}
		cooldown *= 2
	}
	return cooldown
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

func (s *Scheduler) selectionFailureLocked(now time.Time) *SelectionError {
	ready, unavailable, reasons := s.statusLocked()
	reason := SelectionNoReady
	// Dominant unavailable reason when the ready pool is empty.
	if unavailable > 0 {
		switch {
		case reasons[account.ReasonQuota] > 0 && reasons[account.ReasonQuota] >= reasons[account.ReasonCooldown] &&
			reasons[account.ReasonQuota] >= reasons[account.ReasonAuth] &&
			reasons[account.ReasonQuota] >= reasons[account.ReasonValidating]:
			reason = SelectionQuota
		case reasons[account.ReasonCooldown] > 0 && reasons[account.ReasonCooldown] >= reasons[account.ReasonAuth] &&
			reasons[account.ReasonCooldown] >= reasons[account.ReasonValidating]:
			reason = SelectionCooling
		case reasons[account.ReasonAuth] > 0 && reasons[account.ReasonAuth] >= reasons[account.ReasonValidating]:
			reason = SelectionAuth
		case reasons[account.ReasonValidating] > 0:
			reason = SelectionValidating
		}
	}
	return &SelectionError{
		Reason:      reason,
		RetryAfter:  s.retryAfterLocked(now),
		Ready:       ready,
		Unavailable: unavailable,
	}
}

func (s *Scheduler) selectionSaturatedLocked(now time.Time) *SelectionError {
	ready, unavailable, _ := s.statusLocked()
	if ready == 0 {
		return s.selectionFailureLocked(now)
	}
	// Ready accounts exist but none are free/eligible.
	return &SelectionError{
		Reason:      SelectionSaturated,
		RetryAfter:  time.Second,
		Ready:       ready,
		Unavailable: unavailable,
	}
}

func (s *Scheduler) retryAfterLocked(now time.Time) time.Duration {
	var earliest time.Time
	for _, item := range s.accounts {
		if item == nil || item.Pool != account.PoolUnavailable || item.RetryAt.IsZero() {
			continue
		}
		if earliest.IsZero() || item.RetryAt.Before(earliest) {
			earliest = item.RetryAt
		}
	}
	if earliest.IsZero() {
		return time.Second
	}
	d := earliest.Sub(now)
	if d < time.Second {
		return time.Second
	}
	return d
}

func (s *Scheduler) statusLocked() (int, int, map[account.UnavailableReason]int) {
	reasons := make(map[account.UnavailableReason]int)
	ready := 0
	unavailable := 0
	for _, item := range s.accounts {
		if item == nil {
			continue
		}
		if item.Pool == account.PoolReady {
			ready++
			continue
		}
		unavailable++
		reasons[item.UnavailableReason]++
	}
	return ready, unavailable, reasons
}

func (s *Scheduler) ReadyCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.ready)
}

func (s *Scheduler) Status() (int, int, map[account.UnavailableReason]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statusLocked()
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
		if !item.RecoverCooldown(now) {
			continue
		}
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
	if s.maxActiveCap > 0 && item.MaxActive > s.maxActiveCap {
		item.MaxActive = s.maxActiveCap
	}
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
	item.MarkUnavailable(reason, retryAt, errorCode, time.Now())
	l.scheduler.removeReadyLocked(l.accountID)
	// Drop sticky bindings so subsequent requests do not prefer a parked account.
	for key, entry := range l.scheduler.sticky {
		if entry.accountID == l.accountID {
			delete(l.scheduler.sticky, key)
		}
	}
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
	item.RecordUsage(actual, limit, at)
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

// ConfigureQuotaRetry updates the backoff used when parking known-exhausted ready accounts.
func (s *Scheduler) ConfigureQuotaRetry(duration time.Duration) {
	if s == nil || duration <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quotaRetry = duration
}
