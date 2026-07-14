# syntax=docker/dockerfile:1.7

ARG BUILDPLATFORM=linux/amd64

FROM --platform=$BUILDPLATFORM node:22-bookworm AS frontend
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm \
    npm ci --no-audit --no-fund
COPY frontend/ ./
RUN npm run build
RUN find dist -name "*.map" -delete

FROM --platform=$BUILDPLATFORM golang:1.25-bookworm AS build
ARG TARGETOS=linux
ARG TARGETARCH=amd64
WORKDIR /src/backend
COPY backend/go.mod backend/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY backend/cmd ./cmd
COPY backend/internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/grok2api \
    ./cmd/grok2api

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/grok2api /app/grok2api
COPY --from=frontend /src/frontend/dist/ /app/frontend/dist/
ENV GROK2API_FRONTEND_STATIC_PATH=/app/frontend/dist
USER nonroot:nonroot
EXPOSE 8787
ENTRYPOINT ["/app/grok2api"]
CMD ["serve", "--config", "/app/config.json"]
