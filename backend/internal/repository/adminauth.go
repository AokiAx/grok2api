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
	GetAdminSession(context.Context, string) (adminauth.Session, bool, error)
	FindAdminSessionByAccessHash(context.Context, [32]byte) (adminauth.Session, bool, error)
	RotateAdminSession(context.Context, string, [32]byte, adminauth.Session, time.Time) (bool, error)
	RevokeAdminSession(context.Context, string, time.Time, adminauth.RevocationReason) error
	RevokeAdminSessionFamily(context.Context, string, time.Time, adminauth.RevocationReason) error
}

type AdminLoginAttemptStore interface {
	RecordAdminLoginAttempt(context.Context, adminauth.LoginAttempt) error
	CountRecentAdminLoginFailures(context.Context, string, string, time.Time) (int, error)
}

type AdminAuthRepository interface {
	AdminUserStore
	AdminSessionStore
	AdminLoginAttemptStore
}
