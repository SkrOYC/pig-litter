#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
module="$root/extensions/pig-litter"
go -C "$root/scripts" build -mod=readonly -o "$module/dist/pig-litter" ./cmd/pig-litter
go -C "$root/scripts" build -mod=readonly -o "$module/dist/verify-tui" ./cmd/verify-tui
exec "$module/dist/verify-tui" \
  --pig "$(command -v pig)" \
  --extension "$module/dist/pig-litter" \
  --piglet "$root/piglet.yaml" \
  --evidence-root "$root/.pstack/evidence/go-tui" "$@"
