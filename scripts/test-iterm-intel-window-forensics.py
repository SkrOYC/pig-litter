#!/usr/bin/env python3
import importlib.util
import json
from pathlib import Path
import re
import tempfile
import unittest
from unittest import mock


spec = importlib.util.spec_from_file_location("intel_window_forensics", Path(__file__).with_name("iterm-intel-window-forensics.py"))
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)


class ForensicsTests(unittest.TestCase):
    def test_state_requires_an_unambiguous_successful_response(self):
        self.assertEqual(probe.parse_state({"returncode": 0, "stdout": "present:missing\n"}), "present:missing")
        self.assertEqual(probe.parse_state({"returncode": 0, "stdout": "missing\n"}), "missing")
        for response in ({"returncode": 1, "stdout": "missing\n"}, {"returncode": 0, "stdout": ""}, {"returncode": 0, "stdout": "present\n"}):
            with self.subTest(response=response), self.assertRaises(RuntimeError):
                probe.parse_state(response)

    def test_missing_session_is_reported_as_a_still_present_window(self):
        self.run_simulation("present:missing", "present:missing", True)

    def test_natural_window_disappearance_skips_close(self):
        self.run_simulation("missing", "missing", False)

    def test_changed_session_refuses_the_close(self):
        self.run_simulation("present:changed", None, False)

    def run_simulation(self, natural_state, expected_state, should_close):
        with tempfile.TemporaryDirectory(prefix="itf-test.") as directory:
            scratch = Path(directory)
            diagnostic = probe.Forensics(scratch, scratch)
            diagnostic.capability_tty = "/dev/ttys002"
            snapshots = []
            actions = []
            nonce = "P03_INTEL_" + "a" * 24
            control = {"pid": 40, "ppid": 1, "tty": "ttys002", "start": "control birth", "command": "shell"}
            target = {"pid": 123, "ppid": 1, "tty": "ttys003", "start": "target birth", "command": "sh"}
            snapshots_by_time = [{40: control}, {40: control, 123: target}, {40: control}]

            def action(stage, _script, *arguments, **_kwargs):
                actions.append((stage, arguments))
                if stage == "target.create":
                    return "owned-session\n/dev/ttys003\n51"
                if stage == "target.send":
                    return "sent"
                if stage == "target.set-title":
                    return nonce + "\n" + nonce + "\n1"
                if stage == "target.text":
                    return nonce
                if stage == "target.guarded-close":
                    return "session missing"
                self.fail(f"Unexpected action {stage}")

            def wait_file(path, _stage, **_kwargs):
                return {"pid": "123", "tty": "/dev/ttys003", "ready": nonce, "exit": "0"}[path.name]

            with mock.patch.object(diagnostic, "action", side_effect=action), mock.patch.object(diagnostic, "snapshot", side_effect=snapshots.append), mock.patch.object(diagnostic, "wait_file", side_effect=wait_file), mock.patch.object(diagnostic, "state", return_value="present:owned"), mock.patch.object(diagnostic, "wait_transition", return_value=natural_state), mock.patch.object(probe.driver, "process_snapshot", side_effect=snapshots_by_time), mock.patch.object(probe.secrets, "token_hex", return_value="a" * 24):
                if expected_state is None:
                    with self.assertRaisesRegex(RuntimeError, "refusing to close"):
                        diagnostic.run(("40", "control-session"))
                else:
                    self.assertEqual(diagnostic.run(("40", "control-session")), expected_state)
            close_actions = [args for stage, args in actions if stage == "target.guarded-close"]
            expected_actions = [("51", "owned-session", "close")] if should_close else []
            self.assertEqual(close_actions, expected_actions)
            if expected_state is not None:
                self.assertEqual(snapshots, ["baseline", "before-exit", "after-shell-exit", "after-natural-exit", "after-guarded-close"])
                events = [json.loads(line) for line in diagnostic.events.read_text().splitlines()]
                final = next(event for event in events if event["stage"] == "target.final-state")
                self.assertEqual(final["state"], expected_state)

    def test_diagnostic_selector_is_excluded_from_product_scenarios(self):
        workflow = Path(__file__).resolve().parents[1] / ".github/workflows/collaboration-platform-probe.yml"
        text = workflow.read_text()
        product = text.split("  p03-macos:\n", 1)[1].split("\n  iterm-lifecycle:\n", 1)[0]
        selector = re.search(r"if: \$\{\{ github\.event_name == 'workflow_dispatch' && contains\(fromJSON\('([^']+)'\), inputs\.scenario\) \}\}", product)
        self.assertIsNotNone(selector)
        allowed = json.loads(selector.group(1))
        self.assertEqual(allowed, ["p03-all", "p03-team", "p03-policy", "p03-unselected"])
        self.assertNotIn("intel-window-forensics", allowed)
        diagnostic = text.split("  intel-window-forensics:\n", 1)[1]
        self.assertIn("inputs.scenario == 'intel-window-forensics'", diagnostic)
        self.assertIn("runs-on: macos-15-intel", diagnostic)
        self.assertIn('test "$SELECTED_ARCHITECTURE" = intel', diagnostic)
        self.assertNotIn("verify-p03-macos.sh", diagnostic)


if __name__ == "__main__":
    unittest.main()
