package scheduler

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/AokiAx/grok2api/internal/account"
)

var ErrNoReadyAccount = errors.New("no ready account")

type Scheduler struct {
	mu       sync.Mutex
	accounts map[string]*account.Account
	ready    []string
	notify   chan struct{}
	revision uint64
}

func New(accounts []account.Account) *Scheduler {
	scheduler := &Scheduler{
		accounts: make(map[string]*account.Account, len(accounts)),
		notify:   make(chan struct{}, 1),
		revision: 1,
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

func (s *Scheduler) Acquire(ctx context.Context) (*Lease, error) {
	for {
		s.mu.Lock()
		if len(s.ready) == 0 {
			s.mu.Unlock()
			return nil, ErrNoReadyAccount
		}
		for range len(s.ready) {
			id := s.ready[0]
			s.ready = append(s.ready[1:], id)
			item := s.accounts[id]
			if item == nil || !item.Available(time.Now()) {
				continue
			}
			item.Active++
			item.RequestCount++
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
		if item.UnavailableReason != account.ReasonQuota && item.UnavailableReason != account.ReasonCooldown {
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
