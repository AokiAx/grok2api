package repository

import (
	"context"

	"github.com/AokiAx/grok2api/backend/internal/domain/modelreg"
)

// ModelRegistryReader loads catalog models.
type ModelRegistryReader interface {
	ListModels(context.Context, bool) ([]modelreg.Model, error) // includeDisabled
	GetModel(context.Context, string) (modelreg.Model, bool, error)
}

// ModelRegistryWriter persists catalog models.
type ModelRegistryWriter interface {
	UpsertModel(context.Context, modelreg.Model) error
	SeedModels(context.Context, []modelreg.Model) (int, error)
}

// ModelRegistryRepository is the full model registry port.
type ModelRegistryRepository interface {
	ModelRegistryReader
	ModelRegistryWriter
}
