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
}

func New(accounts []account.Account) *Scheduler {
	scheduler := &Scheduler{
		accounts: make(map[string]*account.Account, len(accounts)),
		notify:   make(chan struct{}, 1),
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
	item.Pool = account.PoolUnavailable
	item.UnavailableReason = reason
	item.RetryAt = retryAt
	item.LastErrorCode = errorCode
	item.UpdatedAt = time.Now().UTC()
	l.scheduler.removeReadyLocked(l.accountID)
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
