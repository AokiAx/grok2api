package api

import (
	"embed"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// SPA assets produced by `frontend` (Vite base `/`).
//
//go:embed all:paneldist
var panelDistFS embed.FS

// registerSPARoutes mounts the embedded admin SPA directly at the service root.
// More specific API routes registered on the same ServeMux take precedence.
func (s *Server) registerSPARoutes(mux *http.ServeMux) {
	mux.Handle("GET /", s.panelSPA())
}

func (s *Server) panelSPA() http.Handler {
	sub, err := fs.Sub(panelDistFS, "paneldist")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "panel assets missing", http.StatusServiceUnavailable)
		})
	}
	static := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		rel := strings.TrimPrefix(request.URL.Path, "/")
		if rel == "" || rel == "." {
			writePanelIndex(writer, sub, s)
			return
		}
		rel = path.Clean(rel)
		if rel == ".." || strings.HasPrefix(rel, "../") {
			http.NotFound(writer, request)
			return
		}
		if rejectsSPAFallback(rel) {
			http.NotFound(writer, request)
			return
		}

		if f, openErr := sub.Open(rel); openErr == nil {
			info, statErr := f.Stat()
			_ = f.Close()
			if statErr == nil && !info.IsDir() {
				if strings.HasPrefix(rel, "assets/") {
					writer.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				// Serve via FileServer with path relative to FS root.
				r2 := request.Clone(request.Context())
				r2.URL.Path = "/" + rel
				// Avoid FileServer directory redirect behavior by only hitting files.
				static.ServeHTTP(writer, r2)
				return
			}
		}
		if rel == "assets" || strings.HasPrefix(rel, "assets/") {
			http.NotFound(writer, request)
			return
		}

		writePanelIndex(writer, sub, s)
	})
}

func rejectsSPAFallback(rel string) bool {
	for _, prefix := range []string{
		"panel", "manager",
		"api", "admin", "v1", "chat",
		"health", "healthz", "readyz",
	} {
		if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
			return true
		}
	}
	return false
}

func writePanelIndex(writer http.ResponseWriter, sub fs.FS, s *Server) {
	index, err := sub.Open("index.html")
	if err != nil {
		http.Error(writer, "panel assets missing", http.StatusServiceUnavailable)
		return
	}
	defer index.Close()
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusOK)
	_, _ = io.Copy(writer, index)
}

func panelDistAvailable() bool {
	sub, err := fs.Sub(panelDistFS, "paneldist")
	if err != nil {
		return false
	}
	f, err := sub.Open("index.html")
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}
