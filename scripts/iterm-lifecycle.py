#!/usr/bin/env python3
import argparse
from datetime import datetime, timezone
import importlib.util
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import sys
import tempfile
import time


spec = importlib.util.spec_from_file_location("p03_macos", Path(__file__).with_name("p03-macos.py"))
driver = importlib.util.module_from_spec(spec)
spec.loader.exec_module(driver)

CENSUS = r'''
tell application "iTerm"
  set reportText to ""
  repeat with candidateWindow in windows
    try
      set windowID to id of candidateWindow as text
      set reportText to reportText & "window " & windowID & linefeed
      try
        set currentRef to current session of candidateWindow
        if currentRef is missing value then
          set reportText to reportText & "  current-session missing" & linefeed
        else
          set reportText to reportText & "  current-session " & (unique id of currentRef) & linefeed
        end if
      on error errorText number errorNumber
        set reportText to reportText & "  current-session error " & errorNumber & " " & errorText & linefeed
      end try
      repeat with candidateTab in tabs of candidateWindow
        try
          set tabIndex to index of candidateTab as text
          set tabID to "unavailable"
          try
            set tabID to id of candidateTab as text
          on error errorText number errorNumber
            set tabID to "error " & errorNumber & " " & errorText
          end try
          set reportText to reportText & "  tab index=" & tabIndex & " id=" & tabID & linefeed
          repeat with candidateSession in sessions of candidateTab
            try
              set reportText to reportText & "    session id=" & (unique id of candidateSession) & " tty=" & (tty of candidateSession) & linefeed
            on error errorText number errorNumber
              set reportText to reportText & "    session error " & errorNumber & " " & errorText & linefeed
            end try
          end repeat
        on error errorText number errorNumber
          set reportText to reportText & "  tab error " & errorNumber & " " & errorText & linefeed
        end try
      end repeat
    on error errorText number errorNumber
      set reportText to reportText & "window error " & errorNumber & " " & errorText & linefeed
    end try
  end repeat
  return reportText
end tell
'''

WINDOW_EXISTS = r'''
on run argv
  set wantedWindow to item 1 of argv
  tell application "iTerm"
    repeat with candidateWindow in windows
      if (id of candidateWindow as text) is wantedWindow then return "present"
    end repeat
  end tell
  return "missing"
end run
'''

UI_CENSUS = r'''
tell application "System Events"
  set reportText to ""
  repeat with candidateProcess in application processes
    try
      set processName to name of candidateProcess
      repeat with candidateWindow in windows of candidateProcess
        try
          set windowText to ""
          try
            set windowText to (name of candidateWindow as text) & " " & ((value of every static text of candidateWindow) as text)
          end try
          if processName is "iTerm2" or processName is "iTerm" or windowText contains "local networks" then
            set reportText to reportText & "process " & processName & linefeed
            set reportText to reportText & "  window name=" & (name of candidateWindow as text) & linefeed
            try
              set reportText to reportText & "    AXWindowNumber=" & (value of attribute "AXWindowNumber" of candidateWindow as text) & linefeed
            on error errorText number errorNumber
              set reportText to reportText & "    AXWindowNumber error " & errorNumber & " " & errorText & linefeed
            end try
            try
              set reportText to reportText & "    AXModal=" & (value of attribute "AXModal" of candidateWindow as text) & linefeed
            on error errorText number errorNumber
              set reportText to reportText & "    AXModal error " & errorNumber & " " & errorText & linefeed
            end try
            set reportText to reportText & "    sheets=" & (count of sheets of candidateWindow) & linefeed
            repeat with candidateSheet in sheets of candidateWindow
              set reportText to reportText & "    sheet text=" & ((value of every static text of candidateSheet) as text) & linefeed
              set reportText to reportText & "    sheet buttons=" & ((name of every button of candidateSheet) as text) & linefeed
            end repeat
            set reportText to reportText & "    buttons=" & ((name of every button of candidateWindow) as text) & linefeed
            set reportText to reportText & "    text=" & ((value of every static text of candidateWindow) as text) & linefeed
          end if
        on error errorText number errorNumber
          set reportText to reportText & "  window inspection error " & errorNumber & " " & errorText & linefeed
        end try
      end repeat
    on error errorText number errorNumber
      set reportText to reportText & "process inspection error " & errorNumber & " " & errorText & linefeed
    end try
  end repeat
  return reportText
end tell
'''

