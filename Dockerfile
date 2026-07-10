FROM golang:1.25-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

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
