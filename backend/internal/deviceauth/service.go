package deviceauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/account"
	domain "github.com/AokiAx/grok2api/backend/internal/domain/deviceauth"
	"github.com/AokiAx/grok2api/backend/internal/repository"
	"github.com/AokiAx/grok2api/backend/internal/upstream"
)

type Starter interface {
	StartDeviceAuthorization(context.Context, string, string, string) (upstream.DeviceAuthorization, error)
}

type Poller interface {
	PollDeviceToken(context.Context, string, string, string) (upstream.DeviceTokenResult, error)
}

type Validator interface {
	Validate(context.Context, account.Account) (account.UnavailableReason, string, error)
}

type AccountWriter interface {
	SaveAccount(context.Context, account.Account) error
}

type PoolSink interface {
	Upsert(account.Account)
}

type Service struct {
	store     repository.DeviceAuthRepository
	starter   Starter
	poller    Poller
	validator Validator
	accounts  AccountWriter
	sink      PoolSink
	now       func() time.Time
	issuer    string
	clientID  string
	scope     string
}

type Option func(*Service)

func WithNow(now func() time.Time) Option {
	return func(s *Service) {
		if now != nil {
			s.now = now
		}
	}
}

func WithOIDC(issuer, clientID, scope string) Option {
	return func(s *Service) {
		if issuer != "" {
			s.issuer = issuer
		}
		if clientID != "" {
			s.clientID = clientID
		}
		if scope != "" {
			s.scope = scope
		}
	}
}

func NewService(
	store repository.DeviceAuthRepository,
	starter Starter,
	poller Poller,
	validator Validator,
	accounts AccountWriter,
	sink PoolSink,
	options ...Option,
) *Service {
	s := &Service{
		store:     store,
		starter:   starter,
		poller:    poller,
		validator: validator,
		accounts:  accounts,
		sink:      sink,
		now:       time.Now,
		issuer:    "https://auth.x.ai",
		clientID:  "grok-cli",
		scope:     "openid profile email offline_access",
	}
	for _, option := range options {
		option(s)
	}
	return s
}

type StartRequest struct {
	Issuer   string `json:"issuer"`
	ClientID string `json:"client_id"`
	Scope    string `json:"scope"`
}

func (s *Service) Start(ctx context.Context, req StartRequest) (domain.Session, error) {
	issuer := first(req.Issuer, s.issuer)
	clientID := first(req.ClientID, s.clientID)
	scope := first(req.Scope, s.scope)
	auth, err := s.starter.StartDeviceAuthorization(ctx, issuer, clientID, scope)
	if err != nil {
		return domain.Session{}, err
	}
	now := s.now().UTC()
	session := domain.Session{
		ID:                      newID("das"),
		Status:                  domain.StatusPending,
		Issuer:                  issuer,
		ClientID:                clientID,
		Scope:                   scope,
		UserCode:                auth.UserCode,
		VerificationURI:         auth.VerificationURI,
		VerificationURIComplete: auth.VerificationURIComplete,
		DeviceCode:              auth.DeviceCode,
		IntervalSec:             int(auth.Interval / time.Second),
		ExpiresAt:               now.Add(auth.ExpiresIn),
		CreatedAt:               now,
		UpdatedAt:               now,
	}
	if err := session.Normalize(); err != nil {
		return domain.Session{}, err
	}
	if err := s.store.CreateDeviceAuthSession(ctx, session); err != nil {
		return domain.Session{}, err
	}
	return session, nil
}

func (s *Service) Get(ctx context.Context, id string) (domain.Session, bool, error) {
	return s.store.GetDeviceAuthSession(ctx, strings.TrimSpace(id))
}

func (s *Service) Cancel(ctx context.Context, id string) (domain.Session, error) {
	session, found, err := s.store.GetDeviceAuthSession(ctx, strings.TrimSpace(id))
	if err != nil {
		return domain.Session{}, err
	}
	if !found {
		return domain.Session{}, fmt.Errorf("device auth session not found")
	}
	if session.Status == domain.StatusSucceeded {
		return session, nil
	}
	now := s.now().UTC()
	session.Status = domain.StatusCancelled
	session.CompletedAt = now
	session.UpdatedAt = now
	session.DeviceCode = ""
	if err := s.store.UpdateDeviceAuthSession(ctx, session); err != nil {
		return domain.Session{}, err
	}
	return session, nil
}

