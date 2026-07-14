package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/AokiAx/grok2api/backend/internal/admin"
	"github.com/AokiAx/grok2api/backend/internal/repository"
	"github.com/AokiAx/grok2api/backend/internal/service"
)

const adminAPIVersion = "v1"

// registerAdminRoutes mounts both the legacy /admin/api/* surface (used by the
// embedded panel) and the frozen /api/admin/v1/* contract for the next frontend.
func (s *Server) registerAdminRoutes(mux *http.ServeMux) {
	// Public panel meta (auth requirement probe).
	mux.HandleFunc("GET /admin/api/panel-meta", s.panelMeta)
	mux.HandleFunc("GET /api/admin/v1/system/meta", s.adminV1Meta)

	// Health aliases (k8s-friendly).
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.readyz)

	if s.admin == nil {
		return
	}

	// ---- legacy (panel.html) ----
	mux.HandleFunc("GET /admin/api/cli-accounts", s.adminList)
	mux.HandleFunc("DELETE /admin/api/cli-accounts/{id}", s.adminDelete)
	mux.HandleFunc("POST /admin/api/cli-accounts/{id}/recover", s.adminRecover)
	mux.HandleFunc("POST /admin/api/accounts/import/preview", s.adminImportPreview)
	mux.HandleFunc("POST /admin/api/accounts/import", s.adminImport)

	// ---- versioned management API contract ----
	mux.HandleFunc("POST /api/admin/v1/auth/login", s.adminV1Login)
	mux.HandleFunc("POST /api/admin/v1/auth/logout", s.adminV1Logout)
	mux.HandleFunc("POST /api/admin/v1/auth/refresh", s.adminV1Refresh)
	mux.HandleFunc("GET /api/admin/v1/auth/me", s.adminV1Me)
	mux.HandleFunc("GET /api/admin/v1/me", s.adminV1Me)
	mux.HandleFunc("GET /api/admin/v1/dashboard", s.adminV1Dashboard)
	mux.HandleFunc("GET /api/admin/v1/pool", s.adminV1Pool)
	mux.HandleFunc("GET /api/admin/v1/system", s.adminV1System)
	mux.HandleFunc("GET /api/admin/v1/accounts", s.adminV1List)
	mux.HandleFunc("GET /api/admin/v1/accounts/summary", s.adminV1AccountsSummary)
	mux.HandleFunc("POST /api/admin/v1/accounts/batch", s.adminV1BatchAccounts)
	mux.HandleFunc("GET /api/admin/v1/accounts/{id}", s.adminV1GetAccount)
	mux.HandleFunc("PATCH /api/admin/v1/accounts/{id}", s.adminV1UpdateAccount)
	mux.HandleFunc("GET /api/admin/v1/accounts/{id}/events", s.adminV1AccountEvents)
	mux.HandleFunc("DELETE /api/admin/v1/accounts/{id}", s.adminV1Delete)
	mux.HandleFunc("POST /api/admin/v1/accounts/{id}/recover", s.adminV1Recover)
	mux.HandleFunc("POST /api/admin/v1/accounts/{id}/refresh-token", s.adminV1RefreshCredential)
	mux.HandleFunc("POST /api/admin/v1/accounts/{id}/refresh-quota", s.adminV1RefreshQuota)
	mux.HandleFunc("POST /api/admin/v1/accounts/{id}/refresh-billing", s.adminV1RefreshBilling)
	mux.HandleFunc("GET /api/admin/v1/accounts/{id}/credentials/export", s.adminV1ExportCredential)
	mux.HandleFunc("POST /api/admin/v1/accounts/import/preview", s.adminV1ImportPreview)
	mux.HandleFunc("POST /api/admin/v1/accounts/import", s.adminV1Import)
}

func (s *Server) readyz(writer http.ResponseWriter, _ *http.Request) {
	status := s.status.PoolStatus()
	ready := status.Ready > 0
	code := http.StatusOK
	if !ready {
		code = http.StatusServiceUnavailable
	}
	writeJSON(writer, code, map[string]any{
		"ready":        ready,
		"state":        map[bool]string{true: "ready", false: "not_ready"}[ready],
		"account_pool": status,
	})
}

