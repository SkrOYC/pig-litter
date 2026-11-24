import importlib.util
import json
from pathlib import Path
import shlex
import sys
import tempfile
import unittest
from unittest import mock


spec = importlib.util.spec_from_file_location("iterm_lifecycle", Path(__file__).with_name("iterm-lifecycle.py"))
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)


class LifecycleTests(unittest.TestCase):
    def test_three_windows_preserve_identity_and_observe_exit(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            evidence = root / "evidence"
            scratch = root / "scratch"
            evidence.mkdir()
            scratch.mkdir()
            diagnostic = probe.Diagnostic(evidence, scratch)
            windows = {}
            snapshots = []

            def apple(stage, script, *arguments, timeout=30):
                if script == probe.driver.CREATE:
                    number = len(windows) + 1
                    window = str(40 + number)
                    windows[window] = {"number": number, "alive": True, "visible": True}
                    return {"returncode": 0, "stdout": f"session-{number}\n/dev/ttys00{number}\n{window}\n", "stderr": ""}
                if script == probe.WINDOW_EXISTS:
                    return {"returncode": 0, "stdout": "present\n" if windows[arguments[0]]["visible"] else "missing\n", "stderr": ""}
                window, session, action = arguments[:3]
                actor = windows[window]
                number = actor["number"]
                self.assertEqual(session, f"session-{number}")
                generation = scratch / f"window-{number}"
                if action == "send":
                    self.assertEqual(Path(shlex.split(arguments[3])[-1]).parent, generation)
                    for name, value in {"pid": str(100 + number), "tty": f"/dev/ttys00{number}", "process": f"{100 + number} 1 ttys00{number} Tue Nov 24 12:00:00 2026 /bin/sh launch.sh", "sentinel": f"ITERM_LIFECYCLE_{number}_local"}.items():
                        (generation / name).write_text(value + "\n")
                    if number == 1:
                        (generation / "exit").write_text("0\n")
                        actor["alive"] = False
                    output = "sent"
                elif action == "text":
                    output = f"ITERM_LIFECYCLE_{number}_local"
                elif action == "exit":
                    (generation / "exit").write_text("0\n")
                    actor["alive"] = False
                    output = "sent exit key"
                elif action == "close":
                    self.assertFalse(actor["alive"])
                    actor["visible"] = False
                    output = "closed"
                else:
                    self.fail(action)
                return {"returncode": 0, "stdout": output + "\n", "stderr": ""}

            def processes():
                return {100 + actor["number"]: {"start": "Tue Nov 24 12:00:00 2026"} for actor in windows.values() if actor["alive"]}

            with mock.patch.object(diagnostic, "apple", side_effect=apple), mock.patch.object(diagnostic, "snapshot", side_effect=snapshots.append), mock.patch.object(diagnostic, "screenshot", side_effect=snapshots.append), mock.patch.object(probe.driver, "process_snapshot", side_effect=processes), mock.patch.dict(probe.os.environ, {"GITHUB_RUN_ID": "local"}):
                for number in range(1, 4):
                    diagnostic.run_window(number)
            self.assertEqual(diagnostic.failures, [])
            self.assertEqual([item["window"] for item in diagnostic.owned], ["41", "42", "43"])
            self.assertTrue(all(item["disappeared"] for item in diagnostic.owned))
            self.assertTrue(all(f"window-{number}-before-exit" in snapshots and f"window-{number}-after-close" in snapshots for number in range(1, 4)))
            events = [json.loads(line) for line in diagnostic.events.read_text().splitlines()]
            self.assertEqual([event["pid"] for event in events if event["stage"].endswith("process-exited")], [101, 102, 103])

    def test_timeout_preserves_output_and_does_not_claim_success(self):
        with tempfile.TemporaryDirectory() as temp:
            diagnostic = probe.Diagnostic(Path(temp), Path(temp))
            result = diagnostic.command("slow", [sys.executable, "-c", "import time; print('ready', flush=True); time.sleep(5)"], timeout=0.2)
            self.assertTrue(result["timeout"])
            self.assertEqual(result["stdout"], "ready\n")
            events = [json.loads(line) for line in diagnostic.events.read_text().splitlines()]
            self.assertEqual([event["stage"] for event in events], ["slow.start", "slow.end"])
            self.assertTrue(events[-1]["timeout"])

    def test_cleanup_does_not_close_a_window_with_unverified_process(self):
        with tempfile.TemporaryDirectory() as temp:
            diagnostic = probe.Diagnostic(Path(temp), Path(temp))
            diagnostic.owned.append({"window": "41", "session": "owned", "command_sent": True})
            with mock.patch.object(diagnostic, "action") as action, mock.patch.object(diagnostic, "snapshot"):
                diagnostic.cleanup()
            action.assert_not_called()
            self.assertIn("cleanup-41.unverified-process", diagnostic.failures)


if __name__ == "__main__":
    unittest.main()
