package buildtest_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, current, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(current), "..", ".."))
}

func readFile(t *testing.T, relative string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), relative))
	if err != nil {
		t.Fatalf("read %s: %v", relative, err)
	}
	return string(data)
}

func requireContains(t *testing.T, content string, values ...string) {
	t.Helper()
	for _, value := range values {
		if !strings.Contains(content, value) {
			t.Errorf("content missing %q", value)
		}
	}
}

func requireNotContains(t *testing.T, content string, values ...string) {
	t.Helper()
	for _, value := range values {
		if strings.Contains(content, value) {
			t.Errorf("content unexpectedly contains %q", value)
		}
	}
}

func publishedDockerfile(t *testing.T) string {
	t.Helper()
	workflow := readFile(t, ".github/workflows/image.yml")
	for _, line := range strings.Split(workflow, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "file:") {
			path := strings.TrimSpace(strings.TrimPrefix(line, "file:"))
			if path != "" {
				return path
			}
		}
	}
	return "Dockerfile"
}

func TestPublishedDockerfileBuildsStaticNonRootGoImage(t *testing.T) {
	dockerfile := readFile(t, publishedDockerfile(t))
	requireContains(
		t,
		dockerfile,
		"golang:1.25",
		"CGO_ENABLED=0",
		"go build",
		"gcr.io/distroless/static-debian12:nonroot",
		"USER nonroot:nonroot",
	)
	if strings.Contains(dockerfile, "COPY data") || strings.Contains(dockerfile, "COPY config.json") {
		t.Fatal("runtime secrets must not be copied into image")
	}
}

func TestImageWorkflowPublishesSignedMultiArchitectureGHCRImage(t *testing.T) {
	workflow := readFile(t, ".github/workflows/image.yml")
	requireContains(
		t,
		workflow,
		"ghcr.io/aokiax/grok2api",
		"linux/amd64,linux/arm64",
		"docker/build-push-action",
		"actions/attest-build-provenance",
		"cosign sign",
		"aquasecurity/trivy-action@v0.36.0",
		"packages: write",
		"id-token: write",
	)
}

func TestCISeparatesBackendFrontendAndImageSmoke(t *testing.T) {
	workflow := readFile(t, ".github/workflows/ci.yml")
	requireContains(
		t,
		workflow,
		"backend:",
		"frontend:",
		"image-smoke:",
		"go test -race",
		"go vet ./...",
		"coverprofile",
		"go build ./cmd/grok2api",
		"npm ci",
		"npm run build",
		"docker build --tag grok2api:ci .",
		"bash scripts/smoke-docker-image.sh grok2api:ci",
	)
	requireNotContains(t, workflow, "paneldist", "refresh embed")
}

func TestImageWorkflowPublishesOnlyFromTheAuthoritativeDockerfile(t *testing.T) {
	workflow := readFile(t, ".github/workflows/image.yml")
	requireContains(t, workflow, "linux/amd64,linux/arm64", "context: .")
	requireNotContains(t, workflow, "pull_request:", "Dockerfile.golang", "file: Dockerfile.golang")
}

func TestDockerignoreExcludesRuntimeSecrets(t *testing.T) {
	ignore := readFile(t, ".dockerignore")
	requireContains(t, ignore, "data/", "config.json", ".env", ".git/")
}

func TestDeployWorkflowIsManualAndPinsDigest(t *testing.T) {
	workflow := readFile(t, ".github/workflows/deploy.yml")
	requireContains(
		t,
		workflow,
		"workflow_dispatch",
		"environment: production",
		"docker inspect",
		"RepoDigests",
		"8788",
		"Verify canary health and frontend asset",
		"Verify promoted service",
		"/assets/",
		"Rollback to previous Go image",
		"docker start grok2api-cli",
	)
}

func TestLocalDeployVerifiesHealthAndFrontendAsset(t *testing.T) {
	script := readFile(t, "deploy/deploy-stack.sh")
	requireContains(t, script, "/health", "/assets/", "index.html", "frontend asset")
}

func TestDeliveryExamplesUseRuntimeFrontendDirectory(t *testing.T) {
	envExample := readFile(t, ".env.example")
	configExample := readFile(t, "config.example.json")
	readme := readFile(t, "README.md")
	frontendReadme := readFile(t, "frontend/README.md")

	requireContains(t, envExample, "GROK2API_FRONTEND_STATIC_PATH=/app/frontend/dist")
	requireContains(t, configExample, `"frontend"`, `"static_path"`)
	requireContains(
		t,
		readme,
		"Docker 镜像是唯一正式交付物",
		"GROK2API_FRONTEND_STATIC_PATH",
		"/app/frontend/dist",
		"裸 Go",
	)
	requireContains(t, frontendReadme, "npm ci", "npm run build", "Docker")
	requireNotContains(t, readme, "Dockerfile.golang", "sync-paneldist", "go build -trimpath -o grok2api.exe")
	requireNotContains(t, frontendReadme, "embed", "sync-paneldist")
}

func TestDockerImageSmokeScriptCoversRuntimeDeliveryContract(t *testing.T) {
	script := readFile(t, "scripts/smoke-docker-image.sh")
	requireContains(
		t,
		script,
		"docker image inspect",
		".Config.User",
		"docker run",
		"/app/config.json:ro",
		"/app/data",
		"/health",
		"/assets/",
		"grok2api.db",
	)
}
