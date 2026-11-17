#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
test_root="$(mktemp -d /tmp/pig-litter-doctor-test.XXXXXXXX)"
trap 'rm -rf -- "$test_root"' EXIT
fixture="$test_root/repo"
bin="$test_root/bin"
mkdir -p "$bin" "$fixture/.agents/skills/verify-pig-litter/scripts" "$fixture/scripts" "$fixture/extensions/pig-litter"
cp "$script_dir/verify.sh" "$fixture/.agents/skills/verify-pig-litter/scripts/verify.sh"
touch "$fixture/extensions/pig-litter/adapter.go" "$fixture/piglet.yaml" "$fixture/scripts/verify-terminal-ui.sh"
ln -s "$(command -v bash)" "$bin/bash"
ln -s "$(command -v dirname)" "$bin/dirname"
ln -s "$(command -v jq)" "$bin/jq"
export REAL_GO="$(command -v go)"
export GOTOOLCHAIN=local GOENV=off

cat > "$bin/pig" <<'PIG'
#!/usr/bin/env bash
[[ "$*" == '--version' ]]
printf '%s\n' "$TEST_PIG_VERSION"
PIG
cat > "$bin/tmux" <<'TMUX'
#!/usr/bin/env bash
[[ "$*" == '-V' ]]
printf '%s\n' "$TEST_TMUX_VERSION"
TMUX
cat > "$bin/go" <<'GO'
#!/usr/bin/env bash
set -euo pipefail
module_dir=
if [[ "${1:-}" == '-C' ]]; then
  module_dir="$2"
  shift 2
fi
if [[ "$*" == 'env GOVERSION' ]]; then
  printf '%s\n' "$TEST_GO_VERSION"
else
  if [[ -n "$module_dir" ]]; then
    set -- "$@" "$module_dir/go.mod"
  fi
  exec "$REAL_GO" "$@"
fi
GO
chmod +x "$bin/pig" "$bin/go" "$bin/tmux"

write_mod() {
  cat > "$fixture/scripts/go.mod" <<MOD
module example.com/fixture

go 1.26.0

toolchain $2

require github.com/MichaelKinsy/PiG v$1
MOD
}

doctor() {
  PATH="$bin" "$fixture/.agents/skills/verify-pig-litter/scripts/verify.sh" doctor 2>&1
}

expect_success() {
  local output
  if ! output="$(doctor)"; then
    printf 'expected doctor success, got: %s\n' "$output" >&2
    exit 1
  fi
  for value in "$TEST_PIG_VERSION" "${TEST_GO_VERSION#go}" "$TEST_TMUX_VERSION"; do
    [[ "$output" == *"$value"* ]] || { printf 'doctor omitted %s: %s\n' "$value" "$output" >&2; exit 1; }
  done
  printf 'pass: %s, %s, %s\n' "$TEST_PIG_VERSION" "$TEST_GO_VERSION" "$TEST_TMUX_VERSION"
}

expect_failure() {
  local expected="$1" output
  if output="$(doctor)"; then
    printf 'expected doctor failure, got: %s\n' "$output" >&2
    exit 1
  fi
  [[ "$output" == *"$expected"* ]] || { printf 'expected failure containing %s, got: %s\n' "$expected" "$output" >&2; exit 1; }
  printf 'pass: rejected %s\n' "$expected"
}

export TEST_PIG_VERSION='0.4.1+1.0.3' TEST_GO_VERSION='go1.27.1' TEST_TMUX_VERSION='tmux 3.7c'
write_mod '0.4.1' 'go1.27.1'
expect_success

export TEST_PIG_VERSION='0.7.0+1.5.0' TEST_GO_VERSION='go1.27.4' TEST_TMUX_VERSION='tmux 3.9a'
write_mod '0.7.0' 'go1.27.4'
expect_success

export TEST_PIG_VERSION='0.7.01+1.5.0'
expect_failure 'expected PiG 0.7.0'
export TEST_PIG_VERSION='0.7.0+1.5.0' TEST_GO_VERSION='go1.27.1'
expect_failure 'expected Go go1.27.4'
export TEST_GO_VERSION='go1.27.4'
mv "$bin/tmux" "$test_root/tmux"
expect_failure 'missing required command: tmux'
mv "$test_root/tmux" "$bin/tmux"
rm "$fixture/piglet.yaml"
expect_failure 'required file is missing: piglet.yaml'
