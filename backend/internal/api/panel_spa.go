package api

import (
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// registerSPARoutes mounts the configured admin SPA directly at the service root.
// More specific API routes registered on the same ServeMux take precedence.
func (s *Server) registerSPARoutes(mux *http.ServeMux) {
	mux.Handle("GET /", s.panelSPA())
}

func (s *Server) panelSPA() http.Handler {
	static := http.FileServer(http.FS(s.frontend))
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		rel := strings.TrimPrefix(request.URL.Path, "/")
		if rel == "" || rel == "." {
			writePanelIndex(writer, s.frontend)
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

		if f, openErr := s.frontend.Open(rel); openErr == nil {
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

		writePanelIndex(writer, s.frontend)
	})
}

func rejectsSPAFallback(rel string) bool {
	for _, prefix := range []string{
		"panel", "manager",
		"api", "admin", "v1", "chat",
		"health", "healthz", "readyz",
		"openapi.json", "openapi.yaml", "docs",
	} {
		if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
			return true
		}
	}
	return false
}

func writePanelIndex(writer http.ResponseWriter, frontend fs.FS) {
	index, err := frontend.Open("index.html")
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
