package repository

import (
	"context"

	"github.com/AokiAx/grok2api/backend/internal/domain/deviceauth"
)

// DeviceAuthRepository persists Build Device OAuth sessions.
type DeviceAuthRepository interface {
	CreateDeviceAuthSession(context.Context, deviceauth.Session) error
	GetDeviceAuthSession(context.Context, string) (deviceauth.Session, bool, error)
	UpdateDeviceAuthSession(context.Context, deviceauth.Session) error
	ListDeviceAuthSessions(context.Context, int) ([]deviceauth.Session, error)
}
