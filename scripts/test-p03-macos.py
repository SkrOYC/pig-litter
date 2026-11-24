import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from urllib.error import HTTPError
from urllib.request import Request, urlopen


spec = importlib.util.spec_from_file_location("p03_macos", Path(__file__).with_name("p03-macos.py"))
driver = importlib.util.module_from_spec(spec)
spec.loader.exec_module(driver)


class InputAndProviderTests(unittest.TestCase):
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
