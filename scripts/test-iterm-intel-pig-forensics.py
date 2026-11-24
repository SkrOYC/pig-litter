#!/usr/bin/env python3
import importlib.util
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock


spec = importlib.util.spec_from_file_location("intel_pig_window_forensics", Path(__file__).with_name("iterm-intel-pig-forensics.py"))
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)


class PigForensicsTests(unittest.TestCase):
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
                    return nonce + " (pig)\n" + nonce + "\n1"
                raise RuntimeError("Unexpected action")

            process = {"pid": 72, "start": "control birth", "tty": "ttys001"}
            with mock.patch.object(probe.driver, "start_provider", return_value=(Server(), Thread())), mock.patch.object(probe.driver, "Terminal", Terminal), mock.patch.object(probe.driver, "process_snapshot", return_value={72: process, os.getpid(): {"pid": os.getpid(), "start": "provider birth", "tty": "??"}}), mock.patch.object(probe.driver, "executable", return_value="/python"), mock.patch.object(probe.driver, "same_process", return_value=False), mock.patch.object(probe.driver, "idle_editor", return_value=True), mock.patch.object(diagnostic, "snapshot"), mock.patch.object(diagnostic, "state", side_effect=state), mock.patch.object(diagnostic, "action", side_effect=action):
                first = diagnostic.run_case("startup-only", Path("/pig"), control, prompted=False)
                second = diagnostic.run_case("deterministic-prompt", Path("/pig"), control, prompted=True)
            self.assertEqual((first["state"], first["requests"], first["error"]), ("missing", 0, None))
            self.assertEqual((second["state"], second["requests"], second["error"]), ("missing", 1, None))
            self.assertTrue(first["provider_joined"] and second["provider_joined"])
            self.assertTrue((evidence / "startup-only/result.json").is_file())
            self.assertTrue((evidence / "deterministic-prompt/result.json").is_file())

    def test_pig_diagnostic_has_an_exclusive_workflow_selector(self):
        workflow = Path(__file__).resolve().parents[1] / ".github/workflows/collaboration-platform-probe.yml"
        text = workflow.read_text()
        product = text.split("  p03-macos:\n", 1)[1].split("\n  iterm-lifecycle:\n", 1)[0]
        self.assertNotIn("intel-pig-window-forensics", product)
        diagnostic = text.split("  intel-pig-window-forensics:\n", 1)[1]
        self.assertIn("inputs.scenario == 'intel-pig-window-forensics'", diagnostic)
        self.assertIn("runs-on: macos-15-intel", diagnostic)
        self.assertIn('test "$SELECTED_ARCHITECTURE" = intel', diagnostic)
        self.assertIn('bash "$DRIVER_ROOT/scripts/verify-pig-platforms.sh"', diagnostic)
        self.assertNotIn("verify-p03-macos.sh", diagnostic)


if __name__ == "__main__":
    unittest.main()
