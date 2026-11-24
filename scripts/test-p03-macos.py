import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock
from urllib.error import HTTPError
from urllib.request import Request, urlopen


spec = importlib.util.spec_from_file_location("p03_macos", Path(__file__).with_name("p03-macos.py"))
driver = importlib.util.module_from_spec(spec)
spec.loader.exec_module(driver)


class InputAndProviderTests(unittest.TestCase):
    def test_probe_window_stays_open_across_failed_cases(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            probe_dir = root / "collaboration-platform-probe"
            probe_dir.mkdir()
            (probe_dir / "result.json").write_text(json.dumps({"status": "pass", "signature_status": "verified", "automation_status": "approved", "window_id": "123", "session_id": "probe-session"}))
            events = []

            def applescript(_script, *arguments):
                self.assertEqual(arguments[:2], ("123", "probe-session"))
                events.append(arguments[2])
                return {"exists": "present", "close": "closed"}[arguments[2]]

            def run_case(name, *_args):
                events.append(name)
                if name == "policy":
                    raise RuntimeError("policy failed")

            arguments = ["p03-macos.py", "--scenario", "p03-all", "--pig", "/pig", "--extension", "/extension", "--scratch", str(root), "--evidence", str(root / "evidence")]
            with mock.patch.dict(os.environ, {"RUNNER_TEMP": str(root)}), mock.patch.object(driver.sys, "platform", "darwin"), mock.patch.object(driver.sys, "argv", arguments), mock.patch.object(driver, "applescript", applescript), mock.patch.object(driver, "run_case", run_case):
                with self.assertRaisesRegex(RuntimeError, "policy failed"):
                    driver.main()
            self.assertEqual(events, ["exists", "team", "policy", "unselected", "close"])
            self.assertEqual(json.loads((root / "evidence/summary.json").read_text())["result"], "failed")
            self.assertFalse((root / "evidence/cleanup-ok").exists())

    def test_exit_waits_for_owned_process_before_closing_window(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            generation = root / "generation-1"
            generation.mkdir()
            process = subprocess.Popen(["/bin/sh", "-c", "read -r line"], stdin=subprocess.PIPE, text=True)
            released = False
            terminal = driver.Terminal(root, root, root, root, root, root, root / "pig", root / "extension", True, 1)
            terminal.window = {"id": "42", "session": "owned", "generation_dir": str(generation), "wrapper_pid": process.pid}
            terminal.owned = {"wrapper": {"pid": process.pid}}

            def same_process(_identity):
                nonlocal released
                if process.poll() is not None:
                    return False
                if not released:
                    process.stdin.write("done\n")
                    process.stdin.flush()
                    released = True
                return True

            def session_action(action, _value=None):
                if action == "text":
                    return "─" * 30 + "\n\n" + "─" * 30 + "\nparent"
                if action == "exit":
                    (generation / "exit.txt").write_text("0\n")
                    return "sent"
                if action == "close":
                    self.assertIsNotNone(process.poll(), "The wrapper was still running when its window closed")
                    return "closed"
                return "missing"

            try:
                with mock.patch.object(driver, "same_process", same_process), mock.patch.object(terminal, "observe"), mock.patch.object(terminal, "session_action", session_action):
                    terminal.exit_host()
                self.assertTrue(released)
                self.assertIsNone(terminal.window)
            finally:
                if process.poll() is None:
                    process.terminate()
                process.wait(timeout=5)
                process.stdin.close()

    def test_wrong_candidate_never_runs_the_prepared_command(self):
        with tempfile.TemporaryDirectory() as temp:
            evidence = Path(temp) / "evidence"
            marker = Path(temp) / "ran"
            repo = Path(__file__).resolve().parent.parent
            environment = os.environ | {"PIG_PLATFORM_ROOT_DIR": str(repo)}
            result = subprocess.run(
                ["bash", "-x", str(repo / "scripts/verify-pig-platforms.sh"), "0" * 40, "linux", "amd64", str(evidence), "--", "bash", "-c", 'printf ran > "$1"', "callback", str(marker)],
                env=environment, capture_output=True, text=True, check=False,
            )
            result_file = evidence / "result.txt"
            recorded_result = result_file.read_text() if result_file.exists() else "<missing>"
            details = f"stdout:\n{result.stdout}\nstderr:\n{result.stderr}\nresult.txt:\n{recorded_result}"
            self.assertEqual(result.returncode, 1, details)
            self.assertFalse(marker.exists(), details)
            self.assertEqual(recorded_result, "exit_code=1\n", details)

    def test_wrong_runner_stops_before_creating_build_scratch(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            candidate = root / "candidate"
            candidate.mkdir()
            subprocess.run(["git", "init", "-q", str(candidate)], check=True, capture_output=True)
            subprocess.run(["git", "-C", str(candidate), "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null", "commit", "-q", "--allow-empty", "-m", "fixture"], check=True, capture_output=True)
            sha = subprocess.run(["git", "-C", str(candidate), "rev-parse", "HEAD"], check=True, capture_output=True, text=True).stdout.strip()
            evidence = root / "evidence"
            marker = root / "ran"
            repo = Path(__file__).resolve().parent.parent
            environment = os.environ | {"PIG_PLATFORM_ROOT_DIR": str(candidate), "PIG_PLATFORM_SCRATCH_PARENT": str(root), "RUNNER_OS": "unexpected", "RUNNER_ARCH": "X64"}
            result = subprocess.run(["bash", "-x", str(repo / "scripts/verify-pig-platforms.sh"), sha, "linux", "amd64", str(evidence), "--", "touch", str(marker)], env=environment, capture_output=True, text=True, check=False)
            result_file = evidence / "result.txt"
            recorded_result = result_file.read_text() if result_file.exists() else "<missing>"
            details = f"stdout:\n{result.stdout}\nstderr:\n{result.stderr}\nresult.txt:\n{recorded_result}"
            self.assertEqual(result.returncode, 1, details)
            self.assertEqual(recorded_result, "exit_code=1\n", details)
            self.assertFalse(marker.exists(), details)
            self.assertEqual(list(root.glob("pig-platform.*")), [], details)

    def test_macos_driver_rejects_wrong_candidate_before_scratch(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            repo = Path(__file__).resolve().parent.parent
            environment = os.environ | {"CANDIDATE_SHA": "0" * 40, "CANDIDATE_ROOT": str(repo), "DRIVER_ROOT": str(repo), "EVIDENCE_DIR": str(root), "P03_SCENARIO": "p03-all"}
            result = subprocess.run(["bash", "-x", str(repo / "scripts/verify-p03-macos.sh")], env=environment, capture_output=True, text=True, check=False)
            details = f"stdout:\n{result.stdout}\nstderr:\n{result.stderr}"
            self.assertEqual(result.returncode, 1, details)
            self.assertIn("verification check failed at line", result.stderr)
            self.assertFalse((root / "scratch.txt").exists(), details)

    def test_terminal_text_accepts_quotes_and_rejects_control_characters(self):
        self.assertEqual(driver.safe_text("a path with 'quotes' and $(literal)"), "a path with 'quotes' and $(literal)")
        for value in ("one\ntwo", "one\x1btwo", "one\x7ftwo", "one\x85two"):
            with self.subTest(value=value), self.assertRaisesRegex(RuntimeError, "Control character"):
                driver.safe_text(value)

    def test_unselected_provider_checks_the_registered_tools(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            fixture = driver.Fixture("unselected", root, root)
            server, thread = driver.start_provider(fixture)
            try:
                url = f"http://127.0.0.1:{server.server_address[1]}/v1/chat/completions"
                request = {"model": "parent", "messages": [{"role": "user", "content": "P03_UNSELECTED"}], "tools": [{"function": {"name": name}} for name in ("read", "write", "edit", "bash")]}
                def send():
                    return urlopen(Request(url, data=json.dumps(request).encode(), headers={"Content-Type": "application/json"}), timeout=5)
                with send() as response:
                    self.assertIn(b"P03_UNSELECTED_DONE", response.read())
                request["tools"].append({"function": {"name": "team_spawn"}})
                with self.assertRaises(HTTPError) as failure:
                    send()
                self.assertEqual(failure.exception.code, 500)
                self.assertEqual(fixture.errors, ["Unselected session registered Pig Litter tools"])
            finally:
                server.shutdown()
                server.server_close()
                thread.join(timeout=5)
            self.assertFalse(thread.is_alive())


class WindowClosureTests(unittest.TestCase):
    def terminal(self, root):
        terminal = driver.Terminal(root, root, root, root, root, root, root / "pig", root / "extension", True, 1)
        window = {"id": "42", "session": "owned-session", "wrapper_pid": 100, "launch_sent": True}
        terminal.window = window
        terminal.generations.append(window)
        terminal.owned = {"root": {"pid": 100, "start": "birth", "executable": "/bin/sh"}}
        return terminal, window

    def test_warning_is_acknowledged_after_exit_and_window_disappears(self):
        with tempfile.TemporaryDirectory() as temp:
            terminal, window = self.terminal(Path(temp))
            observations = iter(("present", "missing"))

            def applescript(script, *arguments):
                if script == driver.SESSION:
                    self.assertEqual(arguments, ("42", "owned-session", "close"))
                    return "closed"
                if script == driver.WINDOW_EXISTS:
                    self.assertEqual(arguments, ("42",))
                    return next(observations)
                self.assertEqual(script, driver.ACKNOWLEDGE_WARNING)
                self.assertEqual(arguments, ("42", "owned-session"))
                return "acknowledged owned short-session warning on pig"

            with mock.patch.object(driver, "applescript", side_effect=applescript), mock.patch.object(driver, "same_process", return_value=False), mock.patch.object(terminal, "observe"), mock.patch.object(terminal, "capture_closure"), mock.patch.object(driver.time, "sleep"):
                terminal.close_window()
            self.assertIsNone(terminal.window)
            self.assertEqual(window["close_result"], "closed")
            self.assertEqual(window["warning_results"][-1]["result"], "acknowledged owned short-session warning on pig")
            self.assertEqual(window["presence_results"][-1]["result"], "missing")

    def test_missing_session_remains_pending_until_the_window_disappears(self):
        with tempfile.TemporaryDirectory() as temp:
            terminal, window = self.terminal(Path(temp))
            observations = iter(("present", "present", "missing"))

            def applescript(script, *arguments):
                if script == driver.SESSION:
                    return "session missing"
                if script == driver.WINDOW_EXISTS:
                    return next(observations)
                return "owned current session missing"

            with mock.patch.object(driver, "applescript", side_effect=applescript), mock.patch.object(driver, "same_process", return_value=False), mock.patch.object(terminal, "observe"), mock.patch.object(terminal, "capture_closure"), mock.patch.object(driver.time, "sleep"):
                terminal.close_window()
            self.assertIsNone(terminal.window)
            self.assertEqual(window["close_result"], "session missing")
            self.assertEqual([item["result"] for item in window["presence_results"]], ["present", "present", "missing"])
            self.assertEqual([item["result"] for item in window["warning_results"]], ["owned current session missing", "owned current session missing"])

    def test_missing_session_with_a_persistent_window_fails(self):
        with tempfile.TemporaryDirectory() as temp:
            terminal, window = self.terminal(Path(temp))

            def applescript(script, *_arguments):
                if script == driver.SESSION:
                    return "session missing"
                if script == driver.WINDOW_EXISTS:
                    return "present"
                return "owned current session missing"

            with mock.patch.object(driver, "applescript", side_effect=applescript), mock.patch.object(driver, "same_process", return_value=False), mock.patch.object(terminal, "observe"), mock.patch.object(terminal, "capture_closure") as capture, mock.patch.object(driver.time, "sleep"), mock.patch.object(driver.time, "monotonic", side_effect=[0, 0, 31]):
                with self.assertRaisesRegex(RuntimeError, "Timed out waiting for owned iTerm window to close"):
                    terminal.close_window()
            self.assertIs(terminal.window, window)
            self.assertEqual(window["presence_results"][-1]["result"], "present")
            self.assertEqual(window["warning_results"][-1]["result"], "owned current session missing")
            capture.assert_any_call("failure", include_ui=True)

    def test_changed_session_or_live_process_never_closes_window(self):
        with tempfile.TemporaryDirectory() as temp:
            terminal, window = self.terminal(Path(temp))
            with mock.patch.object(driver, "same_process", return_value=True), mock.patch.object(terminal, "capture_closure"), mock.patch.object(driver, "applescript") as apple:
                with self.assertRaisesRegex(RuntimeError, "Owned processes are still running"):
                    terminal.close_window()
                apple.assert_not_called()
            self.assertIs(terminal.window, window)
            with mock.patch.object(driver, "same_process", return_value=False), mock.patch.object(terminal, "capture_closure"), mock.patch.object(driver, "applescript", side_effect=RuntimeError("Owned window session changed.")):
                with self.assertRaisesRegex(RuntimeError, "Owned window session changed"):
                    terminal.close_window()
            self.assertIs(terminal.window, window)

    def test_capability_close_waits_for_window_disappearance(self):
        with tempfile.TemporaryDirectory() as temp:
            evidence = Path(temp)
            observations = iter(("present", "missing"))

            def applescript(script, *arguments):
                if script == driver.SESSION:
                    self.assertEqual(arguments, ("123", "probe-session", "close"))
                    return "closed"
                self.assertEqual(script, driver.WINDOW_EXISTS)
                self.assertEqual(arguments, ("123",))
                return next(observations)

            with mock.patch.object(driver, "applescript", side_effect=applescript), mock.patch.object(driver.time, "sleep"):
                self.assertTrue(driver.close_probe(evidence / "unused", evidence, ("123", "probe-session")))
            self.assertIn("missing", (evidence / "probe-cleanup.txt").read_text())


if __name__ == "__main__":
    unittest.main()
