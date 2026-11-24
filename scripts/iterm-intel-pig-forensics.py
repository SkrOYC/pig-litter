#!/usr/bin/env python3
import argparse
import ctypes
import importlib.util
import json
import os
from pathlib import Path
import plistlib
import re
import subprocess
import sys
import time


spec = importlib.util.spec_from_file_location("intel_window_forensics", Path(__file__).with_name("iterm-intel-window-forensics.py"))
window_probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(window_probe)
driver = window_probe.driver


def native_windows():
    graphics = ctypes.CDLL("/System/Library/Frameworks/CoreGraphics.framework/CoreGraphics")
    foundation = ctypes.CDLL("/System/Library/Frameworks/CoreFoundation.framework/CoreFoundation")
    graphics.CGWindowListCopyWindowInfo.argtypes = [ctypes.c_uint32, ctypes.c_uint32]
    graphics.CGWindowListCopyWindowInfo.restype = ctypes.c_void_p
    foundation.CFPropertyListCreateData.argtypes = [ctypes.c_void_p, ctypes.c_void_p, ctypes.c_long, ctypes.c_ulong, ctypes.POINTER(ctypes.c_void_p)]
    foundation.CFPropertyListCreateData.restype = ctypes.c_void_p
    foundation.CFDataGetLength.argtypes = [ctypes.c_void_p]
    foundation.CFDataGetLength.restype = ctypes.c_long
    foundation.CFDataGetBytePtr.argtypes = [ctypes.c_void_p]
    foundation.CFDataGetBytePtr.restype = ctypes.POINTER(ctypes.c_ubyte)
    foundation.CFRelease.argtypes = [ctypes.c_void_p]
    windows = graphics.CGWindowListCopyWindowInfo(0, 0)
    if not windows:
        raise RuntimeError("Core Graphics did not return a window census")
    data = None
    error = ctypes.c_void_p()
    try:
        data = foundation.CFPropertyListCreateData(None, windows, 200, 0, ctypes.byref(error))
        if not data:
            raise RuntimeError("Core Graphics window census could not be serialized")
        content = ctypes.string_at(foundation.CFDataGetBytePtr(data), foundation.CFDataGetLength(data))
        values = plistlib.loads(content)
        if not isinstance(values, list):
            raise RuntimeError("Core Graphics returned an unexpected window census")
        records = []
        for value in values:
            if not isinstance(value, dict) or not isinstance(value.get("kCGWindowNumber"), int) or not isinstance(value.get("kCGWindowOwnerPID"), int):
                raise RuntimeError("Core Graphics window identity is incomplete")
            records.append({"number": value["kCGWindowNumber"], "owner_pid": value["kCGWindowOwnerPID"], "onscreen": value.get("kCGWindowIsOnscreen")})
        return records
    finally:
        if error.value:
            foundation.CFRelease(error)
        if data:
            foundation.CFRelease(data)
        foundation.CFRelease(windows)


def require_native_mapping(records, window, owner_pid):
    matches = [record for record in records if record["number"] == int(window["window"])]
    if len(matches) != 1 or matches[0]["owner_pid"] != owner_pid:
        raise RuntimeError("The iTerm window ID did not map to the expected native owner")


def check_fixture(fixture, prompted):
    if fixture.errors:
        raise RuntimeError(f"The local provider failed: {fixture.errors}")
    if prompted and (fixture.requests < 1 or fixture.stage != 1):
        raise RuntimeError("The deterministic prompt did not reach the local provider")
    if not prompted and fixture.requests:
        raise RuntimeError("The startup-only session made a provider request")


def verify_pig_title(title, nonce):
    fields = (title or "").splitlines()
    if len(fields) != 3 or not (fields[0] == nonce or fields[0].startswith(nonce + " (")) or not fields[1] or not fields[2].isdigit() or int(fields[2]) < 1:
        raise RuntimeError("The session nonce or window title could not be verified")


