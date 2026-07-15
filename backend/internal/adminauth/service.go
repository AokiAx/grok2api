package adminauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"net"
	"strings"
	"time"

	domain "github.com/AokiAx/grok2api/backend/internal/domain/adminauth"
	"github.com/AokiAx/grok2api/backend/internal/repository"
	"github.com/AokiAx/grok2api/backend/internal/security"
)

var (
	ErrSetupRequired      = errors.New("admin setup required")
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrRateLimited        = errors.New("login rate limited")
	ErrUnauthorized       = errors.New("unauthorized")
	ErrConflict           = errors.New("session conflict")
	ErrInvalidRefresh     = errors.New("invalid refresh session")
)

type Clock func() time.Time
type Random func([]byte) error
type Option func(*Service)

func WithClock(c Clock) Option {
	return func(s *Service) {
		if c != nil {
			s.clock = c
		}
	}
}
func WithRandom(r Random) Option {
	return func(s *Service) {
		if r != nil {
			s.random = r
		}
	}
}
func WithBcryptCost(cost int) Option { return func(s *Service) { s.bcryptCost = cost } }

type Service struct {
	repo       repository.AdminAuthRepository
	clock      Clock
	random     Random
	bcryptCost int
}

func NewService(repo repository.AdminAuthRepository, opts ...Option) *Service {
	s := &Service{repo: repo, clock: time.Now, random: func(b []byte) error { _, err := rand.Read(b); return err }, bcryptCost: 10}
	for _, o := range opts {
		o(s)
	}
	return s
}

type LoginInput struct {
	Username, Password, SourceIP, UserAgent string
	Remember                                bool
}
type LoginOutput struct {
	Admin                             domain.AdminUser
	AccessToken, RefreshCookieValue   string
	AccessExpiresAt, RefreshExpiresAt time.Time
	Remember                          bool
}

func (s *Service) Login(ctx context.Context, in LoginInput) (LoginOutput, error) {
	count, err := s.repo.CountAdminUsers(ctx)
	if err != nil {
		return LoginOutput{}, err
	}
	if count == 0 {
		return LoginOutput{}, ErrSetupRequired
	}
	now := s.clock().UTC()
	username := strings.ToLower(strings.TrimSpace(in.Username))
	ip := strings.TrimSpace(in.SourceIP)
	if net.ParseIP(ip) == nil {
		return LoginOutput{}, ErrInvalidCredentials
	}
	failures, err := s.repo.CountRecentAdminLoginFailures(ctx, username, ip, now.Add(-15*time.Minute))
	if err != nil {
		return LoginOutput{}, err
	}
	if failures >= 5 {
		return LoginOutput{}, ErrRateLimited
	}
	u, ok, err := s.repo.GetAdminUserByUsername(ctx, username)
	if err != nil {
		return LoginOutput{}, err
	}
	if !ok || !u.CanAuthenticate() || !security.VerifyAdminPassword(u.Password, in.Password) {
		if err := s.recordAttempt(ctx, username, ip, false, "invalid_credentials", now); err != nil {
			return LoginOutput{}, err
		}
		return LoginOutput{}, ErrInvalidCredentials
	}
	access, err := s.randomToken(32)
	if err != nil {
		return LoginOutput{}, err
	}
	refresh, err := s.randomToken(32)
	if err != nil {
		return LoginOutput{}, err
	}
	sid, err := s.randomToken(16)
	if err != nil {
		return LoginOutput{}, err
	}
	family, err := s.randomToken(16)
	if err != nil {
		return LoginOutput{}, err
	}
	session, err := domain.NewSession(sid, family, u.ID, sha256.Sum256([]byte(access)), sha256.Sum256([]byte(refresh)), now.Add(5*time.Minute), now.Add(30*24*time.Hour), now)
	if err != nil {
		return LoginOutput{}, err
	}
	session.SourceIP = ip
	session.UserAgent = in.UserAgent
	session.Remember = in.Remember
	if err := s.repo.CreateAdminSession(ctx, session); err != nil {
		return LoginOutput{}, err
	}
	if err := s.recordAttempt(ctx, username, ip, true, "", now); err != nil {
		revokeErr := s.repo.RevokeAdminSession(ctx, session.ID, now, domain.RevocationLogout)
		if revokeErr != nil {
			return LoginOutput{}, errors.Join(err, revokeErr)
		}
		return LoginOutput{}, err
	}
	return LoginOutput{Admin: u, AccessToken: access, RefreshCookieValue: sid + "." + refresh, AccessExpiresAt: session.AccessExpiresAt, RefreshExpiresAt: session.ExpiresAt, Remember: session.Remember}, nil
}

