package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/settings"
	"github.com/AokiAx/grok2api/backend/internal/repository"
)

// SettingsAdmin is the versioned settings center port.
type SettingsAdmin interface {
	GetSettings(context.Context) (settings.Document, error)
	PutSettings(context.Context, int64, settings.Document, string) (settings.Document, error)
	ListSettingsSnapshots(context.Context, int) ([]settings.Snapshot, error)
	GetSettingsSnapshot(context.Context, int64) (settings.Snapshot, bool, error)
	RollbackSettings(context.Context, int64, int64, string) (settings.Document, error)
}

// SettingsApplier applies accepted settings to live runtime components.
type SettingsApplier interface {
	ApplySettings(settings.Document) error
}

func (s *Server) registerSettingsRoutes(mux *http.ServeMux) {
	if s == nil || mux == nil || s.settings == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/v1/settings", s.adminV1GetSettings)
	mux.HandleFunc("PUT /api/admin/v1/settings", s.adminV1PutSettings)
	mux.HandleFunc("GET /api/admin/v1/settings/snapshots", s.adminV1ListSettingsSnapshots)
	mux.HandleFunc("GET /api/admin/v1/settings/snapshots/{revision}", s.adminV1GetSettingsSnapshot)
	mux.HandleFunc("POST /api/admin/v1/settings/rollback", s.adminV1RollbackSettings)
}

func (s *Server) adminV1GetSettings(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	doc, err := s.settings.GetSettings(request.Context())
	if err != nil {
		writeAdminError(writer, http.StatusInternalServerError, "settings_get_failed", err.Error())
		return
	}
	writeAdminOK(writer, http.StatusOK, settingsToMap(doc))
}

func (s *Server) adminV1PutSettings(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	var body struct {
		ExpectedRevision int64             `json:"expected_revision"`
		Pool             settings.Pool     `json:"pool"`
		Timeouts         settings.Timeouts `json:"timeouts"`
		Audit            settings.Audit    `json:"audit"`
		Proxy            settings.Proxy    `json:"proxy"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1<<20)).Decode(&body); err != nil {
		writeAdminError(writer, http.StatusBadRequest, "invalid_json", "Invalid settings payload")
		return
	}
	current, err := s.settings.GetSettings(request.Context())
	if err != nil {
		writeAdminError(writer, http.StatusInternalServerError, "settings_get_failed", err.Error())
		return
	}
	if body.ExpectedRevision <= 0 {
		writeAdminError(writer, http.StatusBadRequest, "revision_required", "expected_revision is required")
		return
	}
	next := current
	next.Pool = body.Pool
	next.Timeouts = body.Timeouts
	next.Audit = body.Audit
	next.Proxy = body.Proxy
	updatedBy := "admin"
	doc, err := s.settings.PutSettings(request.Context(), body.ExpectedRevision, next, updatedBy)
	if errors.Is(err, repository.ErrSettingsConflict) {
		writeAdminError(writer, http.StatusConflict, "revision_conflict", "Settings were updated by another request; reload and retry")
		return
	}
	if err != nil {
		writeAdminError(writer, http.StatusBadRequest, "settings_put_failed", err.Error())
		return
	}
	if s.settingsApplier != nil {
		if err := s.settingsApplier.ApplySettings(doc); err != nil {
			writeAdminError(writer, http.StatusInternalServerError, "settings_apply_failed", err.Error())
			return
		}
	}
	writeAdminOK(writer, http.StatusOK, settingsToMap(doc))
}

func (s *Server) adminV1ListSettingsSnapshots(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	limit, _ := strconv.Atoi(strings.TrimSpace(request.URL.Query().Get("limit")))
	items, err := s.settings.ListSettingsSnapshots(request.Context(), limit)
	if err != nil {
		writeAdminError(writer, http.StatusInternalServerError, "settings_snapshots_failed", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		out = append(out, snapshotToMap(item))
	}
	writeAdminOK(writer, http.StatusOK, map[string]any{"count": len(out), "snapshots": out})
}

func (s *Server) adminV1GetSettingsSnapshot(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	rev, err := strconv.ParseInt(request.PathValue("revision"), 10, 64)
	if err != nil {
		writeAdminError(writer, http.StatusBadRequest, "invalid_revision", "revision must be an integer")
		return
	}
	item, found, err := s.settings.GetSettingsSnapshot(request.Context(), rev)
	if err != nil {
		writeAdminError(writer, http.StatusInternalServerError, "settings_snapshot_failed", err.Error())
		return
	}
	if !found {
		writeAdminError(writer, http.StatusNotFound, "not_found", "settings snapshot not found")
		return
	}
	writeAdminOK(writer, http.StatusOK, snapshotToMap(item))
}

func (s *Server) adminV1RollbackSettings(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	var body struct {
		ExpectedRevision int64 `json:"expected_revision"`
		TargetRevision   int64 `json:"target_revision"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1<<20)).Decode(&body); err != nil {
		writeAdminError(writer, http.StatusBadRequest, "invalid_json", "Invalid rollback payload")
		return
	}
	if body.TargetRevision <= 0 {
		writeAdminError(writer, http.StatusBadRequest, "invalid_revision", "target_revision is required")
		return
	}
	if body.ExpectedRevision <= 0 {
		writeAdminError(writer, http.StatusBadRequest, "revision_required", "expected_revision is required")
		return
	}
	doc, err := s.settings.RollbackSettings(request.Context(), body.ExpectedRevision, body.TargetRevision, "admin")
	if errors.Is(err, repository.ErrSettingsConflict) {
		writeAdminError(writer, http.StatusConflict, "revision_conflict", "Settings were updated by another request; reload and retry")
		return
	}
	if errors.Is(err, repository.ErrSettingsSnapshotGone) {
		writeAdminError(writer, http.StatusNotFound, "not_found", "settings snapshot not found")
		return
	}
	if err != nil {
		writeAdminError(writer, http.StatusBadRequest, "settings_rollback_failed", err.Error())
		return
	}
	if s.settingsApplier != nil {
		if err := s.settingsApplier.ApplySettings(doc); err != nil {
			writeAdminError(writer, http.StatusInternalServerError, "settings_apply_failed", err.Error())
			return
		}
	}
	writeAdminOK(writer, http.StatusOK, settingsToMap(doc))
}

func settingsToMap(doc settings.Document) map[string]any {
	return map[string]any{
		"revision":   doc.Revision,
		"updated_at": doc.UpdatedAt.UTC().Format(time.RFC3339),
		"updated_by": doc.UpdatedBy,
		"pool":       doc.Pool,
		"timeouts":   doc.Timeouts,
		"audit":      doc.Audit,
		"proxy":      doc.Proxy,
	}
}

func snapshotToMap(item settings.Snapshot) map[string]any {
	return map[string]any{
		"revision":   item.Revision,
		"created_at": item.CreatedAt.UTC().Format(time.RFC3339),
		"created_by": item.CreatedBy,
		"reason":     item.Reason,
		"document":   settingsToMap(item.Document),
	}
}