// --- envelope helpers -------------------------------------------------------

func writeAdminOK(writer http.ResponseWriter, status int, data any) {
	if status <= 0 {
		status = http.StatusOK
	}
	writeJSON(writer, status, map[string]any{
		"ok":    true,
		"data":  data,
		"error": nil,
	})
}

func writeAdminError(writer http.ResponseWriter, status int, code, message string) {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	if code == "" {
		code = "error"
	}
	writeJSON(writer, status, map[string]any{
		"ok":   false,
		"data": nil,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}

func (s *Server) requireAdmin(writer http.ResponseWriter, request *http.Request) bool {
	if authorizedWithKey(request, s.adminKey) {
		return true
	}
	writeAdminError(writer, http.StatusUnauthorized, "unauthorized", "Invalid admin key")
	return false
}

// --- v1 handlers ------------------------------------------------------------

func (s *Server) adminV1Meta(writer http.ResponseWriter, _ *http.Request) {
	writeAdminOK(writer, http.StatusOK, map[string]any{
		"auth_required": s.adminKey != "",
		"version":       "1.0.0-go",
		"api_version":   adminAPIVersion,
		"panel_paths":   []string{"/"},
	})
}

func (s *Server) adminV1Login(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Password string `json:"password"`
		Token    string `json:"token"`
		Username string `json:"username"`
	}
	_ = json.NewDecoder(io.LimitReader(request.Body, 1<<20)).Decode(&body)
	candidate := strings.TrimSpace(body.Password)
	if candidate == "" {
		candidate = strings.TrimSpace(body.Token)
	}
	if s.adminKey == "" {
		writeAdminOK(writer, http.StatusOK, adminLoginPayload("admin", "open"))
		return
	}
	if candidate == "" || !constantTimeEqual(candidate, s.adminKey) {
		writeAdminError(writer, http.StatusUnauthorized, "unauthorized", "Invalid admin key")
		return
	}
	writeAdminOK(writer, http.StatusOK, adminLoginPayload("admin", candidate))
}

func (s *Server) adminV1Logout(writer http.ResponseWriter, _ *http.Request) {
	writeAdminOK(writer, http.StatusOK, map[string]any{"loggedOut": true})
}

func (s *Server) adminV1Refresh(writer http.ResponseWriter, request *http.Request) {
	if !authorizedWithKey(request, s.adminKey) && s.adminKey != "" {
		writeAdminError(writer, http.StatusUnauthorized, "unauthorized", "Invalid admin key")
		return
	}
	token := s.adminKey
	if token == "" {
		token = "open"
	}
	writeAdminOK(writer, http.StatusOK, adminTokens(token))
}

func (s *Server) adminV1Me(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	writeAdminOK(writer, http.StatusOK, map[string]any{
		"id":       "admin",
		"username": "admin",
	})
}

func (s *Server) adminV1Dashboard(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	summary, err := s.adminStatsMerged(request)
	if err != nil {
		writeAdminError(writer, http.StatusInternalServerError, "stats_failed", err.Error())
		return
	}
	now := time.Now().UTC()
	period := strings.TrimSpace(request.URL.Query().Get("period"))
	if period == "" {
		period = "30d"
	}
	writeAdminOK(writer, http.StatusOK, map[string]any{
		"period":      period,
		"generatedAt": now.Format(time.RFC3339),
		"range": map[string]any{
			"start": now.Add(-30 * 24 * time.Hour).Format(time.RFC3339),
			"end":   now.Format(time.RFC3339),
		},
		"resources": map[string]any{
			"activeAccounts":   summary.ReadyAccounts,
			"totalAccounts":    summary.TotalAccounts,
			"enabledModels":    1,
			"totalModels":      1,
			"activeClientKeys": 0,
			"totalClientKeys":  0,
			"allTimeRequests":  summary.TotalRequests,
		},
		"usage": map[string]any{
			"requests":           summary.TotalRequests,
			"successfulRequests": summary.TotalRequests,
			"failedRequests":     0,
			"inputTokens":        0,
			"cachedInputTokens":  0,
			"outputTokens":       0,
			"reasoningTokens":    0,
			"tokens":             0,
			"billedCostUsdTicks": 0,
			"successRate":        100,
		},
		"series":       []any{},
		"topModels":    []any{},
		"summary":      summary,
		"account_pool": s.status.PoolStatus(),
		"generated_at": now.Format(time.RFC3339),
	})
}

func (s *Server) adminV1Pool(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	pool := s.status.PoolStatus()
	var circuit any
	if provider, ok := s.gateway.(interface {
		CircuitStatus() service.CircuitStatus
	}); ok {
		circuit = provider.CircuitStatus()
	}
	writeAdminOK(writer, http.StatusOK, map[string]any{
		"ready":         pool.Ready,
		"unavailable":   pool.Unavailable,
		"reasons":       pool.Reasons,
		"quota_circuit": circuit,
	})
}

func (s *Server) adminV1System(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	writeAdminOK(writer, http.StatusOK, map[string]any{
		"version":       "1.0.0-go",
		"api_version":   adminAPIVersion,
		"default_model": s.defaultModel,
		"auth_required": s.adminKey != "",
		// Intentionally no secrets / raw config dump.
	})
}

func (s *Server) adminV1List(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	q := request.URL.Query()
	if q.Get("page_size") == "" && q.Get("pageSize") != "" {
		q.Set("page_size", q.Get("pageSize"))
	}
	if q.Get("q") == "" && q.Get("search") != "" {
		q.Set("q", q.Get("search"))
	}
	if q.Get("pool") == "" {
		switch strings.ToLower(strings.TrimSpace(q.Get("status"))) {
		case "available", "ready", "active":
			q.Set("pool", "ready")
		case "unavailable", "recovering", "attention":
			q.Set("pool", "unavailable")
		}
	}
	request.URL.RawQuery = q.Encode()

	payload, err := s.buildAdminListPayload(request)
	if err != nil {
		writeAdminError(writer, http.StatusInternalServerError, "list_failed", err.Error())
		return
	}
	items := make([]map[string]any, 0)
	switch raw := payload["accounts"].(type) {
	case []map[string]any:
		for _, a := range raw {
			items = append(items, accountDTO(a))
		}
	case []any:
		for _, item := range raw {
			if m, ok := item.(map[string]any); ok {
				items = append(items, accountDTO(m))
			}
		}
	}
	page := asInt(payload["page"])
	pageSize := asInt(payload["page_size"])
	total := asInt(payload["total"])
	writeAdminOK(writer, http.StatusOK, map[string]any{
		"items":       items,
		"page":        page,
		"pageSize":    pageSize,
		"total":       total,
		"accounts":    payload["accounts"],
		"count":       total,
		"total_pages": payload["total_pages"],
		"summary":     payload["summary"],
	})
}

func (s *Server) adminV1AccountsSummary(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	summary, err := s.adminStatsMerged(request)
	if err != nil {
		writeAdminError(writer, http.StatusInternalServerError, "stats_failed", err.Error())
		return
	}
	reasons := summary.Reasons
	if reasons == nil {
		reasons = map[string]int{}
	}
	writeAdminOK(writer, http.StatusOK, map[string]any{
		"total":      summary.TotalAccounts,
		"available":  summary.ReadyAccounts,
		"recovering": summary.RetryDue,
		"attention":  summary.UnavailableAccounts,
		"providers": map[string]any{
			"grok_build":   map[string]any{"total": summary.TotalAccounts, "available": summary.ReadyAccounts},
			"grok_web":     map[string]any{"total": 0, "available": 0},
			"grok_console": map[string]any{"total": 0, "available": 0},
		},
		"recovery": map[string]any{
			"cooldown":     reasons["cooldown"],
			"waitingReset": reasons["quota"],
			"probing":      reasons["validating"],
		},
		"issues": map[string]any{
			"disabled":       reasons["disabled"],
			"reauthRequired": reasons["auth"],
		},
	})
}

func (s *Server) adminV1GetAccount(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	item, err := s.admin.Get(request.Context(), request.PathValue("id"))
	if err != nil {
		writeAdminServiceError(writer, err, "get_failed")
		return
	}
	writeAdminOK(writer, http.StatusOK, publicAccount(item))
}

func (s *Server) adminV1UpdateAccount(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	var body admin.UpdateAccountRequest
	decoder := json.NewDecoder(io.LimitReader(request.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeAdminError(writer, http.StatusBadRequest, "invalid_json", "Invalid account update payload")
		return
	}
	if body.Enabled == nil && body.Priority == nil && body.MaxActive == nil {
		writeAdminError(writer, http.StatusUnprocessableEntity, "empty_update", "At least one account field is required")
		return
	}
	item, err := s.admin.Update(request.Context(), request.PathValue("id"), body)
	if err != nil {
		writeAdminServiceError(writer, err, "update_failed")
		return
	}
	writeAdminOK(writer, http.StatusOK, publicAccount(item))
}

func (s *Server) adminV1BatchAccounts(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	var body admin.BatchAccountRequest
	decoder := json.NewDecoder(io.LimitReader(request.Body, 2<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeAdminError(writer, http.StatusBadRequest, "invalid_json", "Invalid account batch payload")
		return
	}
	if len(body.IDs) == 0 || len(body.IDs) > 1000 {
		writeAdminError(writer, http.StatusUnprocessableEntity, "invalid_ids", "ids must contain between 1 and 1000 accounts")
		return
	}
	result, err := s.admin.Batch(request.Context(), body)
	if err != nil {
		writeAdminServiceError(writer, err, "batch_failed")
		return
	}
	writeAdminOK(writer, http.StatusOK, result)
}

func (s *Server) adminV1AccountEvents(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	page, _ := strconv.Atoi(strings.TrimSpace(request.URL.Query().Get("page")))
	pageSize, _ := strconv.Atoi(strings.TrimSpace(request.URL.Query().Get("page_size")))
	result, err := s.admin.Events(request.Context(), request.PathValue("id"), page, pageSize)
	if err != nil {
		writeAdminServiceError(writer, err, "events_failed")
		return
	}
	items := make([]map[string]any, 0, len(result.Items))
	for _, item := range result.Items {
		items = append(items, publicAccountEvent(item))
	}
	writeAdminOK(writer, http.StatusOK, map[string]any{
		"items": items, "total": result.Total, "page": result.Page, "page_size": result.PageSize,
	})
}

func publicAccountEvent(item repository.AccountEvent) map[string]any {
	return map[string]any{
		"id": item.ID, "account_id": item.AccountID, "event_type": item.Type,
		"from_pool": item.FromPool, "to_pool": item.ToPool, "reason": item.Reason,
		"error_code": item.ErrorCode, "details": item.Details, "created_at": item.CreatedAt,
	}
}

func writeAdminServiceError(writer http.ResponseWriter, err error, fallbackCode string) {
	status := http.StatusInternalServerError
	code := fallbackCode
	switch {
	case errors.Is(err, admin.ErrAccountNotFound):
		status, code = http.StatusNotFound, "not_found"
	case errors.Is(err, admin.ErrInvalidAccountState):
		status, code = http.StatusConflict, "invalid_account_state"
	case errors.Is(err, admin.ErrInvalidBatchAction):
		status, code = http.StatusUnprocessableEntity, "invalid_batch_action"
	case strings.Contains(err.Error(), "priority"), strings.Contains(err.Error(), "max_active"):
		status, code = http.StatusUnprocessableEntity, "validation_error"
	}
	writeAdminError(writer, status, code, err.Error())
}

func (s *Server) adminV1Delete(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	id := request.PathValue("id")
	if err := s.admin.Delete(request.Context(), id); err != nil {
		status := http.StatusInternalServerError
		code := "delete_failed"
		if errors.Is(err, admin.ErrAccountNotFound) {
			status = http.StatusNotFound
			code = "not_found"
		}
		writeAdminError(writer, status, code, err.Error())
		return
	}
	writeAdminOK(writer, http.StatusOK, map[string]any{"deleted": true, "id": id})
}

func (s *Server) adminV1Recover(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	item, err := s.admin.Recover(request.Context(), request.PathValue("id"))
	if err != nil {
		status := http.StatusBadGateway
		code := "recover_failed"
		if errors.Is(err, admin.ErrAccountNotFound) {
			status = http.StatusNotFound
			code = "not_found"
		}
		writeAdminError(writer, status, code, err.Error())
		return
	}
	writeAdminOK(writer, http.StatusOK, publicAccount(item))
}

func (s *Server) adminV1RefreshCredential(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	item, err := s.admin.RefreshCredential(request.Context(), request.PathValue("id"))
	if err != nil {
		writeAdminMaintenanceError(writer, err, "refresh_failed")
		return
	}
	writeAdminOK(writer, http.StatusOK, publicAccount(item))
}

func (s *Server) adminV1RefreshQuota(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	result, err := s.admin.RefreshQuota(request.Context(), request.PathValue("id"))
	if err != nil {
		writeAdminMaintenanceError(writer, err, "quota_refresh_failed")
		return
	}
	writeAdminOK(writer, http.StatusOK, result)
}

func (s *Server) adminV1RefreshBilling(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	if _, err := s.admin.Get(request.Context(), request.PathValue("id")); err != nil {
		writeAdminServiceError(writer, err, "get_failed")
		return
	}
	writeAdminError(writer, http.StatusNotImplemented, "billing_unsupported", "Free-tier accounts do not expose a reliable billing query; use refresh-quota for observed capacity")
}

func (s *Server) adminV1ExportCredential(writer http.ResponseWriter, request *http.Request) {
	if !s.requireAdmin(writer, request) {
		return
	}
	exported, err := s.admin.ExportCredential(request.Context(), request.PathValue("id"))
	if err != nil {
		writeAdminServiceError(writer, err, "credential_export_failed")
		return
	}
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Content-Disposition", `attachment; filename="grok2api-account-`+safeDownloadName(exported.ID)+`.json"`)
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Pragma", "no-cache")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(writer).Encode(exported)
}

func writeAdminMaintenanceError(writer http.ResponseWriter, err error, fallbackCode string) {
	if errors.Is(err, admin.ErrAccountNotFound) {
		writeAdminError(writer, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if errors.Is(err, admin.ErrMaintenanceUnavailable) {
		writeAdminError(writer, http.StatusServiceUnavailable, "maintenance_unavailable", err.Error())
		return
	}
	writeAdminError(writer, http.StatusBadGateway, fallbackCode, err.Error())
}

func safeDownloadName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "account"
	}
	var result strings.Builder
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.' {
			result.WriteRune(char)
		} else {
			result.WriteByte('_')
		}
	}
	return result.String()
}

func (s *Server) adminV1ImportPreview(writer http.ResponseWriter, request *http.Request) {
	s.handleAdminImportV1(writer, request, true)
}

func (s *Server) adminV1Import(writer http.ResponseWriter, request *http.Request) {
	s.handleAdminImportV1(writer, request, false)
}

func (s *Server) handleAdminImportV1(writer http.ResponseWriter, request *http.Request, dryRun bool) {
	if !s.requireAdmin(writer, request) {
		return
	}
	var payload admin.ImportRequest
	if err := json.NewDecoder(http.MaxBytesReader(writer, request.Body, 32<<20)).Decode(&payload); err != nil {
		writeAdminError(writer, http.StatusBadRequest, "invalid_payload", "Invalid import payload")
		return
	}
	payload.DryRun = dryRun
	result, err := s.admin.Import(request.Context(), payload)
	if err != nil {
		writeAdminError(writer, http.StatusBadGateway, "import_failed", err.Error())
		return
	}
	writeAdminOK(writer, http.StatusOK, result)
}

// --- shared builders (legacy + v1) ------------------------------------------

func (s *Server) adminStatsMerged(request *http.Request) (admin.AccountStats, error) {
	summary, err := s.admin.Stats(request.Context())
	if err != nil {
		return admin.AccountStats{}, err
	}
	if live, ok := s.status.(LiveLeaseProvider); ok {
		activeByID := live.ActiveByID()
		activeTotal := 0
		for _, count := range activeByID {
			activeTotal += count
		}
		summary.ActiveLeases = activeTotal
	}
	return summary, nil
}

func (s *Server) buildAdminListPayload(request *http.Request) (map[string]any, error) {
	query := parseAdminListQuery(request)
	page, err := s.admin.ListPage(request.Context(), query)
	if err != nil {
		return nil, err
	}
	summary, err := s.adminStatsMerged(request)
	if err != nil {
		return nil, err
	}
	if live, ok := s.status.(LiveLeaseProvider); ok {
		activeByID := live.ActiveByID()
		for index := range page.Accounts {
			page.Accounts[index].Active = activeByID[page.Accounts[index].ID]
		}
	}
	public := make([]map[string]any, 0, len(page.Accounts))
	for _, item := range page.Accounts {
		public = append(public, publicAccount(item))
	}
	totalPages := 0
	if page.PageSize > 0 {
		totalPages = (page.Total + page.PageSize - 1) / page.PageSize
	}
	return map[string]any{
		"count":       page.Total,
		"total":       page.Total,
		"page":        page.Page,
		"page_size":   page.PageSize,
		"total_pages": totalPages,
		"pool":        query.Pool,
		"q":           query.Q,
		"accounts":    public,
		"summary":     summary,
	}, nil
}

func adminTokens(token string) map[string]any {
	exp := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339)
	return map[string]any{
		"accessToken":           token,
		"accessTokenExpiresAt":  exp,
		"refreshTokenExpiresAt": exp,
	}
}

