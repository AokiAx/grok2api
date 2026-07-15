package repository

import (
	"context"

	"github.com/AokiAx/grok2api/backend/internal/domain/adminauth"
	"github.com/AokiAx/grok2api/backend/internal/domain/clientkey"
)

type BootstrapStatus string

const (
	AdminBootstrapMarker       = "admin_bootstrap_v1"
	LegacyAdminBootstrapMarker = "legacy_admin_bootstrap_v1"
	ClientKeyBootstrapMarker   = "legacy_client_key_bootstrap_v1"
)

const (
	BootstrapCreated          BootstrapStatus = "created"
	BootstrapExisting         BootstrapStatus = "existing"
	BootstrapAlreadyCompleted BootstrapStatus = "already_completed"
)

type LegacySecurityBootstrapRepository interface {
	CountAdminUsers(context.Context) (int, error)
	BootstrapLegacyAdmin(context.Context, adminauth.AdminUser) (BootstrapStatus, error)
	BootstrapLegacyClientKey(context.Context, clientkey.Credential) (BootstrapStatus, error)
}

// AdminBootstrapRepository owns the one-time local administrator bootstrap.
// Implementations must create the administrator and AdminBootstrapMarker in a
// single transaction.
type AdminBootstrapRepository interface {
	BootstrapAdmin(context.Context, adminauth.AdminUser) (BootstrapStatus, error)
}
