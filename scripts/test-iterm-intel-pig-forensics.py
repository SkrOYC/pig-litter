#!/usr/bin/env python3
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import textwrap
import unittest
from types import SimpleNamespace
from unittest import mock


spec = importlib.util.spec_from_file_location("intel_pig_window_forensics", Path(__file__).with_name("iterm-intel-pig-forensics.py"))
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)


class PigForensicsTests(unittest.TestCase):
    def test_zero_tab_mode_uses_one_case_and_leaves_control_live(self):
        with tempfile.TemporaryDirectory(prefix="itp-main.") as directory:
            root = Path(directory)
            capability = root / "collaboration-platform-probe"
            capability.mkdir()
            (capability / "result.json").write_text(json.dumps({"status": "pass", "signature_status": "verified", "automation_status": "approved", "session_tty": "/dev/ttys001"}))
            evidence = root / "evidence"
            scratch = root / "scratch"
            scratch.mkdir()
            diagnostic = mock.Mock()
            diagnostic.failures = []
            diagnostic.state.return_value = "present:owned"
            diagnostic.snapshot.return_value = [{"number": 39, "owner_pid": 99}]

            def run_case(name, _pig, _control, prompted):
                (evidence / name).mkdir()
                return {"case": name, "prompted": prompted, "error": None, "provider_joined": True}

            diagnostic.run_case.side_effect = run_case
            processes = {99: {"pid": 99, "start": "iterm birth", "tty": "??", "command": "/Applications/iTerm.app/Contents/MacOS/iTerm2"}, 72: {"pid": 72, "start": "control birth", "tty": "ttys001", "command": "-bash"}}
            digest = "d" * 64
            env = {"RUNNER_TEMP": str(root), "PROBE_RUNNER_LABEL": "macos-15-intel", "PIG_LITTER_EXPECTED_PIG": "/pig", "PIG_LITTER_EXPECTED_PIG_SHA256": digest, "INTEL_ZERO_TAB_CLOSE": "1"}

            def command(argv, **_kwargs):
                return SimpleNamespace(stdout="x86_64\n" if argv[0].endswith("uname") else digest + "  /pig\n")

            with mock.patch.dict(os.environ, env), mock.patch.object(sys, "argv", ["probe", "--scratch", str(scratch), "--evidence", str(evidence)]), mock.patch.object(sys, "platform", "darwin"), mock.patch.object(probe.subprocess, "run", side_effect=command), mock.patch.object(probe.driver, "probe_identity", return_value=("39", "control")), mock.patch.object(probe.driver, "process_snapshot", return_value=processes), mock.patch.object(probe.driver, "applescript"), mock.patch.object(probe, "PigForensics", return_value=diagnostic), mock.patch.object(probe.iterm_zero_close, "probe", return_value={"state": "missing"}):
                probe.main()
            summary = json.loads((evidence / "summary.json").read_text())
            self.assertEqual((summary["result"], summary["mode"], summary["control_cleanup_state"]), ("observed", "zero-tab-close", "left-live"))
            self.assertEqual([(call.args[0], call.args[3]) for call in diagnostic.run_case.call_args_list], [("startup-only", False)])
            self.assertEqual(diagnostic.action.call_args_list, [])

    def test_native_log_report_preserves_census_and_missing_files(self):
        report_path = Path(__file__).with_name("report-intel-native.py")
        report_spec = importlib.util.spec_from_file_location("report_intel_native", report_path)
        reporter = importlib.util.module_from_spec(report_spec)
        report_spec.loader.exec_module(reporter)
        with tempfile.TemporaryDirectory(prefix="itp-report.") as directory:
            evidence = Path(directory)
            (evidence / "summary.json").write_text('{"result": "observed"}\n')
            (evidence / "startup-only-after-exit-native.json").write_text('{"windows": []}\n')
            output = io.StringIO()
            with contextlib.redirect_stdout(output):
                reporter.report(evidence)
            self.assertIn('--- summary.json ---\n{"result": "observed"}', output.getvalue())
            self.assertIn('--- startup-only-after-exit-native.json ---\n{"windows": []}', output.getvalue())
            self.assertIn('--- deterministic-prompt-after-exit-native.json ---\nunavailable', output.getvalue())

    def test_native_census_reads_window_numbers_and_owners(self):
        payload = probe.plistlib.dumps([{"kCGWindowNumber": 47, "kCGWindowOwnerPID": 100, "kCGWindowIsOnscreen": True}], fmt=probe.plistlib.FMT_BINARY)
        buffer = probe.ctypes.create_string_buffer(payload)
        released = []

        class Function:
            def __init__(self, value):
                self.value = value

            def __call__(self, *_args):
                return self.value() if callable(self.value) else self.value

        class Framework:
            pass

        graphics = Framework()
        graphics.CGWindowListCopyWindowInfo = Function(1)
        foundation = Framework()
        foundation.CFPropertyListCreateData = Function(2)
        foundation.CFDataGetLength = Function(len(payload))
        foundation.CFDataGetBytePtr = Function(probe.ctypes.addressof(buffer))
        foundation.CFRelease = Function(lambda: released.append(True))
        with mock.patch.object(probe.ctypes, "CDLL", side_effect=[graphics, foundation]):
            self.assertEqual(probe.native_windows(), [{"number": 47, "owner_pid": 100, "onscreen": True}])
        self.assertEqual(len(released), 2)

    def test_native_mapping_requires_the_exact_window_and_owner(self):
        records = [{"number": 47, "owner_pid": 100, "onscreen": True}, {"number": 41, "owner_pid": 100, "onscreen": True}]
        probe.require_native_mapping(records, {"window": "47"}, 100)
        probe.require_native_mapping(records, {"window": "41"}, 100)
        for record_set, target, owner in ((records, "50", 100), (records, "47", 999), (records + [records[0]], "47", 100)):
            with self.subTest(target=target, owner=owner), self.assertRaisesRegex(RuntimeError, "native owner"):
                probe.require_native_mapping(record_set, {"window": target}, owner)

    def test_pig_window_title_may_differ_from_unique_session_nonce(self):
        nonce = "P03_PIG_123"
        self.assertIsNone(probe.verify_pig_title("P03_PIG_123 (pig)\npig - workspace\n1", nonce))
        self.assertIsNone(probe.verify_pig_title("P03_PIG_123 (pig)\npig - workspace\n2", nonce))
        for response in ("other (pig)\npig - workspace\n1", "P03_PIG_123 (pig)\n\n1", "P03_PIG_123 (pig)\npig - workspace\n0"):
            with self.subTest(response=response), self.assertRaises(RuntimeError):
                probe.verify_pig_title(response, nonce)

    def test_startup_and_prompt_use_separate_provider_observations(self):
        with tempfile.TemporaryDirectory(prefix="itp-test.") as directory:
            root = Path(directory)
            startup = probe.driver.Fixture("unselected", root / "workspace", root)
            probe.check_fixture(startup, prompted=False)
            with self.assertRaisesRegex(RuntimeError, "did not reach"):
                probe.check_fixture(startup, prompted=True)
            prompt = probe.driver.Fixture("unselected", root / "workspace", root)
            tools = [{"function": {"name": name}} for name in ("read", "write", "edit", "bash")]
            request = {"model": "parent", "messages": [{"role": "user", "content": "P03_UNSELECTED"}], "tools": tools}
            response = prompt.response(request).decode()
            self.assertIn("P03_UNSELECTED_DONE", response)
            self.assertEqual(prompt.requests, 1)
            probe.check_fixture(prompt, prompted=True)
            with self.assertRaisesRegex(RuntimeError, "startup-only"):
                probe.check_fixture(prompt, prompted=False)

    def test_provider_errors_are_not_accepted(self):
        with tempfile.TemporaryDirectory(prefix="itp-test.") as directory:
            fixture = probe.driver.Fixture("unselected", Path(directory), Path(directory))
            fixture.errors.append("fixture failed")
            with self.assertRaisesRegex(RuntimeError, "fixture failed"):
                probe.check_fixture(fixture, prompted=False)

    def test_separate_cases_record_zero_and_one_fixture_requests(self):
        with tempfile.TemporaryDirectory(prefix="itp-test.") as directory:
            root = Path(directory)
            evidence = root / "evidence"
            evidence.mkdir()
            scratch = root / "scratch"
            scratch.mkdir()
            diagnostic = probe.PigForensics(evidence, scratch)
            diagnostic.iterm = {"pid": 90, "start": "iterm birth"}
            control_process = {"pid": 72, "start": "control birth", "tty": "ttys001"}
            control = {"window": "40", "session": "control", "tty": "/dev/ttys001", "processes": [control_process]}

            class Server:
                server_address = ("127.0.0.1", 12345)

                def shutdown(self):
                    pass

                def server_close(self):
                    pass

            class Thread:
                def join(self, timeout):
                    self.timeout = timeout

                def is_alive(self):
                    return False

            class Terminal:
                def __init__(self, case_root, *_args):
                    self.root = case_root
                    self.owned = {}
                    self.window = None

                def launch(self):
                    generation = self.root / "generation-1"
                    generation.mkdir()
                    process = {"pid": 81, "start": "wrapper birth", "tty": "ttys002"}
                    self.owned = {"wrapper": process}
                    self.window = {"id": "41", "session": "target", "tty": "/dev/ttys002", "wrapper_identity": process, "host": process, "generation_dir": str(generation)}

                def send(self, message):
                    tools = [{"function": {"name": name}} for name in ("read", "write", "edit", "bash")]
                    diagnostic.fixture.response({"model": "parent", "messages": [{"role": "user", "content": message}], "tools": tools})

                def await_marker(self, marker, fixture):
                    if marker != "P03_UNSELECTED_DONE" or fixture.stage != 1:
                        raise RuntimeError("Missing deterministic response")

                def session_action(self, action):
                    if action == "exit":
                        (Path(self.window["generation_dir"]) / "exit.txt").write_text("0\n")
                    return "ready"

                def wait(self, predicate, _label, _timeout):
                    if not predicate():
                        raise RuntimeError("Condition did not settle")

            def state(stage, _identity):
                return "present:owned" if ".control-" in stage else "missing"

            def action(stage, _script, *_arguments):
                if stage.endswith(".set-title"):
                    nonce = _arguments[-1]
                    return nonce + " (pig)\npig - workspace\n1"
                raise RuntimeError("Unexpected action")

            process = {"pid": 72, "start": "control birth", "tty": "ttys001"}

            def snapshot(stage):
                control_window = {"number": 40, "owner_pid": 90}
                if stage.endswith(("-after-exit", "-after-close")):
                    return [control_window]
                return [control_window, {"number": 41, "owner_pid": 90}]

            with mock.patch.object(probe.driver, "start_provider", return_value=(Server(), Thread())), mock.patch.object(probe.driver, "Terminal", Terminal), mock.patch.object(probe.driver, "process_snapshot", return_value={72: process, os.getpid(): {"pid": os.getpid(), "start": "provider birth", "tty": "??"}}), mock.patch.object(probe.driver, "executable", return_value="/python"), mock.patch.object(probe.driver, "same_process", return_value=False), mock.patch.object(probe.driver, "idle_editor", return_value=True), mock.patch.object(diagnostic, "snapshot", side_effect=snapshot), mock.patch.object(diagnostic, "state", side_effect=state), mock.patch.object(diagnostic, "action", side_effect=action):
                first = diagnostic.run_case("startup-only", Path("/pig"), control, prompted=False)
                second = diagnostic.run_case("deterministic-prompt", Path("/pig"), control, prompted=True)
            self.assertEqual((first["state"], first["requests"], first["error"]), ("missing", 0, None))
            self.assertEqual((second["state"], second["requests"], second["error"]), ("missing", 1, None))
            self.assertTrue(first["provider_joined"] and second["provider_joined"])
            self.assertEqual(first["native_before"], [{"number": 40, "owner_pid": 90}, {"number": 41, "owner_pid": 90}])
            self.assertEqual(first["native_after_exit"], [{"number": 40, "owner_pid": 90}])
            self.assertEqual(second["native_after_settling"], [{"number": 40, "owner_pid": 90}])
            self.assertTrue((evidence / "startup-only/result.json").is_file())
            self.assertTrue((evidence / "deterministic-prompt/result.json").is_file())

    def test_mac_identity_and_disk_guards_stop_before_scratch_allocation(self):
        workflow = Path(__file__).resolve().parents[1] / ".github/workflows/collaboration-platform-probe.yml"
        job = workflow.read_text().split("  intel-pig-window-forensics:\n", 1)[1]

        def script_for(step):
            after = job.split("      - name: " + step + "\n", 1)[1].split("        run: |\n", 1)[1]
            lines = []
            for line in after.splitlines():
                if line and not line.startswith("          "):
                    break
                lines.append(line)
            return textwrap.dedent("\n".join(lines)) + "\n"

        identity = script_for("Record and check diagnostic inputs")
        prepare = script_for("Prepare the pinned native host and observe unselected PiG")
        for line in (identity + prepare).splitlines():
            condition = line.strip()
            if condition.startswith(("test ", "[[ ")):
                self.assertTrue(condition.endswith("|| exit 1"), condition)
        shell = os.environ.get("BASH_FOR_TESTS", "/bin/bash")
        with tempfile.TemporaryDirectory(prefix="itp-guards.") as directory:
            root = Path(directory)
            bin_dir = root / "bin"
            bin_dir.mkdir()
            (root / "driver").mkdir()
            candidate = "a" * 40
            git = bin_dir / "git"
            git.write_text("#!/bin/sh\ncase \"$*\" in *status*) exit 0;; *HEAD*) printf '%s\\n' \"${FAKE_GIT_HEAD}\";; esac\n")
            git.chmod(0o755)
            df = bin_dir / "df"
            df.write_text("#!/bin/sh\nprintf 'Filesystem Blocks Used Available Capacity Mounted\\n%s 10 1 9 10%% /private/tmp\\n' \"$FAKE_DEVICE\"\n")
            df.chmod(0o755)
            mktemp = bin_dir / "mktemp"
            mktemp.write_text("#!/bin/sh\nprintf reached > \"$SCRATCH_REACHED\"\nexit 1\n")
            mktemp.chmod(0o755)
            env = os.environ | {"PATH": str(bin_dir) + os.pathsep + os.environ["PATH"], "RUNNER_TEMP": str(root), "CANDIDATE_ROOT": str(root / "candidate"), "CANDIDATE_SHA": candidate, "FAKE_GIT_HEAD": candidate, "FAKE_DEVICE": "/dev/disk1", "SCRATCH_REACHED": str(root / "scratch-reached"), "SELECTED_SCENARIO": "intel-pig-window-forensics", "SELECTED_ARCHITECTURE": "intel", "GITHUB_WORKFLOW_SHA": "b" * 40, "PROBE_RUNNER_LABEL": "macos-15-intel"}
            marker = root / "after-guard"
            valid = subprocess.run([shell, "-e", "-o", "pipefail", "-c", identity + "\nprintf reached > \"$AFTER_GUARD\"\n"], cwd=root, env=env | {"AFTER_GUARD": str(marker)}, capture_output=True, text=True)
            self.assertEqual(valid.returncode, 0, valid.stderr)
            self.assertTrue(marker.exists())
            valid_disk = "cd() { return 0; }\npwd() { printf '/private/tmp\\n'; }\n"
            reached = subprocess.run([shell, "-e", "-o", "pipefail", "-c", valid_disk + prepare], cwd=root, env=env, capture_output=True, text=True)
            self.assertNotEqual(reached.returncode, 0)
            self.assertTrue((root / "scratch-reached").exists())
            (root / "scratch-reached").unlink()
            cases = [
                (identity, {"SELECTED_ARCHITECTURE": "arm"}, ""),
                (identity, {"CANDIDATE_SHA": "invalid"}, ""),
                (prepare, {"FAKE_GIT_HEAD": "c" * 40}, ""),
                (prepare, {"SELECTED_SCENARIO": "invalid"}, ""),
                (prepare, {"FAKE_DEVICE": "tmpfs"}, ""),
                (prepare, {}, "cd() { return 0; }\n"),
            ]
            for script, changes, prefix in cases:
                with self.subTest(changes=changes, prefix=prefix):
                    marker = root / "after-guard"
                    marker.unlink(missing_ok=True)
                    result = subprocess.run([shell, "-e", "-o", "pipefail", "-c", prefix + script + "\nprintf reached > \"$AFTER_GUARD\"\n"], cwd=root, env=env | changes | {"AFTER_GUARD": str(marker)}, capture_output=True, text=True)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertFalse(marker.exists())
                    self.assertFalse((root / "scratch-reached").exists())

    def test_pig_diagnostic_has_an_exclusive_workflow_selector(self):
        workflow = Path(__file__).resolve().parents[1] / ".github/workflows/collaboration-platform-probe.yml"
        text = workflow.read_text()
        product = text.split("  p03-macos:\n", 1)[1].split("\n  iterm-lifecycle:\n", 1)[0]
        self.assertNotIn("intel-pig-window-forensics", product)
        diagnostic = text.split("  intel-pig-window-forensics:\n", 1)[1]
        self.assertIn("contains(fromJSON('[\"intel-pig-window-forensics\",\"intel-zero-tab-close\"]'), inputs.scenario)", diagnostic)
        self.assertNotIn("intel-zero-tab-close", product)
        self.assertIn("runs-on: macos-15-intel", diagnostic)
        self.assertIn('test "$SELECTED_ARCHITECTURE" = intel', diagnostic)
        self.assertIn('bash "$DRIVER_ROOT/scripts/verify-pig-platforms.sh"', diagnostic)
        self.assertIn("report-intel-native.py", diagnostic)
        self.assertIn("steps.upload_native_evidence.outcome == 'failure'", diagnostic)
        self.assertNotIn("verify-p03-macos.sh", diagnostic)


if __name__ == "__main__":
    unittest.main()
