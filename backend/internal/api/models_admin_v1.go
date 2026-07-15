package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/modelreg"
	"github.com/AokiAx/grok2api/backend/internal/upstream"
)

// ModelAdmin is the admin-facing model registry surface.
type ModelAdmin interface {
	ListModels(context.Context, bool) ([]modelreg.Model, error)
	GetModel(context.Context, string) (modelreg.Model, bool, error)
	UpsertModel(context.Context, modelreg.Model) error
}

func (s *Server) registerModelAdminRoutes(mux *http.ServeMux) {
	if s == nil || mux == nil || s.modelAdmin == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/v1/models", s.adminV1ListModels)
	mux.HandleFunc("GET /api/admin/v1/models/{id}", s.adminV1GetModel)
	mux.HandleFunc("PUT /api/admin/v1/models/{id}", s.adminV1PutModel)
	mux.HandleFunc("PATCH /api/admin/v1/models/{id}", s.adminV1PatchModel)
}

func (s *Server) adminV1ListModels(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	includeDisabled := strings.EqualFold(request.URL.Query().Get("include_disabled"), "true") ||
		request.URL.Query().Get("include_disabled") == "1"
	items, err := s.modelAdmin.ListModels(request.Context(), includeDisabled)
	if err != nil {
		writeAdminError(writer, http.StatusInternalServerError, "list_models_failed", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(items))
	enabled := 0
	for _, item := range items {
		if item.Enabled {
			enabled++
		}
		out = append(out, modelToAdminMap(item))
	}
	writeAdminOK(writer, http.StatusOK, map[string]any{
		"count":   len(out),
		"enabled": enabled,
		"models":  out,
	})
}

func (s *Server) adminV1GetModel(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	item, found, err := s.modelAdmin.GetModel(request.Context(), request.PathValue("id"))
	if err != nil {
		writeAdminError(writer, http.StatusInternalServerError, "get_model_failed", err.Error())
		return
	}
	if !found {
		writeAdminError(writer, http.StatusNotFound, "not_found", "model not found")
		return
	}
	writeAdminOK(writer, http.StatusOK, modelToAdminMap(item))
}

func (s *Server) adminV1PutModel(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	var body modelAdminWrite
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1<<20)).Decode(&body); err != nil {
		writeAdminError(writer, http.StatusBadRequest, "invalid_json", "Invalid model payload")
		return
	}
	id := strings.TrimSpace(request.PathValue("id"))
	item := body.toModel(id)
	item.Source = "managed"
	if err := s.modelAdmin.UpsertModel(request.Context(), item); err != nil {
		writeAdminError(writer, http.StatusBadRequest, "upsert_failed", err.Error())
		return
	}
	stored, found, err := s.modelAdmin.GetModel(request.Context(), id)
	if err != nil || !found {
		writeAdminError(writer, http.StatusInternalServerError, "get_model_failed", "model saved but could not be reloaded")
		return
	}
	// Refresh runtime catalog facade when possible.
	s.refreshModelCatalog(request.Context())
	writeAdminOK(writer, http.StatusOK, modelToAdminMap(stored))
}

func (s *Server) adminV1PatchModel(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	id := strings.TrimSpace(request.PathValue("id"))
	existing, found, err := s.modelAdmin.GetModel(request.Context(), id)
	if err != nil {
		writeAdminError(writer, http.StatusInternalServerError, "get_model_failed", err.Error())
		return
	}
	if !found {
		writeAdminError(writer, http.StatusNotFound, "not_found", "model not found")
		return
	}
	var body modelAdminPatch
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1<<20)).Decode(&body); err != nil {
		writeAdminError(writer, http.StatusBadRequest, "invalid_json", "Invalid model patch")
		return
	}
	body.apply(&existing)
	existing.Source = "managed"
	existing.UpdatedAt = time.Now().UTC()
	if err := s.modelAdmin.UpsertModel(request.Context(), existing); err != nil {
		writeAdminError(writer, http.StatusBadRequest, "upsert_failed", err.Error())
		return
	}
	s.refreshModelCatalog(request.Context())
	writeAdminOK(writer, http.StatusOK, modelToAdminMap(existing))
}

type modelAdminWrite struct {
	UpstreamID              string   `json:"upstream_id"`
	Name                    string   `json:"name"`
	APIBackend              string   `json:"api_backend"`
	ContextWindow           int      `json:"context_window"`
	SupportsReasoningEffort bool     `json:"supports_reasoning_effort"`
	ReasoningEfforts        []string `json:"reasoning_efforts"`
	SupportsBackendSearch   bool     `json:"supports_backend_search"`
	OwnedBy                 string   `json:"owned_by"`
	Enabled                 *bool    `json:"enabled"`
	Aliases                 []string `json:"aliases"`
}

