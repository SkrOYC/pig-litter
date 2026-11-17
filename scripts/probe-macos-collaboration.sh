#!/bin/bash
set -u

artifact_dir="${RUNNER_TEMP:?}/collaboration-platform-probe"
mkdir -p "$artifact_dir"
exec > >(tee -a "$artifact_dir/command.log") 2>&1
set -x

status=blocked
reason="Probe did not finish."
host_os="$(uname -s)"
architecture="$(uname -m)"
macos_version="$(sw_vers -productVersion 2>&1)"
commit_sha="$(git rev-parse HEAD)"
iterm_version=""
iterm_executable=""
session_id=""
session_tty=""
window_id=""
screenshot=""
expected="PIG_SENTINEL_${GITHUB_RUN_ID}_${GITHUB_RUN_ATTEMPT}_${architecture}"
app_path=/Applications/iTerm.app

uname -a
sw_vers
sysctl -n machdep.cpu.brand_string
id
who

if [ "$host_os" != Darwin ]; then
  status=failed
  reason="The runner is not macOS."
elif { [ "$PROBE_RUNNER_LABEL" = macos-15-intel ] && [ "$architecture" != x86_64 ]; } || { [ "$PROBE_RUNNER_LABEL" = macos-15 ] && [ "$architecture" != arm64 ]; }; then
  status=failed
  reason="The runner architecture does not match the selected label."
else
  if [ ! -d "$app_path" ]; then
    HOMEBREW_NO_AUTO_UPDATE=1 HOMEBREW_NO_INSTALL_CLEANUP=1 perl -e 'alarm shift; exec @ARGV' 600 brew install --cask iterm2
  fi

  if [ ! -d "$app_path" ]; then
    reason="iTerm2 is unavailable after the installation attempt."
  else
    iterm_version="$(defaults read "$app_path/Contents/Info" CFBundleShortVersionString 2>&1)"
    iterm_executable="$(defaults read "$app_path/Contents/Info" CFBundleExecutable 2>&1)"
    perl -e 'alarm shift; exec @ARGV' 30 open -a "$app_path"
    command_text="printf 'PIG_SENTINEL_${GITHUB_RUN_ID}_${GITHUB_RUN_ATTEMPT}_%s\\n' \"\$(uname -m)\" | tee '$artifact_dir/sentinel.txt'; tty > '$artifact_dir/sentinel-tty.txt'"
    perl -e 'alarm shift; exec @ARGV' 90 osascript - "$command_text" "$expected" > "$artifact_dir/session.txt" 2> "$artifact_dir/applescript-error.txt" <<'APPLESCRIPT'
on run argv
  set commandText to item 1 of argv
  set expectedText to item 2 of argv
  tell application "iTerm"
    activate
    set probeWindow to (create window with default profile)
    set probeSession to current session of probeWindow
    set sessionID to unique id of probeSession
    set sessionTTY to tty of probeSession
    set windowID to id of probeWindow
    tell probeSession to write text commandText
    set sessionContents to ""
    repeat 30 times
      delay 1
      set sessionContents to text of probeSession
      if sessionContents contains expectedText then exit repeat
    end repeat
    return sessionID & linefeed & sessionTTY & linefeed & windowID & linefeed & sessionContents
  end tell
end run
APPLESCRIPT
    applescript_status=$?
    if [ "$applescript_status" -eq 0 ]; then
      for _ in {1..10}; do
        [ -s "$artifact_dir/sentinel-tty.txt" ] && break
        sleep 1
      done
    fi
    session_id="$(sed -n '1p' "$artifact_dir/session.txt")"
    session_tty="$(sed -n '2p' "$artifact_dir/session.txt")"
    window_id="$(sed -n '3p' "$artifact_dir/session.txt")"
    pgrep -fl "$app_path/Contents/MacOS/$iterm_executable" > "$artifact_dir/iterm-process.txt"
    process_status=$?
    cat "$artifact_dir/applescript-error.txt" "$artifact_dir/session.txt" "$artifact_dir/iterm-process.txt"
    if grep -q 'syntax error' "$artifact_dir/applescript-error.txt"; then
      status=failed
      reason="The AppleScript probe did not compile."
    elif [ "$applescript_status" -ne 0 ]; then
      reason="AppleScript could not create or inspect an iTerm2 session."
    elif [ "$process_status" -ne 0 ]; then
      reason="The iTerm2 process was not observed."
    elif [ -z "$session_id" ] || [ -z "$session_tty" ] || [ -z "$window_id" ]; then
      reason="The iTerm2 session identity was incomplete."
    elif ! grep -Fx "$expected" "$artifact_dir/sentinel.txt" || ! grep -Fx "$session_tty" "$artifact_dir/sentinel-tty.txt" || ! grep -Fx "$expected" "$artifact_dir/session.txt"; then
      reason="The session output or terminal side effect did not match the sentinel."
    else
      status=pass
      reason="The iTerm2 session produced the sentinel on its recorded terminal."
    fi
  fi
fi

if perl -e 'alarm shift; exec @ARGV' 30 screencapture -x "$artifact_dir/screenshot.png" && [ -s "$artifact_dir/screenshot.png" ]; then
  screenshot=screenshot.png
elif [ "$status" = pass ]; then
  status=blocked
  reason="The session produced the sentinel, but a screenshot was unavailable."
fi

export status reason host_os architecture macos_version commit_sha iterm_version iterm_executable session_id session_tty window_id screenshot expected artifact_dir
if ! python3 - <<'PYTHON'
import json
import os
from pathlib import Path

keys = ("status", "reason", "host_os", "architecture", "macos_version", "commit_sha", "iterm_version", "iterm_executable", "session_id", "session_tty", "window_id", "screenshot", "expected")
result = {key: os.environ[key] for key in keys}
sentinel_path = Path(os.environ["artifact_dir"], "sentinel.txt")
result.update({
    "runner_label": os.environ["PROBE_RUNNER_LABEL"],
    "runner_name": os.environ.get("RUNNER_NAME", ""),
    "runner_arch": os.environ.get("RUNNER_ARCH", ""),
    "github_sha": os.environ.get("GITHUB_SHA", ""),
    "run_id": os.environ.get("GITHUB_RUN_ID", ""),
    "run_attempt": os.environ.get("GITHUB_RUN_ATTEMPT", ""),
    "command_log": "command.log",
    "session_output": "session.txt",
    "sentinel_output": "sentinel.txt",
    "observed_sentinel": sentinel_path.read_text().strip() if sentinel_path.exists() else "",
    "sentinel_tty": "sentinel-tty.txt",
    "process_log": "iterm-process.txt",
})
output = json.dumps(result, indent=2) + "\n"
Path(os.environ["artifact_dir"], "result.json").write_text(output)
print(output, end="")
PYTHON
then
  exit 1
fi

[ "$status" = pass ]