func adminLoginPayload(username, token string) map[string]any {
	return map[string]any{
		"admin":  map[string]any{"id": "admin", "username": username},
		"tokens": adminTokens(token),
	}
}

func accountDTO(a map[string]any) map[string]any {
	id, _ := a["id"].(string)
	email, _ := a["email"].(string)
	userID, _ := a["user_id"].(string)
	teamID, _ := a["team_id"].(string)
	pool, _ := a["pool"].(string)
	reason, _ := a["unavailable_reason"].(string)
	lastErr, _ := a["last_error_code"].(string)
	hasRefresh, _ := a["has_refresh_token"].(bool)
	quotaActual := asInt64(a["quota_actual"])
	quotaLimit := asInt64(a["quota_limit"])
	maxActive := asInt(a["max_active"])
	if maxActive <= 0 {
		maxActive = 1
	}
	enabled := pool == "ready"
	authStatus := "active"
	if reason == "auth" {
		authStatus = "reauthRequired"
	}
	remaining := int64(0)
	if quotaLimit > quotaActual {
		remaining = quotaLimit - quotaActual
	}
	usagePercent := 0.0
	if quotaLimit > 0 {
		usagePercent = float64(quotaActual) / float64(quotaLimit) * 100
	}
	quotaType := "unknown"
	if quotaLimit > 0 {
		quotaType = "free"
	}
	name := email
	if name == "" {
		name = id
	}
	return map[string]any{
		"id":                  id,
		"provider":            "grok_build",
		"authType":            "oauth",
		"name":                name,
		"email":               emptyToNil(email),
		"userId":              emptyToNil(userID),
		"teamId":              emptyToNil(teamID),
		"enabled":             enabled,
		"authStatus":          authStatus,
		"refreshable":         hasRefresh,
		"refreshFailureCount": 0,
		"priority":            0,
		"maxConcurrent":       maxActive,
		"minimumRemaining":    0,
		"failureCount":        0,
		"lastError":           emptyToNil(lastErr),
		"createdAt":           time.Now().UTC().Format(time.RFC3339),
		"quota": map[string]any{
			"type":         quotaType,
			"source":       "unknown",
			"confidence":   "estimated",
			"status":       "active",
			"unit":         "tokens",
			"used":         quotaActual,
			"limit":        quotaLimit,
			"remaining":    remaining,
			"usagePercent": usagePercent,
			"limitKnown":   quotaLimit > 0,
			"observed":     quotaLimit > 0,
			"confirmed":    false,
		},
	}
}

func emptyToNil(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func asInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}

func asInt64(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	default:
		return 0
	}
}

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
