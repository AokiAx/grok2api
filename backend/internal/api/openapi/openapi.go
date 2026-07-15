package openapi

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"sync"
)

//go:embed openapi.yaml
var rawYAML []byte

//go:embed openapi.json
var rawJSON []byte

//go:embed paths.json
var rawPaths []byte

var (
	once    sync.Once
	pathSet map[string]map[string]struct{}
)

// DocumentJSON returns the OpenAPI document as JSON bytes.
func DocumentJSON() []byte {
	return append([]byte(nil), rawJSON...)
}

// RawYAML returns the embedded OpenAPI YAML source.
func RawYAML() []byte {
	return append([]byte(nil), rawYAML...)
}

// Paths returns method sets keyed by path templates from the contract.
func Paths() map[string]map[string]struct{} {
	once.Do(func() {
		var decoded map[string][]string
		if err := json.Unmarshal(rawPaths, &decoded); err != nil {
			pathSet = map[string]map[string]struct{}{}
			return
		}
		pathSet = make(map[string]map[string]struct{}, len(decoded))
		for path, methods := range decoded {
			set := make(map[string]struct{}, len(methods))
			for _, method := range methods {
				set[method] = struct{}{}
			}
			pathSet[path] = set
		}
	})
	return pathSet
}

// RequiredContractPaths is the minimal route set tests assert against the live mux.
func RequiredContractPaths() map[string][]string {
	return map[string][]string{
		"/health":                   {"GET"},
		"/healthz":                  {"GET"},
		"/readyz":                   {"GET"},
		"/v1/models":                {"GET"},
		"/v1/chat/completions":      {"POST"},
		"/v1/responses":             {"POST"},
		"/v1/messages":              {"POST"},
		"/openapi.json":             {"GET"},
		"/docs":                     {"GET"},
		"/api/admin/v1/system/meta": {"GET"},
		"/api/admin/v1/dashboard":   {"GET"},
		"/api/admin/v1/accounts":    {"GET"},
		"/api/admin/v1/settings":    {"GET", "PUT"},
		"/api/admin/v1/models":      {"GET"},
		"/api/admin/v1/client-keys": {"GET", "POST"},
	}
}

// Mount registers /openapi.json, /openapi.yaml and /docs on mux.
func Mount(mux *http.ServeMux) {
	if mux == nil {
		return
	}
	mux.HandleFunc("GET /openapi.json", serveJSON)
	mux.HandleFunc("GET /openapi.yaml", serveYAML)
	mux.HandleFunc("GET /docs", serveDocs)
	mux.HandleFunc("GET /docs/", serveDocs)
}

func serveJSON(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("Cache-Control", "public, max-age=60")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(rawJSON)
}

func serveYAML(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	writer.Header().Set("Cache-Control", "public, max-age=60")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(rawYAML)
}

func serveDocs(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write([]byte(docsHTML))
}

// Lightweight Swagger UI shell (CDN). Spec is same-origin /openapi.json.
const docsHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>Grok2API Docs</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5.17.14/swagger-ui.css" />
  <style>
    body { margin: 0; background: #0b0f14; }
    .topbar { display: none; }
  </style>
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5.17.14/swagger-ui-bundle.js"></script>
  <script>
    window.ui = SwaggerUIBundle({
      url: '/openapi.json',
      dom_id: '#swagger-ui',
      deepLinking: true,
      presets: [SwaggerUIBundle.presets.apis],
      layout: 'BaseLayout'
    });
  </script>
</body>
</html>
`
