#!/usr/bin/env bash
set -euo pipefail
root="${DEVENV_ROOT:-$(cd "$(dirname "$0")/.." && pwd)}"
cd "$root/extensions/pig-litter"
bun install --frozen-lockfile
bun run test
bun run build
