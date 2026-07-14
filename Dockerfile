FROM node:22-bookworm AS frontend
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json* ./
RUN npm install
COPY frontend/ ./
RUN npm run build

FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
# Refresh embedded SPA from the frontend build stage.
RUN rm -rf internal/api/paneldist && mkdir -p internal/api/paneldist
COPY --from=frontend /src/frontend/dist/ ./internal/api/paneldist/
RUN find internal/api/paneldist -name "*.map" -delete
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/grok2api \
    ./cmd/grok2api

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app
COPY --from=build /out/grok2api /grok2api
USER nonroot:nonroot
EXPOSE 8787
ENTRYPOINT ["/grok2api"]
CMD ["serve", "--config", "/app/config.json"]
