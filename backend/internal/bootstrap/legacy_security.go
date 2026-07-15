package bootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/adminauth"
	"github.com/AokiAx/grok2api/backend/internal/domain/clientkey"
	"github.com/AokiAx/grok2api/backend/internal/repository"
	"github.com/AokiAx/grok2api/backend/internal/security"
)

const (
	AdminBootstrapMarker     = repository.LegacyAdminBootstrapMarker
	ClientKeyBootstrapMarker = repository.ClientKeyBootstrapMarker
)

const (
	BootstrapSkipped          repository.BootstrapStatus = "skipped"
	BootstrapCreated                                     = repository.BootstrapCreated
	BootstrapExisting                                    = repository.BootstrapExisting
	BootstrapAlreadyCompleted                            = repository.BootstrapAlreadyCompleted
)

type LegacySecrets struct {
	PanelPassword string
	AppKey        string
	APIKey        string
}

type LegacySecurityResult struct {
	Admin              repository.BootstrapStatus
	ClientKey          repository.BootstrapStatus
	AdminSetupRequired bool
}

type LegacySecurityService struct {
	repository repository.LegacySecurityBootstrapRepository
	now        func() time.Time
	bcryptCost int
}

func NewLegacySecurityService(
	repository repository.LegacySecurityBootstrapRepository,
	now func() time.Time,
	bcryptCost int,
) *LegacySecurityService {
	return &LegacySecurityService{repository: repository, now: now, bcryptCost: bcryptCost}
}

func (s *LegacySecurityService) Bootstrap(ctx context.Context, secrets LegacySecrets) (LegacySecurityResult, error) {
	result := LegacySecurityResult{Admin: BootstrapSkipped, ClientKey: BootstrapSkipped, AdminSetupRequired: true}
	if s == nil || s.repository == nil || s.now == nil {
		return result, errors.New("legacy security bootstrap dependencies are required")
	}
	at := s.now()
	if at.IsZero() {
		return result, errors.New("legacy security bootstrap clock returned zero time")
	}
	at = at.UTC()

	password := firstNonEmpty(secrets.PanelPassword, secrets.AppKey)
	if password != "" {
		credential, err := security.HashAdminPassword(password, s.bcryptCost)
		if err != nil {
			return result, err
		}
		admin, err := adminauth.NewAdminUser("admin-legacy-bootstrap", "admin", credential, at)
		if err != nil {
			return result, err
		}
		result.Admin, err = s.repository.BootstrapLegacyAdmin(ctx, admin)
		if err != nil {
			return result, err
		}
	}

	apiKey := strings.TrimSpace(secrets.APIKey)
	if apiKey != "" {
		hash := sha256.Sum256([]byte(apiKey))
		key, err := clientkey.NewCredential(clientkey.ClientKey{
			ID:            "client-key-legacy-config",
			Name:          "legacy",
			Origin:        clientkey.OriginConfigAPIKey,
			KeyHash:       hash,
			KeyPrefix:     "legacy_" + hex.EncodeToString(hash[:4]),
			ModelPolicy:   clientkey.ModelPolicyAll,
			RPMLimit:      0,
			MaxConcurrent: 0,
			CreatedAt:     at,
			UpdatedAt:     at,
		}, nil)
		if err != nil {
			return result, err
		}
		result.ClientKey, err = s.repository.BootstrapLegacyClientKey(ctx, key)
		if err != nil {
			return result, err
		}
	}

	adminCount, err := s.repository.CountAdminUsers(ctx)
	if err != nil {
		return result, err
	}
	result.AdminSetupRequired = adminCount == 0
	return result, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
