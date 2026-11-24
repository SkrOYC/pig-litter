#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
factory="$root/extensions/pig-litter"
tools="$root/scripts"
for module in "$factory" "$tools"; do
  go -C "$module" test -mod=readonly ./...
  go -C "$module" test -mod=readonly -race ./...
  go -C "$module" vet ./...
done
bash "$root/scripts/build-pig-litter.sh"
for command in verify-lifecycle verify-tui litter-fixture-guard tui-idle-observer; do
  go -C "$tools" build -mod=readonly -o "$factory/dist/$command" "./cmd/$command"
done
