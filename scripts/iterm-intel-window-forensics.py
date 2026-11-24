#!/usr/bin/env python3
import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import secrets
import shlex
import shutil
import subprocess
import sys
import tempfile
import time


spec = importlib.util.spec_from_file_location("iterm_lifecycle", Path(__file__).with_name("iterm-lifecycle.py"))
lifecycle = importlib.util.module_from_spec(spec)
spec.loader.exec_module(lifecycle)
driver = lifecycle.driver

CENSUS = r'''
tell application "iTerm"
  set reportText to ""
  repeat with candidateWindow in windows
    set reportText to reportText & "window id=" & (id of candidateWindow as text) & linefeed
    try
      set reportText to reportText & "  name=" & (name of candidateWindow as text) & linefeed
    on error errorText number errorNumber
      set reportText to reportText & "  name error=" & errorNumber & " " & errorText & linefeed
    end try
    try
      set currentRef to current session of candidateWindow
      if currentRef is missing value then
        set reportText to reportText & "  current-session=missing" & linefeed
      else
        set reportText to reportText & "  current-session=" & (unique id of currentRef) & linefeed
      end if
    on error errorText number errorNumber
      set reportText to reportText & "  current-session error=" & errorNumber & " " & errorText & linefeed
    end try
    try
      set reportText to reportText & "  tabs=" & (count of tabs of candidateWindow) & linefeed
      set tabOrdinal to 0
      repeat with candidateTab in tabs of candidateWindow
        set tabOrdinal to tabOrdinal + 1
        set reportText to reportText & "  tab ordinal=" & tabOrdinal & " sessions=" & (count of sessions of candidateTab) & linefeed
        repeat with candidateSession in sessions of candidateTab
          try
            set reportText to reportText & "    session id=" & (unique id of candidateSession) & " tty=" & (tty of candidateSession) & " name=" & (name of candidateSession) & linefeed
          on error errorText number errorNumber
            set reportText to reportText & "    session error=" & errorNumber & " " & errorText & linefeed
          end try
        end repeat
      end repeat
    on error errorText number errorNumber
      set reportText to reportText & "  tabs error=" & errorNumber & " " & errorText & linefeed
    end try
  end repeat
  return reportText
end tell
'''

UI_CENSUS = r'''
tell application "System Events"
  set reportText to ""
  repeat with candidateProcess in application processes
    try
      set processName to name of candidateProcess
      repeat with candidateWindow in windows of candidateProcess
        try
          set windowText to (name of candidateWindow as text) & " " & ((value of every static text of candidateWindow) as text)
          if processName is "iTerm2" or processName is "iTerm" or windowText contains "local networks" then
            set reportText to reportText & "process=" & processName & " window=" & (name of candidateWindow as text) & linefeed
            repeat with attributeName in {"AXRole", "AXTitle", "AXModal", "AXPosition", "AXSize"}
              try
                set reportText to reportText & "  " & attributeName & "=" & (value of attribute attributeName of candidateWindow as text) & linefeed
              on error errorText number errorNumber
                set reportText to reportText & "  " & attributeName & " error=" & errorNumber & " " & errorText & linefeed
              end try
            end repeat
            set reportText to reportText & "  text=" & ((value of every static text of candidateWindow) as text) & linefeed
            set reportText to reportText & "  buttons=" & ((name of every button of candidateWindow) as text) & linefeed
            set reportText to reportText & "  sheets=" & (count of sheets of candidateWindow) & linefeed
            repeat with candidateSheet in sheets of candidateWindow
              set reportText to reportText & "  sheet text=" & ((value of every static text of candidateSheet) as text) & linefeed
              set reportText to reportText & "  sheet buttons=" & ((name of every button of candidateSheet) as text) & linefeed
            end repeat
          end if
        on error errorText number errorNumber
          set reportText to reportText & "  window error=" & errorNumber & " " & errorText & linefeed
        end try
      end repeat
    on error errorText number errorNumber
      set reportText to reportText & "process error=" & errorNumber & " " & errorText & linefeed
    end try
  end repeat
  return reportText
end tell
'''