func (b modelAdminWrite) toModel(id string) modelreg.Model {
	enabled := true
	if b.Enabled != nil {
		enabled = *b.Enabled
	}
	now := time.Now().UTC()
	return modelreg.Model{
		ID:                      id,
		UpstreamID:              b.UpstreamID,
		Name:                    b.Name,
		APIBackend:              b.APIBackend,
		ContextWindow:           b.ContextWindow,
		SupportsReasoningEffort: b.SupportsReasoningEffort,
		ReasoningEfforts:        append([]string(nil), b.ReasoningEfforts...),
		SupportsBackendSearch:   b.SupportsBackendSearch,
		OwnedBy:                 b.OwnedBy,
		Enabled:                 enabled,
		Aliases:                 append([]string(nil), b.Aliases...),
		Source:                  "managed",
		CreatedAt:               now,
		UpdatedAt:               now,
	}
}

type modelAdminPatch struct {
	UpstreamID              *string  `json:"upstream_id"`
	Name                    *string  `json:"name"`
	APIBackend              *string  `json:"api_backend"`
	ContextWindow           *int     `json:"context_window"`
	SupportsReasoningEffort *bool    `json:"supports_reasoning_effort"`
	ReasoningEfforts        []string `json:"reasoning_efforts"`
	SupportsBackendSearch   *bool    `json:"supports_backend_search"`
	OwnedBy                 *string  `json:"owned_by"`
	Enabled                 *bool    `json:"enabled"`
	Aliases                 []string `json:"aliases"`
}

func (p modelAdminPatch) apply(item *modelreg.Model) {
	if p.UpstreamID != nil {
		item.UpstreamID = *p.UpstreamID
	}
	if p.Name != nil {
		item.Name = *p.Name
	}
	if p.APIBackend != nil {
		item.APIBackend = *p.APIBackend
	}
	if p.ContextWindow != nil {
		item.ContextWindow = *p.ContextWindow
	}
	if p.SupportsReasoningEffort != nil {
		item.SupportsReasoningEffort = *p.SupportsReasoningEffort
	}
	if p.ReasoningEfforts != nil {
		item.ReasoningEfforts = append([]string(nil), p.ReasoningEfforts...)
	}
	if p.SupportsBackendSearch != nil {
		item.SupportsBackendSearch = *p.SupportsBackendSearch
	}
	if p.OwnedBy != nil {
		item.OwnedBy = *p.OwnedBy
	}
	if p.Enabled != nil {
		item.Enabled = *p.Enabled
	}
	if p.Aliases != nil {
		item.Aliases = append([]string(nil), p.Aliases...)
	}
}

func modelToAdminMap(item modelreg.Model) map[string]any {
	return map[string]any{
		"id":                        item.ID,
		"upstream_id":               item.ResolveUpstream(),
		"name":                      item.Name,
		"api_backend":               item.APIBackend,
		"context_window":            item.ContextWindow,
		"supports_reasoning_effort": item.SupportsReasoningEffort,
		"reasoning_efforts":         item.ReasoningEfforts,
		"supports_backend_search":   item.SupportsBackendSearch,
		"owned_by":                  item.OwnedBy,
		"enabled":                   item.Enabled,
		"aliases":                   item.Aliases,
		"source":                    item.Source,
		"created_at":                item.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at":                item.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func (s *Server) refreshModelCatalog(ctx context.Context) {
	if s == nil || s.modelAdmin == nil {
		return
	}
	items, err := s.modelAdmin.ListModels(ctx, false)
	if err != nil {
		return
	}
	if s.onModelsChanged != nil {
		s.onModelsChanged(items)
	}
	// Always rebuild local catalog facade for subsequent /v1/models enrichment.
	catalogItems := make([]upstream.ModelInfo, 0, len(items))
	for _, model := range items {
		if !model.Enabled {
			continue
		}
		info := upstream.ModelInfo{
			ID: model.ID, UpstreamID: model.ResolveUpstream(), Name: model.Name, APIBackend: model.APIBackend, ContextWindow: model.ContextWindow,
			SupportsReasoningEffort: model.SupportsReasoningEffort, ReasoningEfforts: append([]string(nil), model.ReasoningEfforts...),
			SupportsBackendSearch: model.SupportsBackendSearch, OwnedBy: model.OwnedBy,
		}
		catalogItems = append(catalogItems, info)
		for _, alias := range model.Aliases {
			aliasInfo := info
			aliasInfo.ID = alias
			catalogItems = append(catalogItems, aliasInfo)
		}
	}
	if len(catalogItems) == 0 {
		s.modelCatalog = upstream.NewDefaultCatalog()
	} else {
		s.modelCatalog = upstream.NewCatalog(catalogItems)
	}
	if s.bridge != nil {
		s.bridge.Catalog = s.modelCatalog
	}
}