ACKNOWLEDGE_WARNING = r'''
on run argv
  set wantedWindow to item 1 of argv
  tell application "System Events"
    if not (exists process "iTerm2") then return "iTerm process unavailable"
    tell process "iTerm2"
      repeat with candidateWindow in windows
        set candidateID to ""
        try
          set candidateID to value of attribute "AXWindowNumber" of candidateWindow as text
        end try
        if candidateID is wantedWindow then
          set candidates to {candidateWindow} & (every sheet of candidateWindow)
          repeat with candidateDialog in candidates
            set dialogText to ""
            try
              set dialogText to (value of every static text of candidateDialog) as text
            end try
            if dialogText contains "A session ended very soon after starting." and dialogText contains "Check that the command in profile" and dialogText contains "Default" and dialogText contains "is correct." then
              if exists button "OK" of candidateDialog then
                click button "OK" of candidateDialog
                return "acknowledged owned short-session warning: " & dialogText
              end if
            end if
          end repeat
        end if
      end repeat
    end tell
  end tell
  return "no attributable short-session warning"
end run
'''

ALLOW_ITERM_NETWORK = r'''
tell application "System Events"
  repeat with candidateProcess in application processes
    try
      repeat with candidateWindow in windows of candidateProcess
        try
          set fields to value of every static text of candidateWindow
          repeat with field in fields
            if (contents of field) is "Allow “iTerm” to find devices on local networks?" then
              if exists button "Allow" of candidateWindow then
                set processName to name of candidateProcess
                click button "Allow" of candidateWindow
                return "allowed exact iTerm local-network prompt from " & processName
              end if
            end if
          end repeat
        end try
      end repeat
    end try
  end repeat
end tell
return "no exact iTerm local-network prompt"
'''


