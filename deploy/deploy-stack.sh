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

echo "[deploy] wait health"
for i in $(seq 1 40); do
  if curl -fsS "http://127.0.0.1:${GROK2API_PORT:-8787}/health" >/dev/null; then
    curl -sS "http://127.0.0.1:${GROK2API_PORT:-8787}/health"
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
