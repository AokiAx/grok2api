#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT/frontend"
npm run build
rm -rf "$ROOT/internal/api/paneldist"
mkdir -p "$ROOT/internal/api/paneldist"
cp -r dist/. "$ROOT/internal/api/paneldist/"
find "$ROOT/internal/api/paneldist" -name "*.map" -delete
echo "synced frontend/dist -> internal/api/paneldist"