class PigForensics(window_probe.Forensics):
    def __init__(self, evidence, scratch):
        super().__init__(evidence, scratch)
        self.case_name = "setup"
        self.fixture = None
        self.server = None
        self.thread = None
        self.iterm = None

    def snapshot(self, stage):
        super().snapshot(stage)
        current = driver.process_snapshot().get(self.iterm["pid"])
        if not current or current["start"] != self.iterm["start"]:
            raise RuntimeError("The observed iTerm process identity changed")
        records = native_windows()
        relevant_ids = {int(item["window"]) for item in self.owned}
        relevant_ids.add(int(self.control["window"]))
        selected = [record for record in records if record["owner_pid"] == self.iterm["pid"] or record["number"] in relevant_ids]
        native = {"owner": self.iterm, "total_windows": len(records), "windows": selected}
        self.event(stage + ".native-census", **native)
        (self.evidence / f"{stage}-native.json").write_text(json.dumps(native, indent=2) + "\n")
        self.event(stage + ".fixture-census", process=driver.process_snapshot().get(os.getpid()),
                   listening=self.server.server_address if self.server else None,
                   thread_alive=self.thread.is_alive() if self.thread else False,
                   requests=self.fixture.requests if self.fixture else None,
                   errors=self.fixture.errors if self.fixture else [])
        return records

    def recorded_applescript(self, script, *arguments):
        for value in arguments:
            driver.safe_text(value)
        if script == driver.CREATE:
            stage = "create"
        elif script == driver.SESSION and len(arguments) >= 3:
            stage = "session-" + arguments[2]
        else:
            raise RuntimeError("Unexpected driver AppleScript")
        result = self.apple(self.case_name + ".driver-" + stage, script, *arguments, timeout=30)
        if result.get("returncode") != 0:
            raise RuntimeError(f"AppleScript failed: {result.get('stderr', '').strip()}")
        return result["stdout"].rstrip("\n")

    def check_control(self, stage, control):
        if self.state(stage, control) != "present:owned":
            raise RuntimeError("The control window changed")
        current = driver.process_snapshot()
        if not any(current.get(process["pid"], {}).get("start") == process["start"] for process in control["processes"]):
            raise RuntimeError("No original control TTY process remains alive")

    def observe_disappearance(self, target, seconds=30):
        deadline = time.monotonic() + seconds
        while True:
            state = self.state(self.case_name + ".settling-state", target)
            if state in ("missing", "present:changed") or time.monotonic() >= deadline:
                return state
            time.sleep(0.5)

    def run_case(self, name, pig, control, prompted):
        self.case_name = name
        root = self.scratch / name
        root.mkdir(mode=0o700)
        evidence = self.evidence / name
        evidence.mkdir(mode=0o700)
        requests = evidence / "provider-requests"
        requests.mkdir()
        self.fixture = driver.Fixture("unselected", root / "workspace", requests)
        terminal = None
        result = {"case": name, "prompted": prompted, "state": None, "error": None}
        try:
            self.server, self.thread = driver.start_provider(self.fixture)
            driver.fixture_files(root, self.server.server_address[1])
            terminal = driver.Terminal(root, evidence, root / "workspace", root / "agent", root / "pig-home", root / "sessions", pig, root / "unused-extension", False, self.server.server_address[1])
            provider = driver.process_snapshot().get(os.getpid())
            if not provider:
                raise RuntimeError("The local provider process identity was not observed")
            provider["executable"] = driver.executable(os.getpid())
            self.event(name + ".fixture-start", process=provider, address=self.server.server_address)
            self.snapshot(name + "-provider-started")
            terminal.launch()
            target = {"window": terminal.window["id"], "session": terminal.window["session"], "tty": terminal.window["tty"], "process": terminal.window["wrapper_identity"]}
            self.owned.append(target)
            self.event(name + ".identity", target=target, host=terminal.window["host"], processes=list(terminal.owned.values()))
            nonce = "P03_PIG_" + os.urandom(12).hex()
            title = self.action(name + ".set-title", window_probe.SET_TITLE, target["window"], target["session"], nonce)
            verify_pig_title(title, nonce)
            self.event(name + ".title", nonce=nonce, session_and_window=title)
            self.check_control(name + ".control-before", control)
            check_fixture(self.fixture, prompted=False)
            self.snapshot(name + "-startup")
            if prompted:
                terminal.send("P03_UNSELECTED")
                terminal.await_marker("P03_UNSELECTED_DONE", self.fixture)
                check_fixture(self.fixture, prompted=True)
                self.event(name + ".prompt-complete", requests=self.fixture.requests, fixture_stage=self.fixture.stage)
                self.snapshot(name + "-prompt-complete")
            native_before = self.snapshot(name + "-before-exit")
            require_native_mapping(native_before, target, self.iterm["pid"])
            require_native_mapping(native_before, control, self.iterm["pid"])
            result["native_before"] = [record for record in native_before if record["number"] in (int(target["window"]), int(control["window"]))]
            self.event(name + ".native-mapping-confirmed", target=target["window"], control=control["window"], owner_pid=self.iterm["pid"])
            if not driver.idle_editor(terminal.session_action("text")):
                raise RuntimeError("PiG editor was not idle before exit")
            terminal.session_action("exit")
            generation = Path(terminal.window["generation_dir"])
            terminal.wait(lambda: (generation / "exit.txt").exists(), "PiG exit status", 30)
            exit_status = int((generation / "exit.txt").read_text().strip())
            self.event(name + ".exit-marker", status=exit_status)
            if exit_status != 0:
                raise RuntimeError(f"PiG exit marker was {exit_status}")
            terminal.wait(lambda: not any(driver.same_process(process) for process in terminal.owned.values()), "owned processes to exit", 30)
            self.event(name + ".processes-exited", processes=list(terminal.owned.values()))
            state = self.state(name + ".immediate-exit", target)
            self.event(name + ".immediate-result", state=state)
            native_after = self.snapshot(name + "-after-exit")
            require_native_mapping(native_after, control, self.iterm["pid"])
            result["native_after_exit"] = [record for record in native_after if record["number"] in (int(target["window"]), int(control["window"]))]
            if state == "present:changed":
                raise RuntimeError("PiG window session changed before close")
            if state != "missing":
                closed = self.action(name + ".guarded-close", driver.SESSION, target["window"], target["session"], "close")
                self.event(name + ".close-result", value=closed)
                state = self.observe_disappearance(target)
            result["state"] = state
            native_settled = self.snapshot(name + "-after-close")
            require_native_mapping(native_settled, control, self.iterm["pid"])
            result["native_after_settling"] = [record for record in native_settled if record["number"] in (int(target["window"]), int(control["window"]))]
            if state == "present:changed":
                raise RuntimeError("PiG window session changed after close")
            self.check_control(name + ".control-after", control)
            check_fixture(self.fixture, prompted)
        except Exception as error:
            result["error"] = repr(error)
            self.event(name + ".exception", error=repr(error))
            try:
                self.snapshot(name + "-exception")
            except Exception as capture_error:
                self.event(name + ".capture-exception", error=repr(capture_error))
        finally:
            if self.server:
                self.server.shutdown()
                self.server.server_close()
            if self.thread:
                self.thread.join(timeout=5)
            result["requests"] = self.fixture.requests
            result["provider_errors"] = self.fixture.errors
            result["provider_joined"] = not self.thread or not self.thread.is_alive()
            result["processes"] = list(terminal.owned.values()) if terminal else []
            self.event(name + ".fixture-stop", requests=result["requests"], errors=result["provider_errors"], joined=result["provider_joined"])
            (evidence / "result.json").write_text(json.dumps(result, indent=2) + "\n")
            self.server = None
            self.thread = None
            self.fixture = None
        return result


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--scratch", required=True, type=Path)
    parser.add_argument("--evidence", required=True, type=Path)
    args = parser.parse_args()
    args.evidence.mkdir(parents=True, exist_ok=True)
    if sys.platform != "darwin" or os.environ.get("PROBE_RUNNER_LABEL") != "macos-15-intel":
        raise RuntimeError("The PiG diagnostic requires the Intel macOS runner")
    machine = subprocess.run(["/usr/bin/uname", "-m"], capture_output=True, text=True, check=True).stdout.strip()
    if machine != "x86_64":
        raise RuntimeError("The PiG diagnostic requires an x86_64 machine")
    capability_path = Path(os.environ["RUNNER_TEMP"]) / "collaboration-platform-probe" / "result.json"
    capability = json.loads(capability_path.read_text())
    if any(capability.get(key) != value for key, value in (("status", "pass"), ("signature_status", "verified"), ("automation_status", "approved"))):
        raise RuntimeError("Signed iTerm capability and scoped consent are required")
    identity = driver.probe_identity(capability_path)
    tty = capability.get("session_tty", "")
    if identity is None or not re.fullmatch(r"/dev/ttys\d+", tty):
        raise RuntimeError("The control window has no attributable identity")
    pig = Path(os.environ["PIG_LITTER_EXPECTED_PIG"])
    expected_hash = os.environ["PIG_LITTER_EXPECTED_PIG_SHA256"]
    actual_hash = subprocess.run(["/usr/bin/shasum", "-a", "256", str(pig)], capture_output=True, text=True, check=True).stdout.split()[0]
    if actual_hash != expected_hash:
        raise RuntimeError("The prepared PiG host hash differs")
    diagnostic = PigForensics(args.evidence, args.scratch)
    diagnostic.capability_tty = tty
    control = {"window": identity[0], "session": identity[1], "tty": tty}
    diagnostic.control = control
    iterm_processes = [process for process in driver.process_snapshot().values() if process["command"].split(" ", 1)[0] == "/Applications/iTerm.app/Contents/MacOS/iTerm2"]
    if len(iterm_processes) != 1:
        raise RuntimeError("The iTerm process identity is ambiguous")
    diagnostic.iterm = iterm_processes[0]
    diagnostic.event("start", runner="macos-15-intel", machine=machine, pig=str(pig), pig_sha256=actual_hash, control=control, iterm=diagnostic.iterm)
    results = []
    driver.applescript = diagnostic.recorded_applescript
    try:
        if diagnostic.state("control.initial", control) != "present:owned":
            raise RuntimeError("The capability control is unavailable")
        control_processes = [process for process in driver.process_snapshot().values() if process["tty"] == tty.removeprefix("/dev/")]
        if not control_processes:
            raise RuntimeError("The capability control has no live TTY process")
        control["processes"] = control_processes
        diagnostic.event("control.processes", processes=control_processes)
        native_baseline = diagnostic.snapshot("baseline")
        require_native_mapping(native_baseline, control, diagnostic.iterm["pid"])
        diagnostic.event("control.native-mapping-confirmed", window=control["window"], owner_pid=diagnostic.iterm["pid"])
        for name, prompted in (("startup-only", False), ("deterministic-prompt", True)):
            result = diagnostic.run_case(name, pig, control, prompted)
            results.append(result)
            if result["error"] or not result["provider_joined"]:
                break
    except Exception as error:
        diagnostic.event("exception", error=repr(error))
        diagnostic.failures.append(repr(error))
    finally:
        try:
            diagnostic.snapshot("before-control-cleanup")
            closed = diagnostic.action("control.guarded-close", driver.SESSION, *identity, "close")
            diagnostic.event("control.close-result", value=closed)
            control_state = diagnostic.observe_disappearance(control, seconds=10)
            diagnostic.event("control.cleanup-result", state=control_state)
            diagnostic.snapshot("after-control-cleanup")
        except Exception as error:
            diagnostic.event("control.cleanup-exception", error=repr(error))
            diagnostic.failures.append(repr(error))
        try:
            final_hash = subprocess.run(["/usr/bin/shasum", "-a", "256", str(pig)], capture_output=True, text=True, check=True).stdout.split()[0]
            diagnostic.event("pig.final-hash", expected=expected_hash, actual=final_hash)
            if final_hash != expected_hash:
                diagnostic.failures.append("The prepared PiG host hash changed")
        except Exception as error:
            diagnostic.event("pig.hash-exception", error=repr(error))
            diagnostic.failures.append(repr(error))
        summary = {"result": "observed" if len(results) == 2 and not diagnostic.failures and all(not result["error"] and result["provider_joined"] for result in results) else "incomplete", "cases": results, "failures": diagnostic.failures}
        (args.evidence / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    if summary["result"] != "observed":
        raise RuntimeError("PiG diagnostic incomplete; inspect the event ledger")


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(f"Intel PiG window forensics failed: {error}", file=sys.stderr)
        sys.exit(1)
