package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	auth "github.com/AokiAx/grok2api/backend/internal/adminauth"
)

type AdminAuthHandlerOptions struct{ SecureCookies bool }

func NewAdminAuthHandler(service *auth.Service, opts AdminAuthHandlerOptions) http.Handler {
	mux := http.NewServeMux()
	h := &adminAuthHandler{service: service, opts: opts}
	mux.HandleFunc("POST /api/admin/v1/auth/login", h.login)
	mux.HandleFunc("POST /api/admin/v1/auth/refresh", h.refresh)
	mux.HandleFunc("POST /api/admin/v1/auth/logout", h.logout)
	mux.HandleFunc("GET /api/admin/v1/auth/me", h.me)
	return mux
}

type adminAuthHandler struct {
	service *auth.Service
	opts    AdminAuthHandlerOptions
}
type authRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Remember bool   `json:"remember"`
}

func (h *adminAuthHandler) login(w http.ResponseWriter, r *http.Request) {
	var in authRequest
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		h.error(w, http.StatusBadRequest, "invalid_request", "")
		return
	}
	out, err := h.service.Login(r.Context(), auth.LoginInput{Username: in.Username, Password: in.Password, SourceIP: requestIP(r), UserAgent: r.UserAgent(), Remember: in.Remember})
	if err != nil {
		h.writeErr(w, err)
		return
	}
	h.setCookie(w, out.RefreshCookieValue, out.Remember, out.RefreshExpiresAt)
	h.ok(w, authDTO(out))
}
func (h *adminAuthHandler) refresh(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie("grok2api_admin_refresh")
	if err != nil {
		h.error(w, http.StatusUnauthorized, "unauthorized", "")
		return
	}
	out, err := h.service.Refresh(r.Context(), c.Value, requestIP(r), r.UserAgent())
	if err != nil {
		h.writeErr(w, err)
		return
	}
	h.setCookie(w, out.RefreshCookieValue, out.Remember, out.RefreshExpiresAt)
	h.ok(w, authDTO(out))
}
func (h *adminAuthHandler) logout(w http.ResponseWriter, r *http.Request) {
	if c, e := r.Cookie("grok2api_admin_refresh"); e == nil {
		_ = h.service.Logout(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: "grok2api_admin_refresh", Value: "", Path: "/api/admin/v1/auth", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: h.opts.SecureCookies})
	h.ok(w, map[string]bool{"logged_out": true})
}
func (h *adminAuthHandler) me(w http.ResponseWriter, r *http.Request) {
	parts := strings.Fields(r.Header.Get("Authorization"))
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		h.error(w, http.StatusUnauthorized, "unauthorized", "")
		return
	}
	u, _, err := h.service.AuthenticateAccess(r.Context(), parts[1])
	if err != nil {
		h.writeErr(w, err)
		return
	}
	h.ok(w, map[string]any{"id": u.ID, "username": u.Username, "role": u.Role})
}
func (h *adminAuthHandler) setCookie(w http.ResponseWriter, v string, remember bool, exp time.Time) {
	max := 0
	if remember {
		max = int(time.Until(exp).Seconds())
	}
	http.SetCookie(w, &http.Cookie{Name: "grok2api_admin_refresh", Value: v, Path: "/api/admin/v1/auth", MaxAge: max, HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: h.opts.SecureCookies, Expires: exp})
}
func (h *adminAuthHandler) ok(w http.ResponseWriter, data any) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "data": data})
}
func authDTO(out auth.LoginOutput) map[string]any {
	return map[string]any{"admin": map[string]any{"id": out.Admin.ID, "username": out.Admin.Username, "role": out.Admin.Role}, "tokens": map[string]any{"accessToken": out.AccessToken, "accessTokenExpiresAt": out.AccessExpiresAt, "refreshTokenExpiresAt": out.RefreshExpiresAt}}
}
func (h *adminAuthHandler) error(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Cache-Control", "no-store")
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="admin"`)
	}
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "900")
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": map[string]string{"code": code, "message": msg}})
}
func (h *adminAuthHandler) writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrSetupRequired):
		h.error(w, http.StatusServiceUnavailable, "setup_required", "")
	case errors.Is(err, auth.ErrInvalidCredentials):
		h.error(w, http.StatusUnauthorized, "invalid_credentials", "")
	case errors.Is(err, auth.ErrRateLimited):
		h.error(w, http.StatusTooManyRequests, "rate_limited", "")
	case errors.Is(err, auth.ErrConflict):
		h.error(w, http.StatusConflict, "session_conflict", "")
	default:
		h.error(w, http.StatusUnauthorized, "unauthorized", "")
	}
}
func requestIP(r *http.Request) string {
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i > 0 {
		return strings.Trim(host[:i], "[]")
	}
	return host
}
