package bootstrap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AokiAx/grok2api/backend/internal/domain/adminauth"
	"github.com/AokiAx/grok2api/backend/internal/repository"
	"github.com/AokiAx/grok2api/backend/internal/security"
)

const (
	minimumAdminPasswordRunes = 12
	maximumAdminPasswordBytes = 1024
)

var (
	ErrWeakPassword              = errors.New("admin password does not meet the bootstrap policy")
	ErrAdminAlreadyExists        = errors.New("administrator already exists")
	ErrBootstrapAlreadyCompleted = errors.New("administrator bootstrap already completed")
)

type AdminBootstrapRandom func([]byte) error
type AdminBootstrapOption func(*AdminBootstrapService)

func WithAdminBootstrapRandom(random AdminBootstrapRandom) AdminBootstrapOption {
	return func(service *AdminBootstrapService) {
		if random != nil {
			service.random = random
		}
	}
}

type AdminBootstrapResult struct {
	Status repository.BootstrapStatus
	Admin  adminauth.AdminUser
}

type AdminBootstrapService struct {
	repository repository.AdminBootstrapRepository
	now        func() time.Time
	bcryptCost int
	random     AdminBootstrapRandom
}

func NewAdminBootstrapService(
	repository repository.AdminBootstrapRepository,
	now func() time.Time,
	bcryptCost int,
	options ...AdminBootstrapOption,
) *AdminBootstrapService {
	service := &AdminBootstrapService{
		repository: repository,
		now:        now,
		bcryptCost: bcryptCost,
		random: func(buffer []byte) error {
			_, err := rand.Read(buffer)
			return err
		},
	}
	for _, option := range options {
		option(service)
	}
	return service
}

func (s *AdminBootstrapService) Bootstrap(ctx context.Context, password string) (AdminBootstrapResult, error) {
	if s == nil || s.repository == nil || s.now == nil || s.random == nil {
		return AdminBootstrapResult{}, errors.New("admin bootstrap dependencies are required")
	}
	if err := validateAdminBootstrapPassword(password); err != nil {
		return AdminBootstrapResult{}, err
	}
	at := s.now()
	if at.IsZero() {
		return AdminBootstrapResult{}, errors.New("admin bootstrap clock returned zero time")
	}
	at = at.UTC()
	credential, err := security.HashAdminPassword(password, s.bcryptCost)
	if err != nil {
		return AdminBootstrapResult{}, err
	}
	id, err := s.adminID()
	if err != nil {
		return AdminBootstrapResult{}, err
	}
	admin, err := adminauth.NewAdminUser(id, "admin", credential, at)
	if err != nil {
		return AdminBootstrapResult{}, err
	}
	status, err := s.repository.BootstrapAdmin(ctx, admin)
	if err != nil {
		return AdminBootstrapResult{}, err
	}
	switch status {
	case repository.BootstrapCreated:
		return AdminBootstrapResult{Status: status, Admin: admin}, nil
	case repository.BootstrapExisting:
		return AdminBootstrapResult{}, ErrAdminAlreadyExists
	case repository.BootstrapAlreadyCompleted:
		return AdminBootstrapResult{}, ErrBootstrapAlreadyCompleted
	default:
		return AdminBootstrapResult{}, errors.New("admin bootstrap repository returned an unsupported status")
	}
}

func (s *AdminBootstrapService) adminID() (string, error) {
	buffer := make([]byte, 16)
	if err := s.random(buffer); err != nil {
		return "", err
	}
	return "admin-" + hex.EncodeToString(buffer), nil
}

func validateAdminBootstrapPassword(password string) error {
	if password == "" || strings.TrimSpace(password) == "" {
		return ErrWeakPassword
	}
	if len([]byte(password)) > maximumAdminPasswordBytes || utf8.RuneCountInString(password) < minimumAdminPasswordRunes {
		return ErrWeakPassword
	}
	return nil
}