OWNED_STATE = r'''
on run argv
  set wantedWindow to item 1 of argv
  set wantedSession to item 2 of argv
  tell application "iTerm"
    repeat with candidateWindow in windows
      if (id of candidateWindow as text) is wantedWindow then
        set currentRef to current session of candidateWindow
        if currentRef is missing value then return "present:missing"
        if (unique id of currentRef) is wantedSession then return "present:owned"
        return "present:changed"
      end if
    end repeat
  end tell
  return "missing"
end run
'''


SET_TITLE = r'''
on run argv
  set wantedWindow to item 1 of argv
  set wantedSession to item 2 of argv
  set wantedTitle to item 3 of argv
  tell application "iTerm"
    repeat with candidateWindow in windows
      if (id of candidateWindow as text) is wantedWindow then
        set candidateSession to current session of candidateWindow
        if candidateSession is missing value then error "Owned current session is missing."
        if (unique id of candidateSession) is not wantedSession then error "Owned session changed."
        tell candidateSession to set name to wantedTitle
        set windowName to name of candidateWindow
        set matchingWindows to 0
        repeat with otherWindow in windows
          if (name of otherWindow) is windowName then set matchingWindows to matchingWindows + 1
        end repeat
        return (name of candidateSession) & linefeed & windowName & linefeed & matchingWindows
      end if
    end repeat
  end tell
  error "Owned window is missing."
end run
'''


def parse_state(result):
    if result.get("returncode") != 0:
        raise RuntimeError("Owned window state query failed")
    state = result["stdout"].strip()
    if state not in {"missing", "present:missing", "present:owned", "present:changed"}:
        raise RuntimeError(f"Unexpected owned window state: {state!r}")
    return state


def verify_title(title, nonce):
    fields = (title or "").splitlines()
    if len(fields) != 3 or not fields[0].startswith(nonce) or fields[1] != nonce or fields[2] != "1":
        raise RuntimeError("The session title or unique window name could not be verified")


def shell_wrapper(scratch, nonce):
    release = scratch / "release"
    return (
        "#!/bin/sh\n"
        f"printf '%s\\n' \"$$\" > {shlex.quote(str(scratch / 'pid'))}\n"
        f"/usr/bin/tty > {shlex.quote(str(scratch / 'tty'))}\n"
        f"printf '\\033]0;%s\\007%s\\n' {shlex.quote(nonce)} {shlex.quote(nonce)}\n"
        f"printf '%s\\n' {shlex.quote(nonce)} > {shlex.quote(str(scratch / 'ready'))}\n"
        "count=0\n"
        f"while [ ! -f {shlex.quote(str(release))} ] && [ \"$count\" -lt 240 ]; do count=$((count + 1)); /bin/sleep 0.5; done\n"
        f"if [ -f {shlex.quote(str(release))} ]; then printf '0\\n' > {shlex.quote(str(scratch / 'exit'))}; exit 0; fi\n"
        f"printf '124\\n' > {shlex.quote(str(scratch / 'exit'))}\n"
        "exit 124\n"
    )


