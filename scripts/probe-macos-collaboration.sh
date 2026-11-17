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
signature_status=not_checked
first_launch_status=not_checked
automation_status=not_checked
open_status=not_started
gui_status=not_attempted
automation_gui_status=not_attempted
applescript_status=not_attempted
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
    codesign --verify --deep --strict --verbose=2 "$app_path" >"$artifact_dir/signature-verification.txt" 2>&1
    signature_verification_status=$?
    codesign -dv --verbose=4 "$app_path" >"$artifact_dir/signature-details.txt" 2>&1
    signature_details_status=$?
    perl -e 'alarm shift; exec @ARGV' 60 spctl --assess --type execute --verbose=4 "$app_path" >"$artifact_dir/notarization.txt" 2>&1
    notarization_status=$?
    cat "$artifact_dir/signature-verification.txt" "$artifact_dir/signature-details.txt" "$artifact_dir/notarization.txt"
    if [ "$signature_verification_status" -eq 0 ] &&
      [ "$signature_details_status" -eq 0 ] &&
      [ "$notarization_status" -eq 0 ] &&
      grep -Fx 'Identifier=com.googlecode.iterm2' "$artifact_dir/signature-details.txt" &&
      grep -Fx 'TeamIdentifier=H7V7XYVQ7D' "$artifact_dir/signature-details.txt" &&
      grep -Fx 'Authority=Developer ID Application: GEORGE NACHMAN (H7V7XYVQ7D)' "$artifact_dir/signature-details.txt" &&
      grep -F 'source=Notarized Developer ID' "$artifact_dir/notarization.txt"; then
      signature_status=verified
      perl -e 'alarm shift; exec @ARGV' 120 open -a "$app_path" >"$artifact_dir/open.txt" 2>&1 &
      open_pid=$!
      for _ in {1..5}; do
        kill -0 "$open_pid" 2>/dev/null || break
        sleep 1
      done
      if kill -0 "$open_pid" 2>/dev/null; then
        perl -e 'alarm shift; exec @ARGV' 100 osascript - "$artifact_dir/first-launch.png" >"$artifact_dir/first-launch.txt" 2>&1 <<'APPLESCRIPT'
on run argv
  set screenshotPath to item 1 of argv
  tell application "System Events"
    repeat 80 times
      if exists process "CoreServicesUIAgent" then
        repeat with candidateWindow in (every window of process "CoreServicesUIAgent")
          set dialogText to (name of candidateWindow as text)
          set dialogTexts to (get value of every static text of candidateWindow)
          repeat with textItem in dialogTexts
            set textValue to contents of textItem
            if textValue is not missing value then
              set dialogText to dialogText & " " & (textValue as text)
            end if
          end repeat
          log dialogText
          if dialogText contains "iTerm" and dialogText contains "downloaded" and dialogText contains "Internet" and (exists button "Open" of candidateWindow) then
            set quotedPath to quoted form of screenshotPath
            tell me to do shell script "/usr/sbin/screencapture -x " & quotedPath & " && test -s " & quotedPath
            click button "Open" of candidateWindow
            return "Opened the matching iTerm first-launch dialog."
          end if
        end repeat
      end if
      delay 1
    end repeat
  end tell
  error "The matching iTerm first-launch dialog was not found."
end run
APPLESCRIPT
        gui_status=$?
        if [ "$gui_status" -eq 0 ]; then
          first_launch_status=approved
        else
          first_launch_status=blocked
          if grep -q 'syntax error' "$artifact_dir/first-launch.txt"; then
            status=failed
            reason="The first-launch GUI script did not compile."
          elif grep -Fq '(-1700)' "$artifact_dir/first-launch.txt"; then
            status=failed
            reason="The first-launch GUI script could not read dialog text."
          elif [ "$gui_status" -eq 142 ]; then
            reason="The first-launch GUI script timed out."
          else
            reason="The iTerm2 first-launch dialog could not be approved by GUI automation."
          fi
          if [ ! -s "$artifact_dir/first-launch.png" ]; then
            perl -e 'alarm shift; exec @ARGV' 30 screencapture -x "$artifact_dir/first-launch.png"
          fi
        fi
        cat "$artifact_dir/first-launch.txt"
      else
        first_launch_status=not_prompted
      fi
      for _ in {1..10}; do
        kill -0 "$open_pid" 2>/dev/null || break
        sleep 1
      done
      if kill -0 "$open_pid" 2>/dev/null; then
        kill "$open_pid"
        if [ "$first_launch_status" != blocked ]; then
          first_launch_status=blocked
          reason="The iTerm2 launch did not finish after the first-launch check."
        fi
      fi
      wait "$open_pid"
      open_status=$?
      cat "$artifact_dir/open.txt"
      if [ "$open_status" -ne 0 ] && [ "$first_launch_status" != blocked ]; then
        first_launch_status=blocked
        reason="The iTerm2 launch failed after the first-launch check."
      fi
    else
      signature_status=failed
      status=failed
      reason="iTerm2 signature or notarization verification failed."
    fi
    if [ "$signature_status" = verified ] && [ "$first_launch_status" != blocked ]; then
      command_text="printf 'PIG_SENTINEL_${GITHUB_RUN_ID}_${GITHUB_RUN_ATTEMPT}_%s\\n' \"\$(uname -m)\" | tee '$artifact_dir/sentinel.txt'; tty > '$artifact_dir/sentinel-tty.txt'"
      perl -e 'alarm shift; exec @ARGV' 180 osascript - "$command_text" "$expected" >"$artifact_dir/session.txt" 2>"$artifact_dir/applescript-error.txt" <<'APPLESCRIPT' &
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
      session_pid=$!
      perl -e 'alarm shift; exec @ARGV' 100 osascript - "$artifact_dir/automation-consent.png" >"$artifact_dir/automation-consent.txt" 2>&1 <<'APPLESCRIPT'