class Diagnostic:
    def __init__(self, evidence, scratch):
        self.evidence = evidence
        self.scratch = scratch
        self.failures = []
        self.owned = []
        self.capability_tty = ""
        self.events = evidence / "events.jsonl"

    def event(self, stage, **fields):
        record = {"time": datetime.now(timezone.utc).isoformat(), "stage": stage, **fields}
        with self.events.open("a") as output:
            output.write(json.dumps(record, ensure_ascii=False) + "\n")

    def command(self, stage, argv, input_text=None, timeout=10):
        started = time.monotonic()
        self.event(stage + ".start", argv=argv, timeout=timeout)
        try:
            result = subprocess.run(argv, input=input_text, text=True, capture_output=True, timeout=timeout)
            record = {"returncode": result.returncode, "stdout": result.stdout, "stderr": result.stderr}
        except subprocess.TimeoutExpired as error:
            def decode(value):
                return value.decode(errors="replace") if isinstance(value, bytes) else (value or "")
            record = {"timeout": True, "stdout": decode(error.stdout), "stderr": decode(error.stderr)}
        self.event(stage + ".end", elapsed_seconds=round(time.monotonic() - started, 3), **record)
        return record

    def apple(self, stage, script, *arguments, timeout=30):
        return self.command(stage, ["/usr/bin/osascript", "-", *arguments], script, timeout)

    def screenshot(self, stage):
        target = self.evidence / f"{stage}.png"
        screenshot = self.command(stage + ".screenshot", ["/usr/sbin/screencapture", "-x", str(target)], timeout=15)
        if screenshot.get("returncode") != 0 or not target.is_file() or target.stat().st_size == 0:
            self.failures.append(stage + ".screenshot")

    def snapshot(self, stage):
        self.screenshot(stage)
        try:
            ttys = {item["tty"].removeprefix("/dev/") for item in self.owned}
            if self.capability_tty:
                ttys.add(self.capability_tty.removeprefix("/dev/"))
            processes = driver.process_snapshot()
            selected = [process for process in processes.values() if process["pid"] == os.getpid() or process["tty"] in ttys or process["command"].startswith("/Applications/iTerm.app/Contents/MacOS/")]
            self.event(stage + ".process-census", processes=selected)
        except Exception as error:
            self.failures.append(stage + ".process-census")
            self.event(stage + ".process-census", error=repr(error))
        for name, script in (("census", CENSUS), ("ui", UI_CENSUS)):
            result = self.apple(stage + "." + name, script, timeout=8)
            (self.evidence / f"{stage}-{name}.txt").write_text(result.get("stdout", "") + result.get("stderr", ""))
            if result.get("returncode") != 0:
                self.failures.append(stage + "." + name)

    def action(self, stage, script, *arguments, timeout=30):
        result = self.apple(stage, script, *arguments, timeout=timeout)
        if result.get("returncode") != 0:
            self.failures.append(stage)
            self.snapshot(stage + "-error")
            return None
        return result["stdout"].rstrip("\n")

    def wait_file(self, path, stage, seconds=15):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            if path.is_file():
                value = path.read_text().strip()
                if value:
                    self.event(stage, path=str(path), value=value)
                    return value
            time.sleep(0.2)
        self.failures.append(stage)
        self.event(stage, timeout=True, path=str(path))
        self.snapshot(stage + "-timeout")
        return None

    def window_gone(self, stage, window):
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            result = self.apple(stage, WINDOW_EXISTS, window, timeout=5)
            if result.get("returncode") == 0 and result["stdout"].strip() == "missing":
                return True
            time.sleep(0.5)
        self.failures.append(stage)
        self.snapshot(stage + "-timeout")
        return False

    def run_window(self, number):
        stage = f"window-{number}"
        self.snapshot(stage + "-before-create")
        created = self.action(stage + ".create", driver.CREATE)
        if created is None:
            return
        identity = created.splitlines()
        if len(identity) != 3 or not identity[0] or not re.fullmatch(r"/dev/ttys\d+", identity[1]) or not re.fullmatch(r"\d+", identity[2]):
            self.failures.append(stage + ".identity")
            self.event(stage + ".identity", invalid=identity)
            self.snapshot(stage + "-identity-error")
            return
        session, tty, window = identity
        owned = {"window": window, "session": session, "tty": tty}
        self.owned.append(owned)
        self.event(stage + ".identity", **owned)
        generation = self.scratch / stage
        generation.mkdir()
        sentinel = f"ITERM_LIFECYCLE_{number}_{os.environ.get('GITHUB_RUN_ID', 'local')}"
        wrapper = generation / "launch.sh"
        wrapper.write_text("#!/bin/sh\n" +
            "printf '%s\\n' \"$$\" > " + shlex.quote(str(generation / "pid")) + "\n" +
            "/usr/bin/tty > " + shlex.quote(str(generation / "tty")) + "\n" +
            "/bin/ps -o pid=,ppid=,tty=,lstart=,command= -p \"$$\" > " + shlex.quote(str(generation / "process")) + "\n" +
            "printf '%s\\n' " + shlex.quote(sentinel) + " | /usr/bin/tee " + shlex.quote(str(generation / "sentinel")) + "\n" +
            ("/bin/sleep 2\n" if number == 1 else "IFS= read -r ignored || :\n") +
            "printf '0\\n' > " + shlex.quote(str(generation / "exit")) + "\nexit 0\n")
        command = "exec /bin/sh " + shlex.quote(str(wrapper))
        owned["command_sent"] = True
        if self.action(stage + ".send", driver.SESSION, window, session, "send", command) is None:
            return
        self.screenshot(stage + "-before-exit")
        pid_text = self.wait_file(generation / "pid", stage + ".pid")
        terminal_tty = self.wait_file(generation / "tty", stage + ".tty")
        seen_sentinel = self.wait_file(generation / "sentinel", stage + ".sentinel")
        process_text = self.wait_file(generation / "process", stage + ".process-file")
        if not pid_text or not pid_text.isdigit() or terminal_tty != tty or seen_sentinel != sentinel or not process_text:
            self.failures.append(stage + ".shell-identity")
            self.snapshot(stage + "-shell-identity-error")
            return
        pid = int(pid_text)
        match = re.fullmatch(r"\s*(\d+)\s+(\d+)\s+(\S+)\s+(\S+\s+\S+\s+\d+\s+\S+\s+\d+)\s+(.*)", process_text)
        process = None
        if match:
            process = {"pid": int(match[1]), "ppid": int(match[2]), "tty": match[3], "start": " ".join(match[4].split()), "command": match[5]}
        self.event(stage + ".process", process=process, expected_tty=tty)
        if not process or process["pid"] != pid or process["tty"] != tty.removeprefix("/dev/"):
            self.failures.append(stage + ".process-identity")
            self.snapshot(stage + "-process-identity-error")
            return
        owned["process"] = process
        if number == 1:
            self.snapshot(stage + "-post-send")
        if number != 1:
            text = self.action(stage + ".text", driver.SESSION, window, session, "text")
            if text is None or sentinel not in text:
                self.failures.append(stage + ".screen-sentinel")
            self.snapshot(stage + "-running")
            self.action(stage + ".exit", driver.SESSION, window, session, "exit")
        exit_status = self.wait_file(generation / "exit", stage + ".exit-marker")
        if exit_status != "0":
            self.failures.append(stage + ".exit-status")
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            current = driver.process_snapshot().get(pid)
            if not current or current["start"] != process["start"]:
                self.event(stage + ".process-exited", pid=pid, birth=process["start"], observed=current)
                break
            time.sleep(0.2)
        else:
            self.failures.append(stage + ".process-exit")
            self.snapshot(stage + "-process-exit-timeout")
            return
        self.snapshot(stage + "-before-close")
        self.action(stage + ".close", driver.SESSION, window, session, "close")
        owned["disappeared"] = self.window_gone(stage + ".window-exists", window)
        self.snapshot(stage + "-after-close")
        if not owned["disappeared"]:
            warning = self.action(stage + ".acknowledge-warning", ACKNOWLEDGE_WARNING, window, timeout=10)
            self.snapshot(stage + "-after-warning-check")
            if warning and warning.startswith("acknowledged owned"):
                self.action(stage + ".exists-after-warning", driver.SESSION, window, session, "exists")
                owned["disappeared"] = self.window_gone(stage + ".window-after-warning", window)
            if not owned["disappeared"]:
                network = self.action(stage + ".allow-iterm-network", ALLOW_ITERM_NETWORK, timeout=10)
                self.snapshot(stage + "-after-network-check")
                if network and network.startswith("allowed exact"):
                    self.action(stage + ".exists-after-network", driver.SESSION, window, session, "exists")
                    owned["disappeared"] = self.window_gone(stage + ".window-after-network", window)

    def cleanup(self):
        for owned in self.owned:
            if owned.get("disappeared"):
                continue
            stage = "cleanup-" + owned["window"]
            process = owned.get("process")
            if owned.get("command_sent") and not process:
                self.failures.append(stage + ".unverified-process")
                self.event(stage + ".retained", reason="The shell process identity was not observed")
                continue
            if process:
                current = driver.process_snapshot().get(process["pid"])
                if current and current["start"] == process["start"]:
                    self.action(stage + ".exit", driver.SESSION, owned["window"], owned["session"], "exit")
                    deadline = time.monotonic() + 10
                    while time.monotonic() < deadline:
                        current = driver.process_snapshot().get(process["pid"])
                        if not current or current["start"] != process["start"]:
                            break
                        time.sleep(0.2)
                    else:
                        self.failures.append(stage + ".process-alive")
                        self.snapshot(stage + "-process-alive")
                        continue
            self.action(stage + ".close", driver.SESSION, owned["window"], owned["session"], "close")
            owned["disappeared"] = self.window_gone(stage + ".window-exists", owned["window"])
        self.snapshot("final")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--evidence", required=True, type=Path)
    args = parser.parse_args()
    args.evidence.mkdir(parents=True, exist_ok=True)
    if sys.platform != "darwin":
        raise RuntimeError("The lifecycle diagnostic requires macOS")
    runner = os.environ["PROBE_RUNNER_LABEL"]
    machine = subprocess.run(["/usr/bin/uname", "-m"], capture_output=True, text=True, check=True).stdout.strip()
    if (runner, machine) not in (("macos-15", "arm64"), ("macos-15-intel", "x86_64")):
        raise RuntimeError("Runner label and architecture differ")
    capability_path = Path(os.environ["RUNNER_TEMP"]) / "collaboration-platform-probe" / "result.json"
    capability = json.loads(capability_path.read_text())
    if any(capability.get(key) != value for key, value in (("status", "pass"), ("signature_status", "verified"), ("automation_status", "approved"))):
        raise RuntimeError("Signed iTerm capability and scoped consent are required")
    identity = driver.probe_identity(capability_path)
    if identity is None:
        raise RuntimeError("The capability window has no attributable identity")
    disk = subprocess.run(["/bin/df", "-P", "/private/tmp"], capture_output=True, text=True, check=True).stdout
    if os.path.realpath("/private/tmp") != "/private/tmp" or not disk.splitlines()[-1].split()[0].startswith("/dev/disk"):
        raise RuntimeError("The physical /private/tmp disk could not be verified")
    scratch = Path(tempfile.mkdtemp(prefix="itl.", dir="/private/tmp"))
    scratch_identity = scratch.stat()
    diagnostic = Diagnostic(args.evidence, scratch)
    diagnostic.capability_tty = capability.get("session_tty", "")
    diagnostic.event("start", runner=runner, machine=machine, scratch=str(scratch), scratch_device=scratch_identity.st_dev, scratch_inode=scratch_identity.st_ino, capability_window=identity[0], capability_session=identity[1])
    capability_gone = False
    try:
        diagnostic.snapshot("baseline")
        present = diagnostic.action("capability.exists", driver.SESSION, *identity, "exists")
        if present != "present":
            diagnostic.failures.append("capability.unavailable")
        else:
            for number in range(1, 4):
                try:
                    diagnostic.run_window(number)
                except Exception as error:
                    diagnostic.failures.append(f"window-{number}.exception")
                    diagnostic.event(f"window-{number}.exception", error=repr(error))
                    diagnostic.snapshot(f"window-{number}-exception")
    except Exception as error:
        diagnostic.failures.append("diagnostic.exception")
        diagnostic.event("diagnostic.exception", error=repr(error))
    finally:
        try:
            diagnostic.cleanup()
        except Exception as error:
            diagnostic.failures.append("cleanup.exception")
            diagnostic.event("cleanup.exception", error=repr(error))
        try:
            diagnostic.action("capability.close", driver.SESSION, *identity, "close")
            capability_gone = diagnostic.window_gone("capability.window-exists", identity[0])
            diagnostic.snapshot("after-capability-close")
        except Exception as error:
            diagnostic.failures.append("capability.cleanup.exception")
            diagnostic.event("capability.cleanup.exception", error=repr(error))
        retained = [owned for owned in diagnostic.owned if not owned.get("disappeared")]
        summary = {"result": "failed" if diagnostic.failures else "pass", "failures": diagnostic.failures, "windows": diagnostic.owned, "capability_window": identity[0], "capability_disappeared": capability_gone, "scratch": str(scratch)}
        (args.evidence / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
        current = scratch.stat()
        if not diagnostic.failures and not retained and capability_gone and (current.st_dev, current.st_ino) == (scratch_identity.st_dev, scratch_identity.st_ino):
            shutil.rmtree(scratch)
            diagnostic.event("scratch-removed", path=str(scratch))
        else:
            diagnostic.event("scratch-retained", path=str(scratch))
    if diagnostic.failures:
        raise RuntimeError("Lifecycle diagnostic failed; inspect summary.json and events.jsonl")


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(f"iTerm lifecycle diagnostic failed: {error}", file=sys.stderr)
        sys.exit(1)