class Forensics(lifecycle.Diagnostic):
    def snapshot(self, stage):
        self.screenshot(stage)
        processes = driver.process_snapshot()
        ttys = {item["tty"].removeprefix("/dev/") for item in self.owned}
        if self.capability_tty:
            ttys.add(self.capability_tty.removeprefix("/dev/"))
        selected = [process for process in processes.values() if process["tty"] in ttys or process["command"].startswith("/Applications/iTerm.app/Contents/MacOS/")]
        self.event(stage + ".process-census", processes=selected)
        for name, script in (("iterm", CENSUS), ("ui", UI_CENSUS)):
            result = self.apple(stage + "." + name, script, timeout=15)
            (self.evidence / f"{stage}-{name}.txt").write_text(result.get("stdout", "") + result.get("stderr", ""))
            if result.get("returncode") != 0:
                self.failures.append(stage + "." + name)

    def state(self, stage, identity):
        return parse_state(self.apple(stage, OWNED_STATE, identity["window"], identity["session"], timeout=8))

    def wait_transition(self, identity, stage, seconds):
        deadline = time.monotonic() + seconds
        while True:
            state = self.state(stage, identity)
            if state != "present:owned" or time.monotonic() >= deadline:
                return state
            time.sleep(0.5)

    def run(self, capability):
        control = {"window": capability[0], "session": capability[1], "tty": self.capability_tty}
        self.event("control.identity", **control)
        if self.state("control.before", control) != "present:owned":
            raise RuntimeError("Capability control window is not live")
        control_processes = [process for process in driver.process_snapshot().values() if process["tty"] == self.capability_tty.removeprefix("/dev/")]
        if not control_processes:
            raise RuntimeError("Capability control has no live TTY process")
        self.event("control.processes", processes=control_processes)
        self.snapshot("baseline")
        created = self.action("target.create", driver.CREATE)
        fields = (created or "").splitlines()
        if len(fields) != 3 or not fields[0] or not re.fullmatch(r"/dev/ttys\d+", fields[1]) or not re.fullmatch(r"\d+", fields[2]):
            raise RuntimeError(f"Invalid target identity: {fields!r}")
        target = {"session": fields[0], "tty": fields[1], "window": fields[2]}
        if target["window"] == control["window"] or target["session"] == control["session"]:
            raise RuntimeError("Target and control identities overlap")
        self.owned.append(target)
        self.event("target.identity", **target)
        nonce = "P03_INTEL_" + secrets.token_hex(12)
        self.event("target.nonce", value=nonce)
        wrapper = self.scratch / "launch.sh"
        release = self.scratch / "release"
        wrapper.write_text(shell_wrapper(self.scratch, nonce))
        if self.action("target.send", driver.SESSION, target["window"], target["session"], "send", "exec /bin/sh " + shlex.quote(str(wrapper))) != "sent":
            raise RuntimeError("Could not start the attributed shell")
        pid_text = self.wait_file(self.scratch / "pid", "target.pid")
        tty = self.wait_file(self.scratch / "tty", "target.tty")
        ready = self.wait_file(self.scratch / "ready", "target.ready")
        if not pid_text or not pid_text.isdigit() or tty != target["tty"] or ready != nonce:
            raise RuntimeError("Shell PID, TTY, or nonce does not match")
        process = driver.process_snapshot().get(int(pid_text))
        if not process or process["tty"] != tty.removeprefix("/dev/"):
            raise RuntimeError("Shell process identity was not observed on its TTY")
        target["process"] = process
        self.event("target.process", process=process)
        title = self.action("target.set-title", SET_TITLE, target["window"], target["session"], nonce)
        verify_title(title, nonce)
        self.event("target.title", session_and_window=title)
        if self.state("target.before-exit-state", target) != "present:owned":
            raise RuntimeError("Target identity changed before exit")
        text = self.action("target.text", driver.SESSION, target["window"], target["session"], "text")
        if text is None or nonce not in text:
            raise RuntimeError("Target nonce is absent from the terminal")
        self.snapshot("before-exit")
        release.write_text("exit\n")
        exit_value = self.wait_file(self.scratch / "exit", "target.exit")
        if exit_value != "0":
            raise RuntimeError(f"Shell exit marker is {exit_value!r}")
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            current = driver.process_snapshot().get(process["pid"])
            if not current or current["start"] != process["start"]:
                self.event("target.process-exited", identity=process, observed=current)
                break
            time.sleep(0.2)
        else:
            raise RuntimeError("Attributed shell did not exit")
        initial_state = self.state("target.immediate-exit-state", target)
        self.event("target.immediate-exit-result", state=initial_state)
        self.snapshot("after-shell-exit")
        state = self.wait_transition(target, "target.natural-state", 8)
        self.event("target.natural-result", state=state)
        self.snapshot("after-natural-exit")
        if self.state("control.after-natural-exit", control) != "present:owned":
            raise RuntimeError("Control window changed during target exit")
        if state == "present:changed":
            raise RuntimeError("Target session changed; refusing to close")
        if state != "missing":
            closed = self.action("target.guarded-close", driver.SESSION, target["window"], target["session"], "close")
            self.event("target.close-result", value=closed)
            state = self.wait_transition(target, "target.after-close-state", 10)
        self.snapshot("after-guarded-close")
        self.event("target.final-state", state=state)
        if state == "present:changed":
            raise RuntimeError("Target session changed after close")
        if self.state("control.final", control) != "present:owned":
            raise RuntimeError("Control window changed during target close")
        return state


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--evidence", required=True, type=Path)
    args = parser.parse_args()
    args.evidence.mkdir(parents=True, exist_ok=True)
    if sys.platform != "darwin" or os.environ.get("PROBE_RUNNER_LABEL") != "macos-15-intel":
        raise RuntimeError("The diagnostic requires the Intel macOS runner")
    machine = subprocess.run(["/usr/bin/uname", "-m"], capture_output=True, text=True, check=True).stdout.strip()
    if machine != "x86_64":
        raise RuntimeError("The diagnostic requires an x86_64 machine")
    capability_path = Path(os.environ["RUNNER_TEMP"]) / "collaboration-platform-probe" / "result.json"
    capability = json.loads(capability_path.read_text())
    if any(capability.get(key) != value for key, value in (("status", "pass"), ("signature_status", "verified"), ("automation_status", "approved"))):
        raise RuntimeError("Signed iTerm capability and scoped consent are required")
    identity = driver.probe_identity(capability_path)
    if identity is None or not re.fullmatch(r"/dev/ttys\d+", capability.get("session_tty", "")):
        raise RuntimeError("The control window has no attributable identity")
    disk = subprocess.run(["/bin/df", "-P", "/private/tmp"], capture_output=True, text=True, check=True).stdout
    if os.path.realpath("/private/tmp") != "/private/tmp" or not disk.splitlines()[-1].split()[0].startswith("/dev/disk"):
        raise RuntimeError("The physical /private/tmp disk could not be verified")
    scratch = Path(tempfile.mkdtemp(prefix="itf.", dir="/private/tmp"))
    diagnostic = Forensics(args.evidence, scratch)
    diagnostic.capability_tty = capability["session_tty"]
    diagnostic.event("start", runner="macos-15-intel", machine=machine, scratch=str(scratch), scratch_device=scratch.stat().st_dev, scratch_inode=scratch.stat().st_ino)
    result = {"result": "incomplete", "state": None}
    try:
        result["state"] = diagnostic.run(identity)
        result["result"] = "observed" if not diagnostic.failures else "incomplete"
    except Exception as error:
        diagnostic.event("exception", error=repr(error))
        result["error"] = repr(error)
    finally:
        (scratch / "release").write_text("exit\n")
        try:
            diagnostic.snapshot("final-before-control-cleanup")
            diagnostic.action("control.guarded-close", driver.SESSION, *identity, "close")
            control = {"window": identity[0], "session": identity[1]}
            control_state = diagnostic.wait_transition(control, "control.post-cleanup-state", 10)
            diagnostic.event("control.cleanup-result", state=control_state)
            result["control_cleanup_state"] = control_state
            diagnostic.snapshot("after-control-cleanup")
        except Exception as error:
            diagnostic.event("cleanup.exception", error=repr(error))
            diagnostic.failures.append("cleanup.exception")
        for file in scratch.iterdir():
            if file.is_file():
                shutil.copy2(file, args.evidence / ("scratch-" + file.name))
        result["failures"] = diagnostic.failures
        result["target"] = diagnostic.owned
        (args.evidence / "summary.json").write_text(json.dumps(result, indent=2) + "\n")
    if result["result"] != "observed" or diagnostic.failures:
        raise RuntimeError("Diagnostic was incomplete; inspect the event ledger")


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(f"Intel iTerm window forensics failed: {error}", file=sys.stderr)
        sys.exit(1)
