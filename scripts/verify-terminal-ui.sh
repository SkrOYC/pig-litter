#!/usr/bin/env bash
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
module="$root/extensions/pig-litter"
go -C "$root/scripts" build -mod=readonly -o "$module/dist/pig-litter" ./cmd/pig-litter
go -C "$root/scripts" build -mod=readonly -o "$module/dist/verify-tui" ./cmd/verify-tui
go -C "$root/scripts" build -mod=readonly -o "$module/dist/tui-idle-observer" ./cmd/tui-idle-observer
observer_args=()
case_name= previous=
for argument in "$@"; do
  if [[ "$previous" == --case ]]; then
    case_name="$argument"
  elif [[ "$argument" == --case=* ]]; then
    case_name="${argument#--case=}"
  fi
  previous="$argument"
done
if [[ "$case_name" != unselected ]]; then
  observer_args=(--idle-observer "$module/dist/tui-idle-observer")
fi
exec "$module/dist/verify-tui" \
  "${observer_args[@]}" \
  --pig "$(command -v pig)" \
  --extension "$module/dist/pig-litter" \
  --piglet "$root/piglet.yaml" \
  --evidence-root "$root/.pstack/evidence/go-tui" "$@"