// PollOnce advances one device-token poll for a pending session.
func (s *Service) PollOnce(ctx context.Context, id string) (domain.Session, error) {
	session, found, err := s.store.GetDeviceAuthSession(ctx, strings.TrimSpace(id))
	if err != nil {
		return domain.Session{}, err
	}
	if !found {
		return domain.Session{}, fmt.Errorf("device auth session not found")
	}
	now := s.now().UTC()
	if session.Status != domain.StatusPending && session.Status != domain.StatusSlowDown {
		return session, nil
	}
	if !session.ExpiresAt.IsZero() && !now.Before(session.ExpiresAt) {
		session.Status = domain.StatusExpired
		session.CompletedAt = now
		session.UpdatedAt = now
		session.DeviceCode = ""
		_ = s.store.UpdateDeviceAuthSession(ctx, session)
		return session, nil
	}
	if strings.TrimSpace(session.DeviceCode) == "" {
		session.Status = domain.StatusFailed
		session.LastError = "missing device code"
		session.CompletedAt = now
		session.UpdatedAt = now
		_ = s.store.UpdateDeviceAuthSession(ctx, session)
		return session, nil
	}
	result, pollErr := s.poller.PollDeviceToken(ctx, session.Issuer, session.ClientID, session.DeviceCode)
	if pollErr != nil && !result.Pending && !result.Denied && !result.Expired {
		session.Status = domain.StatusFailed
		session.LastError = pollErr.Error()
		session.CompletedAt = now
		session.UpdatedAt = now
		session.DeviceCode = ""
		_ = s.store.UpdateDeviceAuthSession(ctx, session)
		return session, nil
	}
	switch {
	case result.Pending:
		if result.SlowDown {
			session.Status = domain.StatusSlowDown
			session.IntervalSec += 5
		} else {
			session.Status = domain.StatusPending
		}
		session.UpdatedAt = now
		_ = s.store.UpdateDeviceAuthSession(ctx, session)
		return session, nil
	case result.Denied:
		session.Status = domain.StatusDenied
		session.LastError = first(result.ErrorDescription, result.Error, "access_denied")
		session.CompletedAt = now
		session.UpdatedAt = now
		session.DeviceCode = ""
		_ = s.store.UpdateDeviceAuthSession(ctx, session)
		return session, nil
	case result.Expired:
		session.Status = domain.StatusExpired
		session.LastError = first(result.ErrorDescription, result.Error, "expired_token")
		session.CompletedAt = now
		session.UpdatedAt = now
		session.DeviceCode = ""
		_ = s.store.UpdateDeviceAuthSession(ctx, session)
		return session, nil
	}

	item := account.Account{
		ID:           "device-" + session.ID,
		AccessToken:  result.AccessToken,
		RefreshToken: result.RefreshToken,
		OIDCIssuer:   session.Issuer,
		OIDCClientID: session.ClientID,
		MaxActive:    1,
		CreatedAt:    now,
		UpdatedAt:    now,
	}
	if result.ExpiresIn > 0 {
		item.ExpiresAt = now.Add(result.ExpiresIn)
	}
	if s.validator != nil {
		reason, code, valErr := s.validator.Validate(ctx, item)
		if valErr != nil {
			session.Status = domain.StatusFailed
			session.LastError = valErr.Error()
			session.CompletedAt = now
			session.UpdatedAt = now
			session.DeviceCode = ""
			_ = s.store.UpdateDeviceAuthSession(ctx, session)
			return session, nil
		}
		if reason == "" {
			item.MarkReady(now)
		} else {
			item.MarkUnavailable(reason, now.Add(45*time.Second), code, now)
		}
	} else {
		item.MarkReady(now)
	}
	if s.accounts != nil {
		if err := s.accounts.SaveAccount(ctx, item); err != nil {
			session.Status = domain.StatusFailed
			session.LastError = err.Error()
			session.CompletedAt = now
			session.UpdatedAt = now
			session.DeviceCode = ""
			_ = s.store.UpdateDeviceAuthSession(ctx, session)
			return session, nil
		}
	}
	if s.sink != nil {
		s.sink.Upsert(item)
	}
	session.Status = domain.StatusSucceeded
	session.AccountID = item.ID
	session.CompletedAt = now
	session.UpdatedAt = now
	session.DeviceCode = ""
	if err := s.store.UpdateDeviceAuthSession(ctx, session); err != nil {
		return domain.Session{}, err
	}
	slog.Info("device auth succeeded", "session_id", session.ID, "account_id", item.ID)
	return session, nil
}

// RunWorker polls pending sessions until context cancel.
func (s *Service) RunWorker(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

func (s *Service) tick(ctx context.Context) {
	sessions, err := s.store.ListDeviceAuthSessions(ctx, 50)
	if err != nil {
		slog.Warn("list device auth sessions failed", "error", err)
		return
	}
	for _, session := range sessions {
		if session.Status != domain.StatusPending && session.Status != domain.StatusSlowDown {
			continue
		}
		if _, err := s.PollOnce(ctx, session.ID); err != nil {
			slog.Warn("device auth poll failed", "session_id", session.ID, "error", err)
		}
	}
}

func first(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func newID(prefix string) string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(raw[:])
}