func (s *Service) recordAttempt(ctx context.Context, u, ip string, ok bool, code string, at time.Time) error {
	a, e := domain.NewLoginAttempt(u, ip, ok, code, at)
	if e != nil {
		return e
	}
	return s.repo.RecordAdminLoginAttempt(ctx, a)
}
func (s *Service) randomToken(n int) (string, error) {
	b := make([]byte, n)
	if err := s.random(b); err != nil {
		return "", err
	}
	const hex = "0123456789abcdef"
	out := make([]byte, n*2)
	for i, v := range b {
		out[i*2] = hex[v>>4]
		out[i*2+1] = hex[v&15]
	}
	return string(out), nil
}

func (s *Service) AuthenticateAccess(ctx context.Context, token string) (domain.AdminUser, domain.Session, error) {
	if strings.TrimSpace(token) == "" {
		return domain.AdminUser{}, domain.Session{}, ErrUnauthorized
	}
	sess, ok, err := s.repo.FindAdminSessionByAccessHash(ctx, sha256.Sum256([]byte(token)))
	if err != nil {
		return domain.AdminUser{}, domain.Session{}, err
	}
	if !ok || !sess.AccessActive(s.clock()) {
		return domain.AdminUser{}, domain.Session{}, ErrUnauthorized
	}
	u, ok, err := s.repo.GetAdminUserByID(ctx, sess.AdminUserID)
	if err != nil {
		return domain.AdminUser{}, domain.Session{}, err
	}
	if !ok || !u.CanAuthenticate() {
		return domain.AdminUser{}, domain.Session{}, ErrUnauthorized
	}
	return u, sess, nil
}

func (s *Service) Refresh(ctx context.Context, cookie string, sourceIP, userAgent string) (LoginOutput, error) {
	parts := strings.SplitN(cookie, ".", 2)
	if len(parts) != 2 {
		return LoginOutput{}, ErrInvalidRefresh
	}
	now := s.clock().UTC()
	old, ok, err := s.repo.GetAdminSession(ctx, parts[0])
	if err != nil {
		return LoginOutput{}, err
	}
	if !ok || !old.MatchesRefreshSecretHash(sha256.Sum256([]byte(parts[1]))) {
		return LoginOutput{}, ErrInvalidRefresh
	}
	if !old.RotatedAt.IsZero() || old.RevocationReason == domain.RevocationRotated {
		if err := s.repo.RevokeAdminSessionFamily(ctx, old.FamilyID, now, domain.RevocationRefreshReplay); err != nil {
			return LoginOutput{}, err
		}
		return LoginOutput{}, ErrInvalidRefresh
	}
	if !old.Active(now) {
		return LoginOutput{}, ErrInvalidRefresh
	}
	access, e := s.randomToken(32)
	if e != nil {
		return LoginOutput{}, e
	}
	refresh, e := s.randomToken(32)
	if e != nil {
		return LoginOutput{}, e
	}
	sid, e := s.randomToken(16)
	if e != nil {
		return LoginOutput{}, e
	}
	replacement, e := domain.NewSession(sid, old.FamilyID, old.AdminUserID, sha256.Sum256([]byte(access)), sha256.Sum256([]byte(refresh)), now.Add(5*time.Minute), old.ExpiresAt, now)
	if e != nil {
		return LoginOutput{}, e
	}
	replacement.SourceIP = sourceIP
	replacement.UserAgent = userAgent
	replacement.Remember = old.Remember
	rotated, e := s.repo.RotateAdminSession(ctx, old.ID, sha256.Sum256([]byte(parts[1])), replacement, now)
	if e != nil {
		return LoginOutput{}, e
	}
	if !rotated {
		return LoginOutput{}, ErrConflict
	}
	u, ok, e := s.repo.GetAdminUserByID(ctx, old.AdminUserID)
	if e != nil {
		return LoginOutput{}, e
	}
	if !ok {
		return LoginOutput{}, ErrUnauthorized
	}
	return LoginOutput{Admin: u, AccessToken: access, RefreshCookieValue: sid + "." + refresh, AccessExpiresAt: replacement.AccessExpiresAt, RefreshExpiresAt: replacement.ExpiresAt, Remember: replacement.Remember}, nil
}
func (s *Service) Logout(ctx context.Context, cookie string) error {
	p := strings.SplitN(cookie, ".", 2)
	if len(p) != 2 || p[0] == "" {
		return nil
	}
	old, ok, err := s.repo.GetAdminSession(ctx, p[0])
	if err != nil || !ok || !old.MatchesRefreshSecretHash(sha256.Sum256([]byte(p[1]))) {
		return nil
	}
	return s.repo.RevokeAdminSession(ctx, p[0], s.clock().UTC(), domain.RevocationLogout)
}

func (s *Service) LogoutAccess(ctx context.Context, token string) error {
	if strings.TrimSpace(token) == "" {
		return nil
	}
	sess, ok, err := s.repo.FindAdminSessionByAccessHash(ctx, sha256.Sum256([]byte(token)))
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	return s.repo.RevokeAdminSession(ctx, sess.ID, s.clock().UTC(), domain.RevocationLogout)
}
