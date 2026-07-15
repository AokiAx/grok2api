package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/AokiAx/grok2api/backend/internal/deviceauth"
	domain "github.com/AokiAx/grok2api/backend/internal/domain/deviceauth"
)

// DeviceAuthAdmin is the admin Build Device OAuth surface.
type DeviceAuthAdmin interface {
	Start(ctx context.Context, req deviceauth.StartRequest) (domain.Session, error)
	Get(ctx context.Context, id string) (domain.Session, bool, error)
	Cancel(ctx context.Context, id string) (domain.Session, error)
	PollOnce(ctx context.Context, id string) (domain.Session, error)
}

func (s *Server) registerDeviceAuthRoutes(mux *http.ServeMux) {
	if s == nil || mux == nil || s.deviceAuth == nil {
		return
	}
	mux.HandleFunc("POST /api/admin/v1/device-auth/sessions", s.adminV1StartDeviceAuth)
	mux.HandleFunc("GET /api/admin/v1/device-auth/sessions/{id}", s.adminV1GetDeviceAuth)
	mux.HandleFunc("POST /api/admin/v1/device-auth/sessions/{id}/cancel", s.adminV1CancelDeviceAuth)
	mux.HandleFunc("POST /api/admin/v1/device-auth/sessions/{id}/poll", s.adminV1PollDeviceAuth)
}

func (s *Server) adminV1StartDeviceAuth(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	var body deviceauth.StartRequest
	decoder := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 1<<20))
	if err := decoder.Decode(&body); err != nil {
		writeAdminError(writer, http.StatusBadRequest, "invalid_json", "Invalid device authorization payload")
		return
	}
	session, err := s.deviceAuth.Start(request.Context(), body)
	if err != nil {
		msg := strings.TrimSpace(err.Error())
		if msg == "" {
			msg = "Failed to start device authorization"
		}
		writeAdminError(writer, http.StatusBadGateway, "device_auth_start_failed", msg)
		return
	}
	writeAdminOK(writer, http.StatusCreated, session.Public())
}

func (s *Server) adminV1GetDeviceAuth(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	session, found, err := s.deviceAuth.Get(request.Context(), request.PathValue("id"))
	if err != nil {
		writeAdminError(writer, http.StatusInternalServerError, "device_auth_get_failed", "Failed to load device authorization session")
		return
	}
	if !found {
		writeAdminError(writer, http.StatusNotFound, "not_found", "device auth session not found")
		return
	}
	writeAdminOK(writer, http.StatusOK, session.Public())
}

func (s *Server) adminV1CancelDeviceAuth(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	session, err := s.deviceAuth.Cancel(request.Context(), request.PathValue("id"))
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeAdminError(writer, http.StatusNotFound, "not_found", "device auth session not found")
			return
		}
		writeAdminError(writer, http.StatusInternalServerError, "device_auth_cancel_failed", "Failed to cancel device authorization")
		return
	}
	writeAdminOK(writer, http.StatusOK, session.Public())
}

func (s *Server) adminV1PollDeviceAuth(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	// Manual poll endpoint for operators; background worker also polls.
	session, err := s.deviceAuth.PollOnce(request.Context(), request.PathValue("id"))
	if err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeAdminError(writer, http.StatusNotFound, "not_found", "device auth session not found")
			return
		}
		writeAdminError(writer, http.StatusInternalServerError, "device_auth_poll_failed", "Failed to poll device authorization")
		return
	}
	writeAdminOK(writer, http.StatusOK, session.Public())
}
