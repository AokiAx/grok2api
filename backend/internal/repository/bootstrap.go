package repository

import (
	"context"

	"github.com/AokiAx/grok2api/backend/internal/domain/adminauth"
	"github.com/AokiAx/grok2api/backend/internal/domain/clientkey"
)

type BootstrapStatus string

const (
	AdminBootstrapMarker     = "legacy_admin_bootstrap_v1"
	ClientKeyBootstrapMarker = "legacy_client_key_bootstrap_v1"
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
