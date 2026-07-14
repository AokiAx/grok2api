#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 IMAGE [HOST_PORT]" >&2
  echo "example: $0 grok2api:local 18787" >&2
}

image="${1:-}"
port="${2:-${SMOKE_PORT:-18787}}"
if [[ -z "$image" ]]; then
  usage
  exit 2
fi
if [[ ! "$port" =~ ^[0-9]+$ ]] || ((port < 1 || port > 65535)); then
  echo "invalid host port: $port" >&2
  exit 2
fi

for command in docker curl mktemp; do
  if ! command -v "$command" >/dev/null 2>&1; then
    echo "required command not found: $command" >&2
    exit 2
  fi
done

configured_user="$(docker image inspect --format '{{.Config.User}}' "$image")"
case "${configured_user,,}" in
  ""|0|0:0|root|root:root)
    echo "image must declare a non-root runtime user; got '${configured_user:-<empty>}'" >&2
    exit 1
    ;;
esac

workdir="$(mktemp -d)"
container="grok2api-smoke-$$"
cleanup() {
  docker rm -f "$container" >/dev/null 2>&1 || true
  rm -rf "$workdir"
}
trap cleanup EXIT

mkdir -p "$workdir/data"
chmod 0777 "$workdir/data"
cat >"$workdir/config.json" <<'JSON'
{
  "host": "0.0.0.0",
  "port": 8787,
  "data_dir": "/app/data"
}
JSON

docker run --detach --name "$container" \
  --publish "127.0.0.1:${port}:8787" \
  --env GROK2API_HOST=0.0.0.0 \
  --env GROK2API_DATA_DIR=/app/data \
  --volume "$workdir/config.json:/app/config.json:ro" \
  --volume "$workdir/data:/app/data" \
  "$image" >/dev/null

base_url="http://127.0.0.1:${port}"
health=""
for ((attempt = 1; attempt <= 30; attempt++)); do
  if health="$(curl --fail --silent --show-error "$base_url/health" 2>/dev/null)"; then
    break
  fi
  sleep 1
done
if [[ -z "$health" ]]; then
  echo "health endpoint did not become ready" >&2
  docker logs "$container" >&2 || true
  exit 1
fi
if [[ "$health" != *'"version"'* || "$health" != *'"account_pool"'* ]]; then
  echo "unexpected health payload: $health" >&2
  exit 1
fi

index_html="$(curl --fail --silent --show-error "$base_url/")"
if [[ "$index_html" != *'<div id="root"></div>'* ]]; then
  echo "root URL did not return the admin SPA shell" >&2
  exit 1
fi

asset_path=""
if [[ "$index_html" =~ (src|href)=\"(/assets/[^\"]+)\" ]]; then
  asset_path="${BASH_REMATCH[2]}"
fi
if [[ -z "$asset_path" ]]; then
  echo "admin SPA did not reference a hashed /assets/ file" >&2
  exit 1
fi
curl --fail --silent --show-error "$base_url$asset_path" --output "$workdir/asset"
if [[ ! -s "$workdir/asset" ]]; then
  echo "referenced SPA asset is empty: $asset_path" >&2
  exit 1
fi

deep_route_html="$(curl --fail --silent --show-error "$base_url/accounts")"
if [[ "$deep_route_html" != *'<div id="root"></div>'* ]]; then
  echo "SPA deep-route fallback failed" >&2
  exit 1
fi

if [[ ! -s "$workdir/data/grok2api.db" ]]; then
  echo "container did not create writable /app/data/grok2api.db" >&2
  exit 1
fi

echo "Docker image smoke passed: image=$image user=$configured_user asset=$asset_path"
