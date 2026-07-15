package repository

import (
	"context"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/adminauth"
)

type AdminUserStore interface {
	CountAdminUsers(context.Context) (int, error)
	CreateAdminUser(context.Context, adminauth.AdminUser) error
	GetAdminUserByID(context.Context, string) (adminauth.AdminUser, bool, error)
	GetAdminUserByUsername(context.Context, string) (adminauth.AdminUser, bool, error)
}

type AdminSessionStore interface {
	CreateAdminSession(context.Context, adminauth.Session) error
	CreateAdminSessionWithLoginSuccess(context.Context, adminauth.Session, adminauth.LoginAttempt) error
	CreateAdminSessionWithReservedLoginSuccess(context.Context, int64, adminauth.Session, adminauth.LoginAttempt) error
	GetAdminSession(context.Context, string) (adminauth.Session, bool, error)
	FindAdminSessionByAccessHash(context.Context, [32]byte) (adminauth.Session, bool, error)
	RotateAdminSession(context.Context, string, [32]byte, adminauth.Session, time.Time) (bool, error)
	RevokeAdminSession(context.Context, string, time.Time, adminauth.RevocationReason) error
	RevokeAdminSessionFamily(context.Context, string, time.Time, adminauth.RevocationReason) error
}

type AdminLoginAttemptStore interface {
	RecordAdminLoginAttempt(context.Context, adminauth.LoginAttempt) error
	ReserveAdminLoginAttempt(context.Context, adminauth.LoginAttempt, time.Time, int) (int64, bool, error)
	CompleteAdminLoginFailure(context.Context, int64, string) error
	ReleaseAdminLoginReservation(context.Context, int64) error
	CountRecentAdminLoginFailures(context.Context, string, string, time.Time) (int, error)
	OldestRecentAdminLoginFailure(context.Context, string, string, time.Time) (time.Time, bool, error)
	CountRecentAdminLoginFailuresBySourceIP(context.Context, string, time.Time) (int, error)
	OldestRecentAdminLoginFailureBySourceIP(context.Context, string, time.Time) (time.Time, bool, error)
}

type AdminAuthRetentionCutoffs struct {
	LoginAttemptsBefore    time.Time
	InactiveSessionsBefore time.Time
}

type AdminAuthPruneResult struct {
	LoginAttemptsDeleted int64
	SessionsDeleted      int64
}

type AdminAuthHistoryStore interface {
	PruneAdminAuthHistory(context.Context, AdminAuthRetentionCutoffs) (AdminAuthPruneResult, error)
}

type AdminAuthRepository interface {
	AdminUserStore
	AdminSessionStore
	AdminLoginAttemptStore
	AdminAuthHistoryStore
}
