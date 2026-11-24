import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from urllib.error import HTTPError
from urllib.request import Request, urlopen


spec = importlib.util.spec_from_file_location("p03_macos", Path(__file__).with_name("p03-macos.py"))
driver = importlib.util.module_from_spec(spec)
spec.loader.exec_module(driver)


class InputAndProviderTests(unittest.TestCase):
    def test_wrong_candidate_never_runs_the_prepared_command(self):
        with tempfile.TemporaryDirectory() as temp:
            evidence = Path(temp) / "evidence"
            marker = Path(temp) / "ran"
            repo = Path(__file__).resolve().parent.parent
            environment = os.environ | {"PIG_PLATFORM_ROOT_DIR": str(repo)}
            result = subprocess.run(
                ["bash", str(repo / "scripts/verify-pig-platforms.sh"), "0" * 40, "linux", "amd64", str(evidence), "--", "bash", "-c", 'printf ran > "$1"', "callback", str(marker)],
                env=environment, capture_output=True, text=True, check=False,
            )
            self.assertEqual(result.returncode, 1)
            self.assertFalse(marker.exists())
            self.assertEqual((evidence / "result.txt").read_text(), "exit_code=1\n")

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


if __name__ == "__main__":
    unittest.main()
