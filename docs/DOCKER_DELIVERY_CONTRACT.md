# Docker-only delivery contract

## Decision

The supported product artifact is the Docker image. A bare Go binary is useful
for backend development and tests, but it is not required to contain or ship the
admin frontend.

The image must deliver these runtime capabilities together:

- the `grok2api` server;
- the built admin SPA and its hashed assets;
- a writable `/app/data` mount for SQLite;
- a read-only `/app/config.json` mount;
- a declared non-root runtime user.

The exact frontend packaging mechanism may change from embedded assets to a
runtime directory. The observable HTTP and container contracts above must not.

## Protected release invariants

`internal/buildtest/workflow_test.go` protects invariants that must survive the
directory and packaging migration:

- the Dockerfile selected by the image workflow builds a static Go binary and
  uses the distroless non-root runtime;
- runtime secrets and data are not copied into the image;
- GHCR publishing remains multi-architecture, signed, provenance-aware, and
  vulnerability-scanned;
- CI continues to run race tests, vet, coverage, and a Go build;
- production deployment remains manual, resolves the requested tag to a digest,
  verifies the canary/promoted service, and retains rollback behavior.

Run the repository contract tests with:

```bash
go test ./internal/buildtest
```

## Image smoke contract

After building or pulling an image, run:

```bash
bash scripts/smoke-docker-image.sh grok2api:local
```

An optional second argument selects the host port. The script verifies:

1. the image declares a non-root user;
2. the container can create `/app/data/grok2api.db` through a bind mount;
3. `/health` responds with the expected service envelope;
4. `/` returns the SPA shell;
5. a real `/assets/...` URL referenced by `index.html` is downloadable;
6. a deep SPA route falls back to the same shell.

The script is intentionally not wired into CI in phase 0. Phase 1 should invoke
it against the locally built migration image before publishing.

## Docker build-context risk

The root `.dockerignore` excludes Git metadata, runtime secrets, data, coverage,
and local binaries, but it does **not** currently exclude either of these paths:

```text
frontend/node_modules/
frontend/dist/
```

`frontend/.gitignore` does not affect the Docker build context. If a developer
has installed frontend dependencies locally, the complete host
`frontend/node_modules` tree is sent to the Docker daemon by `docker build .`.
That increases transfer time and can introduce host-platform artifacts into the
frontend build stage before `npm install` runs.

Phase 1 must add explicit root `.dockerignore` entries for both paths. This is a
documented deferred RED item rather than a phase-0 test assertion, because such
an assertion would knowingly break the current main CI before the packaging
change is implemented.

## Phase 1 acceptance / deferred RED list

The Docker packaging migration is complete only when all of these are true:

- `.dockerignore` excludes `frontend/node_modules/` and `frontend/dist/`;
- one authoritative Dockerfile builds frontend and backend stages;
- the final image contains the frontend under `/app/frontend/dist`;
- the container config points the server at `/app/frontend/dist`;
- the Go server no longer depends on `internal/api/paneldist` or `go:embed`;
- generated frontend files are not committed to the Go package;
- CI builds the image and runs `scripts/smoke-docker-image.sh` before publish;
- the existing non-root, signing, scanning, multi-architecture, digest promotion,
  health verification, and rollback contracts remain green.

These checks should be enabled one by one in phase 1 as their implementation
lands; they are not skipped assertions hidden in the phase-0 test suite.
