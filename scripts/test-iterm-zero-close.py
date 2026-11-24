#!/usr/bin/env python3
from copy import deepcopy
import unittest

import iterm_zero_close as probe


class FakeDriver:
    def __init__(self):
        self.alive = set()
        self.current = {99: {"pid": 99, "start": "iterm birth"}}

    def same_process(self, process):
        return process["pid"] in self.alive

    def process_snapshot(self):
        return self.current

    def safe_text(self, value):
        if not value or "\n" in value:
            raise RuntimeError("Unsafe text")
        return value


class FakeDiagnostic:
    def __init__(self):
        self.iterm = {"pid": 99, "start": "iterm birth"}
        self.calls = []
        self.events = []
        self.before = [{"number": 44, "owner_pid": 99}, {"number": 39, "owner_pid": 99}]
        self.inspect = "eligible\nP03_PIG_nonce (pig)\nfalse\ntrue"
        self.close = "close requested"
        self.poll_state = "missing"
        self.control_ok = True

    def check_control(self, stage, _control):
        if not self.control_ok:
            raise RuntimeError("Control changed")
        self.calls.append((stage,))

    def snapshot(self, stage):
        self.calls.append((stage,))
        return self.before

    def action(self, stage, _script, *arguments):
        self.calls.append((stage, *arguments))
        return self.inspect if stage.endswith(".inspect") else self.close

    def state(self, _stage, _target):
        return self.poll_state

    def event(self, stage, **fields):
        self.events.append({"stage": stage, **fields})


def candidate():
    wrapper = {"pid": 10, "ppid": 8, "start": "wrapper birth", "executable": "/bin/sh"}
    host = {"pid": 11, "ppid": 10, "start": "host birth", "executable": "/pig"}
    return {
        "error": None,
        "state": "present:missing",
        "exit_status": 0,
        "processes_exited": True,
        "requests": 0,
        "provider_joined": True,
        "provider_errors": [],
        "target": {"window": "44", "session": "session-id", "tty": "/dev/ttys002", "process": wrapper},
        "host": host,
        "processes": [wrapper, host],
        "nonce": "P03_PIG_nonce",
        "alternate_id": "window-2",
        "native_before": [{"number": 44, "owner_pid": 99}, {"number": 39, "owner_pid": 99}],
    }


class ZeroCloseTests(unittest.TestCase):
    def test_original_alternate_identifier_must_be_unambiguous(self):
        driver = FakeDriver()
        diagnostic = FakeDiagnostic()
        diagnostic.close = "window-2"
        target = {"window": "44", "session": "session-id"}
        self.assertEqual(probe.capture_alternate(diagnostic, driver, target), "window-2")
        for value in ("window-2\nother", "missing value", ""):
            diagnostic.close = value
            with self.subTest(value=value), self.assertRaisesRegex(RuntimeError, "alternate window identifier"):
                probe.capture_alternate(diagnostic, driver, target)

    def test_exact_owned_window_is_closed_once_and_both_censuses_are_checked(self):
        diagnostic = FakeDiagnostic()
        driver = FakeDriver()
        calls = 0

        def native():
            nonlocal calls
            calls += 1
            if calls == 1:
                return diagnostic.before
            return [{"number": 39, "owner_pid": 99}]

        outcome = probe.probe(diagnostic, driver, native, candidate(), {"window": "39", "session": "control", "tty": "/dev/ttys001"}, seconds=0)
        close_calls = [call for call in diagnostic.calls if call[0] == "owned-zero.close"]
        self.assertEqual(close_calls, [("owned-zero.close", "44", "window-2", "P03_PIG_nonce", "close", "P03_PIG_nonce (pig)")])
        self.assertEqual((outcome["state"], outcome["native_target"]), ("missing", []))
        self.assertEqual(outcome["native_control"], [{"number": 39, "owner_pid": 99}])
        self.assertIn(("owned-zero.control-after",), diagnostic.calls)

    def test_ambiguous_identity_or_live_process_prevents_the_close(self):
        for change in ("alternate", "session", "overlap", "process", "native", "title", "closeable", "control", "owner"):
            with self.subTest(change=change):
                diagnostic = FakeDiagnostic()
                driver = FakeDriver()
                result = deepcopy(candidate())
                if change == "alternate":
                    result["alternate_id"] = ""
                elif change == "session":
                    result["state"] = "present:changed"
                elif change == "overlap":
                    result["target"]["tty"] = "/dev/ttys001"
                elif change == "process":
                    driver.alive.add(11)
                elif change == "native":
                    diagnostic.before[0]["owner_pid"] = 77
                elif change == "title":
                    diagnostic.inspect = "eligible\nother\nfalse\ntrue"
                elif change == "closeable":
                    diagnostic.inspect = "eligible\nP03_PIG_nonce (pig)\nfalse\nfalse"
                elif change == "control":
                    diagnostic.control_ok = False
                elif change == "owner":
                    driver.current[99]["start"] = "different birth"
                with self.assertRaises(RuntimeError):
                    probe.probe(diagnostic, driver, lambda: diagnostic.before, result, {"window": "39", "session": "control", "tty": "/dev/ttys001"}, seconds=0)
                self.assertEqual([call for call in diagnostic.calls if call[0] == "owned-zero.close"], [])

    def test_retained_native_window_is_not_reported_closed(self):
        diagnostic = FakeDiagnostic()
        driver = FakeDriver()
        diagnostic.poll_state = "present:missing"
        outcome = probe.probe(diagnostic, driver, lambda: diagnostic.before, candidate(), {"window": "39", "session": "control", "tty": "/dev/ttys001"}, seconds=0)
        self.assertEqual(outcome["state"], "present:missing")
        self.assertEqual(outcome["native_target"], [{"number": 44, "owner_pid": 99}])

    def test_missing_applescript_window_does_not_erase_native_match(self):
        diagnostic = FakeDiagnostic()
        driver = FakeDriver()
        outcome = probe.probe(diagnostic, driver, lambda: diagnostic.before, candidate(), {"window": "39", "session": "control", "tty": "/dev/ttys001"}, seconds=0)
        self.assertEqual(outcome["state"], "missing")
        self.assertEqual(outcome["native_target"], [{"number": 44, "owner_pid": 99}])

    def test_applescript_rechecks_identity_and_has_only_one_close_site(self):
        script = probe.ZERO_WINDOW_ACTION
        close = script.index("tell candidateWindow to close")
        for check in ("numericMatches is not 1", "alternateMatches is not 1", "nonceMatches is not 1", "alternate identity changed", "window has tabs", "current session exists", "title changed", "window is not closeable"):
            self.assertLess(script.index(check), close)
        self.assertEqual(script.count("tell candidateWindow to close"), 1)
        self.assertNotIn("click", script)


if __name__ == "__main__":
    unittest.main()
