import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import threading
import unittest
from unittest import mock
from urllib.error import HTTPError
from urllib.request import Request, urlopen


spec = importlib.util.spec_from_file_location("p03_macos", Path(__file__).with_name("p03-macos.py"))
driver = importlib.util.module_from_spec(spec)
spec.loader.exec_module(driver)


class InputAndProviderTests(unittest.TestCase):
    def test_malformed_sibling_reply_releases_identified_fixture_pane(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            lead = ("11111111-1111-4111-8111-111111111111", "/dev/ttys001")
            added = ("22222222-2222-4222-8222-222222222222", "/dev/ttys002")
            actions = []

            class Fixture:
                def __init__(self, *_args):
                    self.changed = threading.Condition()
                    self.stage = self.requests = 0
                    self.errors = []
                    self.refs = {}
                    self.rosters = {}
                    self.observations = {}
                    self.request_models = {}
                    self.ordinary_release = False

            class Terminal:
                def __init__(self, *_args):
                    self.window = None
                    self.residues = []
                    self.unsafe = False
                    self.interrupted = False
                def launch(self):
                    self.window = {"id": "41", "session": lead[0], "tty": lead[1], "ownership": {"iterm": {"pid": 1}}}
                def session_action(self, action):
                    if action == "text":
                        return "idle" if self.interrupted else "busy"
                    actions.append(action)
                    if action == "interrupt":
                        self.interrupted = True
                def wait(self, predicate, _label, _timeout):
                    if not predicate():
                        raise AssertionError("lead did not settle")
                def frame(self, _name):
                    return ""
                def cleanup(self):
                    actions.append("terminal-cleanup")
                    self.window = None

            class Server:
                server_address = ("127.0.0.1", 12345)
                def shutdown(self):
                    pass
                def server_close(self):
                    pass

            class Thread:
                def join(self, timeout):
                    pass
                def is_alive(self):
                    return False

            def script(kind, *args):
                if kind == driver.SIBLING and args[2] == "create":
                    return "malformed"
                if kind == driver.PANE_BY_TTY:
                    return "41\n" + added[0]
                if kind == driver.SIBLING and args[2] == "close":
                    actions.append((args[1], args[3]))
                    return "closed"
                raise AssertionError(args)

            fixture = Fixture()
            processes = {7: {"pid": 7, "tty": "ttys002", "command": "/bin/sleep 600", "start": "fixture"}}
            with mock.patch.object(driver, "Fixture", return_value=fixture), mock.patch.object(driver, "Terminal", Terminal), mock.patch.object(driver, "start_provider", return_value=(Server(), Thread())), mock.patch.object(driver, "window_sessions", side_effect=[{lead}, {lead, added}]), mock.patch.object(driver, "applescript", side_effect=script), mock.patch.object(driver, "process_snapshot", return_value=processes), mock.patch.object(driver, "executable", return_value="/bin/sleep"), mock.patch.object(driver, "same_process", return_value=True), mock.patch.object(driver, "native_window"), mock.patch.object(driver, "idle_editor", side_effect=lambda frame: frame == "idle"), mock.patch.object(driver, "save_native", return_value=["fixture"]), mock.patch.object(driver.subprocess, "run", side_effect=OSError("not macOS")):
                receipt = driver.run_case("iterm", root, root / "evidence", root / "pig", root / "extension")
            self.assertEqual(actions, [(added[0], added[1]), "interrupt", "exit", "terminal-cleanup"])
            self.assertEqual(receipt["product_status"], "failed")
            self.assertEqual(receipt["teardown_status"], "pass")

    def test_unexpected_case_failure_stops_further_cases(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            probe_dir = root / "collaboration-platform-probe"
            probe_dir.mkdir()
            (probe_dir / "result.json").write_text(json.dumps({"status": "pass", "signature_status": "verified", "automation_status": "approved", "window_id": "123", "session_id": "probe-session"}))
            events = []

            def applescript(_script, *arguments):
                if _script == driver.WINDOW_EXISTS:
                    self.assertEqual(arguments, ("123",))
                    events.append("window missing")
                    return "missing"
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
            self.assertEqual(events, ["exists", "team", "policy", "close", "window missing"])
            self.assertEqual(json.loads((root / "evidence/summary.json").read_text())["result"], "failed")
            self.assertFalse((root / "evidence/cleanup-ok").exists())

    def test_product_results_survive_verified_teardown_residue(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            probe_dir = root / "collaboration-platform-probe"
            probe_dir.mkdir()
            (probe_dir / "result.json").write_text(json.dumps({"status": "pass", "signature_status": "verified", "automation_status": "approved", "window_id": "123", "session_id": "probe-session"}))
            seen = []

            def run_case(name, *_args):
                seen.append(name)
                residue = name == "team"
                return {"product_status": "pass", "product_problems": [], "teardown_status": "failed" if residue else "pass", "teardown_problems": ["UNRELEASED FIXTURE RESIDUE: window 42"] if residue else [], "safe_to_continue": True}

            arguments = ["p03-macos.py", "--scenario", "p03-all", "--pig", "/pig", "--extension", "/extension", "--scratch", str(root), "--evidence", str(root / "evidence")]
            with mock.patch.dict(os.environ, {"RUNNER_TEMP": str(root)}), mock.patch.object(driver.sys, "platform", "darwin"), mock.patch.object(driver.sys, "argv", arguments), mock.patch.object(driver, "applescript", return_value="present"), mock.patch.object(driver, "close_probe", return_value=True), mock.patch.object(driver, "run_case", side_effect=run_case):
                with self.assertRaisesRegex(RuntimeError, "UNRELEASED FIXTURE RESIDUE"):
                    driver.main()
            summary = json.loads((root / "evidence/summary.json").read_text())
            self.assertEqual(seen, ["team", "policy", "unselected"])
            self.assertEqual(summary["product"]["status"], "pass")
            self.assertEqual(summary["teardown"]["status"], "failed")
            self.assertEqual(summary["result"], "failed")
            self.assertEqual((root / "evidence/product-exit.txt").read_text(), "0\n")
            self.assertFalse((root / "evidence/cleanup-ok").exists())

    def test_team_reopens_after_a_verified_unreleased_generation(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            seen = []

            class Fixture:
                def __init__(self, *_args):
                    self.changed = threading.Condition()
                    self.stage = 0
                    self.requests = 0
                    self.request_models = {}
                    self.errors = []
                    self.refs = {"alpha": {"id": "member", "team": {"root": "root"}}}
                    self.rosters = {}
                    self.observations = {}
                    self.ordinary_release = False

            class Server:
                server_address = ("127.0.0.1", 12345)
                def shutdown(self):
                    pass
                def server_close(self):
                    pass

            class Thread:
                def join(self, timeout):
                    pass
                def is_alive(self):
                    return False

            class Terminal:
                def __init__(self, case_root, *_args):
                    self.root = case_root
                    self.window = None
                    self.residues = []
                    self.unsafe = False
                def launch(self, session_path=None):
                    seen.append(("launch", bool(session_path)))
                    self.window = {"id": "42" if session_path is None else "43"}
                    if session_path is None:
                        (self.root / "sessions/root.jsonl").write_text("{}\n")
                def send(self, message):
                    seen.append(("send", message))
                    fixture.stage = 6 if message == "P03_TEAM_FIRST" else 9
                def await_marker(self, marker, _fixture):
                    seen.append(("marker", marker))
                def exit_host(self):
                    self.residues.append(self.window)
                    self.window = None
                def cleanup(self):
                    self.window = None

            fixture = Fixture()
            with mock.patch.object(driver, "Fixture", return_value=fixture), mock.patch.object(driver, "start_provider", return_value=(Server(), Thread())), mock.patch.object(driver, "Terminal", Terminal), mock.patch.object(driver, "save_native", return_value=["evidence"]), mock.patch.object(driver, "native_ids", return_value={"member", "root"}):
                receipt = driver.run_case("team", root, root / "evidence", root / "pig", root / "extension")
            self.assertEqual([item for item in seen if item[0] == "launch"], [("launch", False), ("launch", True)])
            self.assertEqual([item for item in seen if item[0] == "send"], [("send", "P03_TEAM_FIRST"), ("send", "P03_TEAM_RESTART")])
            self.assertEqual((receipt["product_status"], receipt["teardown_status"], receipt["safe_to_continue"]), ("pass", "failed", True))
            self.assertEqual(receipt["stage"], 9)
            self.assertEqual(len(receipt["teardown_problems"]), 2)

    def test_unsafe_fixture_stops_later_cases(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            probe_dir = root / "collaboration-platform-probe"
            probe_dir.mkdir()
            (probe_dir / "result.json").write_text(json.dumps({"status": "pass", "signature_status": "verified", "automation_status": "approved", "window_id": "123", "session_id": "probe-session"}))
            receipt = {"product_status": "failed", "product_problems": ["session changed"], "teardown_status": "failed", "teardown_problems": ["session changed"], "safe_to_continue": False}
            arguments = ["p03-macos.py", "--scenario", "p03-all", "--pig", "/pig", "--extension", "/extension", "--scratch", str(root), "--evidence", str(root / "evidence")]
            with mock.patch.dict(os.environ, {"RUNNER_TEMP": str(root)}), mock.patch.object(driver.sys, "platform", "darwin"), mock.patch.object(driver.sys, "argv", arguments), mock.patch.object(driver, "applescript", return_value="present"), mock.patch.object(driver, "close_probe", return_value=True), mock.patch.object(driver, "run_case", return_value=receipt) as run:
                with self.assertRaisesRegex(RuntimeError, "unsafe or ambiguous"):
                    driver.main()
            self.assertEqual([call.args[0] for call in run.call_args_list], ["team"])
            summary = json.loads((root / "evidence/summary.json").read_text())
            self.assertEqual(summary["product"]["status"], "failed")
            self.assertEqual(summary["teardown"]["status"], "failed")

    def test_exit_waits_for_owned_process_before_closing_window(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            generation = root / "generation-1"
            generation.mkdir()
            process = subprocess.Popen(["/bin/sh", "-c", "read -r line"], stdin=subprocess.PIPE, text=True)
            released = False
            terminal = driver.Terminal(root, root, root, root, root, root, root / "pig", root / "extension", True, 1)
            terminal.window = {"id": "42", "session": "owned", "generation_dir": str(generation), "wrapper_pid": process.pid, "launch_sent": True}
            terminal.owned = {"wrapper": {"pid": process.pid}}
            terminal.window["wrapper_identity"] = terminal.owned["wrapper"]

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
                with mock.patch.object(driver, "same_process", same_process), mock.patch.object(driver, "window_presence", return_value="missing"), mock.patch.object(terminal, "observe"), mock.patch.object(terminal, "session_action", session_action):
                    terminal.exit_host()
                self.assertTrue(released)
                self.assertIsNone(terminal.window)
            finally:
                if process.poll() is None:
                    process.terminate()
                process.wait(timeout=5)
                process.stdin.close()

    def test_hosted_finish_retains_scratch_and_preserves_product_result(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            evidence = root / "evidence"
            scratch = root / "scratch"
            evidence.mkdir()
            scratch.mkdir()
            (scratch / "sentinel").write_text("retained\n")
            (evidence / "product-exit.txt").write_text("0\n")
            repo = Path(__file__).resolve().parent.parent
            script = '''set -euo pipefail
driver=$TEST_DRIVER
scratch=$TEST_SCRATCH
evidence=$TEST_EVIDENCE
eval "$(sed -n '/^finish() {$/,/^}$/p' "$driver/scripts/verify-p03-macos.sh")"
trap finish EXIT
exit 1
'''
            env = os.environ | {"TEST_DRIVER": str(repo), "TEST_SCRATCH": str(scratch), "TEST_EVIDENCE": str(evidence)}
            result = subprocess.run(["bash", "-c", script], env=env, capture_output=True, text=True, check=False)
            self.assertEqual(result.returncode, 1, result.stderr)
            self.assertEqual((evidence / "product-exit.txt").read_text(), "0\n")
            self.assertEqual((evidence / "runner-exit.txt").read_text(), "1\n")
            self.assertEqual(json.loads((evidence / "scratch-cleanup.json").read_text())["status"], "retained")
            self.assertEqual((scratch / "sentinel").read_text(), "retained\n")

    def test_p04_recovery_selector_is_fixed_and_separate(self):
        workflow = Path(__file__).resolve().parents[1] / ".github/workflows/collaboration-platform-probe.yml"
        text = workflow.read_text()
        product = text.split("  p03-macos:\n", 1)[1].split("\n  iterm-lifecycle:\n", 1)[0]
        job = text.split("  p04-host-recovery:\n", 1)[1]
        self.assertNotIn("p04-host-recovery", product)
        self.assertIn("inputs.scenario == 'p04-host-recovery'", job)
        self.assertIn('macos-15-intel', job)
        self.assertIn('macos-15', job)
        self.assertIn("go -C extensions/pig-litter test -run '^TestDarwinPeer(HostGone|Recovery|UnknownAttemptRemainsPending)$' -count=1 -v -timeout=90s", job)
        self.assertIn('scripts/verify-pig-platforms.sh', job)
        self.assertNotIn('probe-macos-collaboration.sh', job)
        self.assertNotIn('verify-p03-macos.sh', job)
        for line in job.splitlines():
            condition = line.strip()
            if condition.startswith(("test ", "[[ ")):
                self.assertTrue(condition.endswith("|| exit 1"), condition)

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

    def test_preflight_uses_product_scripts_and_preserves_the_probe(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source = Path(__file__).resolve().parent.parent / "extensions/pig-litter/team_iterm.go"
            scripts = driver.product_iterm_scripts(source)
            self.assertEqual(set(scripts), {"itermFind", "itermSplit", "itermSelect", "itermClose"})
            window = "41"
            lead = ("11111111-1111-4111-8111-111111111111", "/dev/ttys001")
            child = ("22222222-2222-4222-8222-222222222222", "/dev/ttys002")
            sessions = {lead}
            process = {"pid": 7, "tty": "ttys002", "command": "/bin/sleep 600", "start": "fixture"}
            probe = root / "probe.json"
            probe.write_text(json.dumps({"status": "pass", "signature_status": "verified", "automation_status": "approved", "window_id": window, "session_id": lead[0], "session_tty": lead[1]}))
            calls = []

            def script(code, *args):
                calls.append(code)
                if code == driver.SESSION:
                    return "present"
                if code == scripts["itermFind"]:
                    match = next((item for item in sessions if args[0] == item[0] or args[0] == item[1]), None)
                    return "\n".join((window, *match)) if match else "missing"
                if code == scripts["itermSplit"]:
                    sessions.add(child)
                    return "\n".join((window, *child))
                if code == scripts["itermSelect"]:
                    return "selected"
                if code == scripts["itermClose"]:
                    if args[2] != child[1]:
                        raise RuntimeError("iTerm owned session changed")
                    if child not in sessions:
                        return "missing"
                    sessions.remove(child)
                    return "closed"
                raise AssertionError("Unexpected AppleScript")

            with mock.patch.object(driver, "iterm_owner", return_value={"pid": 1}), mock.patch.object(driver, "native_window"), mock.patch.object(driver, "window_sessions", side_effect=lambda _window: set(sessions)), mock.patch.object(driver, "applescript", side_effect=script), mock.patch.object(driver, "process_snapshot", return_value={7: process}), mock.patch.object(driver, "executable", return_value="/bin/sleep"), mock.patch.object(driver, "same_process", side_effect=lambda _process: child in sessions):
                driver.preflight_iterm(source, root, probe)
            receipt = json.loads((root / "iterm-control-preflight.json").read_text())
            self.assertEqual(receipt["status"], "pass")
            self.assertIsNone(receipt["cleanup_error"])
            self.assertEqual(sessions, {lead})
            self.assertTrue(all(scripts[name] in calls for name in scripts))

    def test_preflight_retains_a_probe_window_with_unrelated_sessions(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source = Path(__file__).resolve().parent.parent / "extensions/pig-litter/team_iterm.go"
            lead = ("11111111-1111-4111-8111-111111111111", "/dev/ttys001")
            other = ("22222222-2222-4222-8222-222222222222", "/dev/ttys002")
            probe = root / "probe.json"
            probe.write_text(json.dumps({"status": "pass", "signature_status": "verified", "automation_status": "approved", "window_id": "41", "session_id": lead[0], "session_tty": lead[1]}))
            with mock.patch.object(driver, "iterm_owner", return_value={"pid": 1}), mock.patch.object(driver, "native_window"), mock.patch.object(driver, "window_sessions", return_value={lead, other}), mock.patch.object(driver, "applescript", return_value="present") as control:
                with self.assertRaisesRegex(RuntimeError, "unrelated or missing sessions"):
                    driver.preflight_iterm(source, root, probe)
            result = json.loads((root / "iterm-control-preflight.json").read_text())
            self.assertEqual(result["status"], "failed")
            self.assertIsNotNone(result["cleanup_error"])
            self.assertTrue(all(call.args[0] == driver.SESSION for call in control.call_args_list))

    def test_preflight_never_marks_unverified_ownership_as_clean(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            source = Path(__file__).resolve().parent.parent / "extensions/pig-litter/team_iterm.go"
            probe = root / "probe.json"
            probe.write_text(json.dumps({"status": "pass", "signature_status": "verified", "automation_status": "approved", "window_id": "41", "session_id": "11111111-1111-4111-8111-111111111111", "session_tty": "/dev/ttys001"}))
            with mock.patch.object(driver, "iterm_owner", side_effect=RuntimeError("application identity unavailable")):
                with self.assertRaisesRegex(RuntimeError, "application identity unavailable"):
                    driver.preflight_iterm(source, root, probe)
            result = json.loads((root / "iterm-control-preflight.json").read_text())
            self.assertEqual(result["status"], "failed")
            self.assertEqual(result["cleanup_error"], "Fixture cleanup was not verified")

    def test_provider_reports_unicode_error_in_utf8_body(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            fixture = driver.Fixture("iterm", root, root)
            message = "Can’t set kind to tty"
            with mock.patch.object(fixture, "response", side_effect=RuntimeError(message)):
                server, thread = driver.start_provider(fixture)
                try:
                    url = f"http://127.0.0.1:{server.server_address[1]}/v1/chat/completions"
                    request = Request(url, data=b"{}", headers={"Content-Type": "application/json"})
                    with self.assertRaises(HTTPError) as failure:
                        urlopen(request, timeout=5)
                    self.assertEqual(failure.exception.reason, "Fixture error")
                    self.assertEqual(json.loads(failure.exception.read().decode("utf-8")), {"error": message})
                    self.assertEqual(fixture.errors, [message])
                finally:
                    server.shutdown()
                    server.server_close()
                    thread.join(timeout=5)
            self.assertFalse(thread.is_alive())

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
        window["wrapper_identity"] = terminal.owned["root"]
        return terminal, window

    def test_residue_checks_original_session_and_native_owner(self):
        script = driver.VERIFY_RESIDUE
        for guard in ("Original session is still present", "Window identity is ambiguous", "Alternate window identity changed", "Owned window still has tabs", "Owned window has a current session", "Owned window is visible"):
            self.assertIn(guard, script)
        self.assertNotIn(" to close", script)
        owner = {"pid": 9, "start": "birth", "executable": "/iTerm"}
        with mock.patch.object(driver, "same_process", return_value=True), mock.patch.object(driver, "native_windows", return_value=[{"number": 42, "owner_pid": 10}]):
            with self.assertRaisesRegex(RuntimeError, "Native window identity or owner changed"):
                driver.native_window(owner, "42")
        with mock.patch.object(driver, "same_process", return_value=False):
            with self.assertRaisesRegex(RuntimeError, "iTerm process identity changed"):
                driver.native_window(owner, "42")

    def test_owned_hidden_zero_tab_residue_is_retained_for_teardown(self):
        with tempfile.TemporaryDirectory() as temp:
            terminal, window = self.terminal(Path(temp))
            host = {"pid": 101, "start": "host birth", "executable": "/pig"}
            terminal.owned["host"] = host
            window["host"] = host
            window["ownership"] = {"iterm": {"pid": 9, "start": "iterm birth", "executable": "/iTerm"}, "native_before": {"number": 42, "owner_pid": 9}, "alternate_id": "window-2"}

            def applescript(script, *arguments):
                if script == driver.SESSION:
                    return "session missing"
                if script == driver.WINDOW_EXISTS:
                    return "present"
                self.assertEqual(script, driver.VERIFY_RESIDUE)
                self.assertEqual(arguments, ("42", "owned-session", "window-2"))
                return "verified\npig"

            with mock.patch.object(driver, "applescript", side_effect=applescript), mock.patch.object(driver, "same_process", return_value=False), mock.patch.object(driver, "native_window", return_value={"number": 42, "owner_pid": 9}), mock.patch.object(terminal, "capture_closure"):
                terminal.close_window()
            self.assertIsNone(terminal.window)
            self.assertEqual(window["fixture_teardown"]["status"], "UNRELEASED FIXTURE RESIDUE")
            self.assertEqual(terminal.residues, [window])
            self.assertEqual(window["presence_results"][-1]["result"], "present")

    def test_ambiguous_residue_never_allows_the_next_generation(self):
        with tempfile.TemporaryDirectory() as temp:
            terminal, window = self.terminal(Path(temp))
            host = {"pid": 101, "start": "host birth", "executable": "/pig"}
            terminal.owned["host"] = host
            window["host"] = host
            window["ownership"] = {"iterm": {"pid": 9, "start": "iterm birth", "executable": "/iTerm"}, "native_before": {"number": 42, "owner_pid": 9}, "alternate_id": "window-2"}

            def applescript(script, *_arguments):
                if script == driver.SESSION:
                    return "session missing"
                if script == driver.WINDOW_EXISTS:
                    return "present"
                raise RuntimeError("Original session is still present")

            with mock.patch.object(driver, "applescript", side_effect=applescript), mock.patch.object(driver, "same_process", return_value=False), mock.patch.object(driver, "native_window", return_value={"number": 42, "owner_pid": 9}), mock.patch.object(terminal, "capture_closure"):
                with self.assertRaisesRegex(RuntimeError, "Original session is still present"):
                    terminal.close_window()
            self.assertIs(terminal.window, window)
            self.assertTrue(terminal.unsafe)
            self.assertEqual(terminal.residues, [])

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
            self.assertEqual(window["process_exit_observed"]["wrapper"], {"pid": 100, "start": "birth", "executable": "/bin/sh"})
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

    def test_session_change_after_a_missing_session_is_an_error(self):
        with tempfile.TemporaryDirectory() as temp:
            terminal, window = self.terminal(Path(temp))

            def applescript(script, *_arguments):
                if script == driver.SESSION:
                    return "session missing"
                if script == driver.WINDOW_EXISTS:
                    return "present"
                return "owned session changed"

            with mock.patch.object(driver, "applescript", side_effect=applescript), mock.patch.object(driver, "same_process", return_value=False), mock.patch.object(terminal, "observe"), mock.patch.object(terminal, "capture_closure"):
                with self.assertRaisesRegex(RuntimeError, "Owned window session changed"):
                    terminal.close_window()
            self.assertIs(terminal.window, window)
            self.assertEqual(window["warning_results"][-1]["result"], "owned session changed")

    def test_unattributed_wrapper_is_not_closed(self):
        with tempfile.TemporaryDirectory() as temp:
            terminal, window = self.terminal(Path(temp))
            del window["wrapper_identity"]
            with mock.patch.object(terminal, "capture_closure"), mock.patch.object(driver, "applescript") as apple:
                with self.assertRaisesRegex(RuntimeError, "Owned wrapper process identity was not observed"):
                    terminal.close_window()
                apple.assert_not_called()
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

    def test_capability_window_still_present_is_not_a_success(self):
        with tempfile.TemporaryDirectory() as temp:
            evidence = Path(temp)

            def applescript(script, *_arguments):
                return "closed" if script == driver.SESSION else "present"

            with mock.patch.object(driver, "applescript", side_effect=applescript), mock.patch.object(driver.time, "sleep"), mock.patch.object(driver.time, "monotonic", side_effect=[0, 0, 31]):
                self.assertFalse(driver.close_probe(evidence / "unused", evidence, ("123", "probe-session")))
            self.assertEqual((evidence / "probe-cleanup.txt").read_text(), "closed\npresent\n")


if __name__ == "__main__":
    unittest.main()
