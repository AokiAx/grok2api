package repository_test

import (
	"context"
	"testing"

	"github.com/AokiAx/grok2api/backend/internal/domain/adminauth"
	"github.com/AokiAx/grok2api/backend/internal/repository"
)

type adminBootstrapContractRepository struct{}

func (adminBootstrapContractRepository) CountAdminUsers(context.Context) (int, error) {
	return 0, nil
}

func (adminBootstrapContractRepository) BootstrapAdmin(context.Context, adminauth.AdminUser) (repository.BootstrapStatus, error) {
	return repository.BootstrapCreated, nil
}

func TestAdminBootstrapRepositoryPort(t *testing.T) {
	var _ repository.AdminBootstrapRepository = adminBootstrapContractRepository{}
}
