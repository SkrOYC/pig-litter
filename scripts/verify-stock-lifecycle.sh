#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
module="$root/extensions/pig-litter"
go -C "$root/scripts" build -mod=readonly -o "$module/dist/pig-litter" ./cmd/pig-litter
go -C "$root/scripts" build -mod=readonly -o "$module/dist/verify-lifecycle" ./cmd/verify-lifecycle
go -C "$root/scripts" build -mod=readonly -o "$module/dist/litter-fixture-guard" ./cmd/litter-fixture-guard
exec "$module/dist/verify-lifecycle" "$@"
