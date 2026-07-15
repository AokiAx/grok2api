package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/domain/settings"
	"github.com/AokiAx/grok2api/backend/internal/repository"
)

// SettingsAdmin is the settings center port.
type SettingsAdmin interface {
	GetSettings(context.Context) (settings.Document, error)
	PutSettings(context.Context, int64, settings.Document, string) (settings.Document, error)
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
		ExpectedRevision int64               `json:"expected_revision"`
		Pool             settings.Pool       `json:"pool"`
		Timeouts         settings.Timeouts   `json:"timeouts"`
		Audit            settings.Audit      `json:"audit"`
		Proxy            settings.Proxy      `json:"proxy"`
		ClientKeys       settings.ClientKeys `json:"client_keys"`
		DeviceAuth       settings.DeviceAuth `json:"device_auth"`
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
	next.ClientKeys = body.ClientKeys
	next.DeviceAuth = body.DeviceAuth
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

func settingsToMap(doc settings.Document) map[string]any {
	return map[string]any{
		"revision":    doc.Revision,
		"updated_at":  doc.UpdatedAt.UTC().Format(time.RFC3339),
		"updated_by":  doc.UpdatedBy,
		"pool":        doc.Pool,
		"timeouts":    doc.Timeouts,
		"audit":       doc.Audit,
		"proxy":       doc.Proxy,
		"client_keys": doc.ClientKeys,
		"device_auth":  doc.DeviceAuth,
	}
}

