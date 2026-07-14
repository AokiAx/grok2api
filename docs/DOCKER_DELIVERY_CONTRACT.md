# Docker-only delivery contract

## Decision

The supported product artifact is the Docker image. A bare Go binary is useful
for backend development and tests, but it is not required to contain or ship the
admin frontend.

The image must deliver these runtime capabilities together:

- the `Grok2API` server;
- the built admin SPA and its hashed assets;
- a writable `/app/data` mount for SQLite;
- a read-only `/app/config.json` mount;
- a declared non-root runtime user.

The frontend is built in the authoritative root `Dockerfile`, copied to
`/app/frontend/dist`, and selected with
`GROK2API_FRONTEND_STATIC_PATH=/app/frontend/dist`. A bare Go process leaves
`frontend.static_path` empty by default and therefore serves API routes only.

## Protected release invariants

`backend/internal/buildtest/workflow_test.go` protects invariants that must survive the
directory and packaging migration:

- the Dockerfile selected by the image workflow builds a static Go binary and
  uses the distroless non-root runtime;
- runtime secrets and data are not copied into the image;
- GHCR publishing remains multi-architecture, signed, provenance-aware, and
  vulnerability-scanned;
- CI independently validates backend, frontend, and a single-architecture image
  smoke build;
- production deployment remains manual, resolves the requested tag to a digest,
  verifies the canary/promoted service, and retains rollback behavior.

Run the repository contract tests with:

```bash
go -C backend test ./internal/buildtest
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

CI invokes it against a locally built single-architecture image. The publish
workflow is reserved for `main`, version tags, and manual runs, where it builds
the signed multi-architecture image once.

## Docker build context

The root `.dockerignore` excludes Git metadata, runtime secrets, data, coverage,
local binaries, and local frontend output:

```text
frontend/node_modules/
frontend/dist/
```

`frontend/.gitignore` does not affect the Docker build context, so these root
rules are part of the delivery contract.

## Delivery acceptance

The Docker packaging migration is complete only when all of these are true:

- `.dockerignore` excludes `frontend/node_modules/` and `frontend/dist/`;
- one authoritative Dockerfile builds frontend and backend stages;
- the final image contains the frontend under `/app/frontend/dist`;
- the container config points the server at `/app/frontend/dist`;
- the Go server no longer depends on `backend/internal/api/paneldist` or `go:embed`;
- generated frontend files are not committed to the Go package;
- CI builds the image and runs `scripts/smoke-docker-image.sh` before publish;
- the existing non-root, signing, scanning, multi-architecture, digest promotion,
  health verification, and rollback contracts remain green.

These checks are locked by `backend/internal/buildtest` and the image smoke script.
