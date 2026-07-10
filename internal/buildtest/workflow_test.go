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

func TestDockerfileBuildsStaticNonRootGoImage(t *testing.T) {
	dockerfile := readFile(t, "Dockerfile.golang")
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

func TestCIRequiresRaceVetAndCoverage(t *testing.T) {
	workflow := readFile(t, ".github/workflows/ci.yml")
	requireContains(
		t,
		workflow,
		"go test -race",
		"go vet ./...",
		"coverprofile",
		"go build ./cmd/grok2api",
	)
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
		"Verify promoted service",
		"Rollback to previous Go image",
		"docker start grok2api-cli",
	)
}