on run argv
  set screenshotPath to item 1 of argv
  set expectedPrompt to "“hosted-compute-agent” wants access to control “iTerm”."
  tell application "System Events"
    repeat 30 times
      repeat with dialogProcess in (every process)
        set processWindows to {}
        try
          set processWindows to (get every window of dialogProcess)
        on error errorText number errorNumber
          log "Cannot inspect process windows: " & errorText & " (" & errorNumber & ")"
        end try
        repeat with candidateWindow in processWindows
          set matchesPrompt to false
          set dialogText to ""
          try
            if exists button "Allow" of candidateWindow then
              set windowName to (get name of candidateWindow)
              if windowName is not missing value then set dialogText to (windowName as text)
              set dialogTexts to (get value of every static text of candidateWindow)
              repeat with textItem in dialogTexts
                set textValue to contents of textItem
                if textValue is not missing value then
                  set dialogText to dialogText & " " & (textValue as text)
                end if
              end repeat
              if dialogText contains expectedPrompt then set matchesPrompt to true
            end if
          on error errorText number errorNumber
            log "Cannot inspect window: " & errorText & " (" & errorNumber & ")"
          end try
          if matchesPrompt then
            log dialogText
            set quotedPath to quoted form of screenshotPath
            tell me to do shell script "/usr/sbin/screencapture -x " & quotedPath & " && test -s " & quotedPath
            click button "Allow" of candidateWindow
            return "Allowed the matching hosted-compute-agent to iTerm Automation request."
          end if
        end repeat
      end repeat
      delay 1
    end repeat
  end tell
  error "The matching hosted-compute-agent to iTerm Automation request was not found."
end run
APPLESCRIPT
      automation_gui_status=$?
      if [ "$automation_gui_status" -eq 0 ]; then
        automation_status=approved
      else
        automation_status=blocked
        reason="The specific iTerm Automation consent could not be approved."
        if grep -q 'syntax error' "$artifact_dir/automation-consent.txt" || grep -Eq 'execution error:.*\(-1700\)' "$artifact_dir/automation-consent.txt"; then
          status=failed
          reason="The Automation consent script failed to inspect dialog text."
        fi
        if [ ! -s "$artifact_dir/automation-consent.png" ]; then
          perl -e 'alarm shift; exec @ARGV' 30 screencapture -x "$artifact_dir/automation-consent.png"
        fi
        kill "$session_pid" 2>/dev/null || true
      fi
      cat "$artifact_dir/automation-consent.txt"
      wait "$session_pid"
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
      pgrep -fl "$app_path/Contents/MacOS/$iterm_executable" >"$artifact_dir/iterm-process.txt"
      process_status=$?
      cat "$artifact_dir/applescript-error.txt" "$artifact_dir/session.txt" "$artifact_dir/iterm-process.txt"
      if [ "$automation_status" = approved ]; then
        if grep -q 'syntax error' "$artifact_dir/applescript-error.txt"; then
          status=failed
          reason="The AppleScript probe did not compile."
        elif [ "$applescript_status" -eq 142 ]; then
          reason="The iTerm2 session AppleScript timed out."
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
    else
      pgrep -fl "$app_path/Contents/MacOS/$iterm_executable" >"$artifact_dir/iterm-process.txt"
      cat "$artifact_dir/iterm-process.txt"
    fi
  fi
fi

if perl -e 'alarm shift; exec @ARGV' 30 screencapture -x "$artifact_dir/screenshot.png" && [ -s "$artifact_dir/screenshot.png" ]; then
  screenshot=screenshot.png
elif [ "$status" = pass ]; then
  status=blocked
  reason="The session produced the sentinel, but a screenshot was unavailable."
fi

export status reason host_os architecture macos_version commit_sha iterm_version iterm_executable signature_status first_launch_status automation_status open_status gui_status automation_gui_status applescript_status session_id session_tty window_id screenshot expected artifact_dir
if ! python3 - <<'PYTHON'
import json
import os
from pathlib import Path

keys = ("status", "reason", "host_os", "architecture", "macos_version", "commit_sha", "iterm_version", "iterm_executable", "signature_status", "first_launch_status", "automation_status", "open_status", "gui_status", "automation_gui_status", "applescript_status", "session_id", "session_tty", "window_id", "screenshot", "expected")
result = {key: os.environ[key] for key in keys}
sentinel_path = Path(os.environ["artifact_dir"], "sentinel.txt")
first_launch_path = Path(os.environ["artifact_dir"], "first-launch.txt")
automation_consent_path = Path(os.environ["artifact_dir"], "automation-consent.txt")
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
    "signature_log": "signature-verification.txt",
    "signature_details": "signature-details.txt",
    "notarization_log": "notarization.txt",
    "first_launch_log": "first-launch.txt",
    "first_launch_output": first_launch_path.read_text().strip() if first_launch_path.exists() else "",
    "first_launch_screenshot": "first-launch.png" if Path(os.environ["artifact_dir"], "first-launch.png").exists() else "",
    "automation_consent_output": automation_consent_path.read_text().strip() if automation_consent_path.exists() else "",
    "automation_consent_screenshot": "automation-consent.png" if Path(os.environ["artifact_dir"], "automation-consent.png").exists() else "",
})
output = json.dumps(result, indent=2) + "\n"
Path(os.environ["artifact_dir"], "result.json").write_text(output)
print(output, end="")
PYTHON
then
  exit 1
fi

[ "$status" = pass ]
