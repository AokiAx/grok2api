#!/usr/bin/env bash
# Deploy / refresh grok2api stack (app + warp + privoxy + flaresolverr)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

if [[ ! -f config.json ]]; then
  echo "missing config.json in $ROOT" >&2
  exit 1
fi

if [[ ! -f .env ]]; then
  if [[ -f deploy/.env.example ]]; then
    cp deploy/.env.example .env
    echo "created .env from deploy/.env.example"
  fi
fi

mkdir -p data deploy
if [[ ! -f deploy/privoxy-warp.conf ]]; then
  echo "missing deploy/privoxy-warp.conf" >&2
  exit 1
fi

echo "[deploy] pull images"
docker compose pull

echo "[deploy] up stack"
docker compose up -d --remove-orphans

verify_service() {
  local base_url="$1"
  local index_html asset_path

  curl -fsS "$base_url/health" >/dev/null || return 1
  index_html="$(curl -fsS "$base_url/")" || return 1
  grep -q '<div id="root"></div>' <<<"$index_html" || return 1
  asset_path="$(grep -oE '(src|href)="/assets/[^"]+"' <<<"$index_html" | head -n 1 | cut -d '"' -f 2)"
  [[ -n "$asset_path" ]] || return 1
  curl -fsS "$base_url$asset_path" >/dev/null || return 1
  curl -fsS "$base_url/accounts" | grep -q '<div id="root"></div>' || return 1
}

base_url="http://127.0.0.1:${GROK2API_PORT:-8787}"
echo "[deploy] wait for health and frontend asset"
for i in $(seq 1 40); do
  if verify_service "$base_url"; then
    curl -sS "$base_url/health"
    echo
    docker compose ps
    exit 0
  fi
  sleep 2
done

echo "health check failed" >&2
docker compose ps
docker compose logs --tail 100 app || true
exit 1
