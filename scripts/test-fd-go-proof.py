#!/usr/bin/env python3
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import textwrap
import unittest


spec = importlib.util.spec_from_file_location("verify_fd_go_proof", Path(__file__).with_name("verify-fd-go-proof.py"))
probe = importlib.util.module_from_spec(spec)
spec.loader.exec_module(probe)


class ProofTests(unittest.TestCase):
    def test_outside_sentinel_is_verified_independently(self):
        with tempfile.TemporaryDirectory(prefix="fdg-test.") as directory:
            scratch = Path(directory)
            fixture = scratch / "fd-go.example"
            sentinel = fixture / "outside/pig-platform/go-mod/sentinel"
            sentinel.parent.mkdir(parents=True)
            sentinel.write_bytes(b"outside remains unchanged\n")
            proof = scratch / "proof.json"
            proof.write_text(json.dumps({"fixture": str(fixture), "replacement_sentinel_sha256": hashlib.sha256(b"outside remains unchanged\n").hexdigest()}))
            self.assertEqual(probe.verify(scratch, proof)["sha256"], "dfa942d4d5d7dea95af6182b5125f5073356310417a876507043a8a51a426601")
            alias = scratch.parent / (scratch.name + "-alias")
            alias.symlink_to(scratch, target_is_directory=True)
            try:
                proof.write_text(json.dumps({"fixture": str(alias / fixture.name), "replacement_sentinel_sha256": hashlib.sha256(b"outside remains unchanged\n").hexdigest()}))
                self.assertEqual(probe.verify(scratch, proof)["status"], "verified")
            finally:
                alias.unlink()
            sentinel.write_text("changed\n")
            proof.write_text(json.dumps({"fixture": str(fixture), "replacement_sentinel_sha256": hashlib.sha256(b"outside remains unchanged\n").hexdigest()}))
            with self.assertRaisesRegex(RuntimeError, "sentinel differs"):
                probe.verify(scratch, proof)

    def test_outside_and_symlinked_fixtures_are_rejected(self):
        with tempfile.TemporaryDirectory(prefix="fdg-test.") as directory:
            root = Path(directory)
            scratch = root / "scratch"
            scratch.mkdir()
            outside = root / "fd-go.outside"
            outside.mkdir()
            proof = scratch / "proof.json"
            proof.write_text(json.dumps({"fixture": str(outside)}))
            with self.assertRaisesRegex(RuntimeError, "outside the private allocation"):
                probe.verify(scratch, proof)
            (scratch / "fd-go.link").symlink_to(outside, target_is_directory=True)
            proof.write_text(json.dumps({"fixture": str(scratch / "fd-go.link")}))
            with self.assertRaisesRegex(RuntimeError, "symlinked"):
                probe.verify(scratch, proof)

    def test_mac_probe_guards_stop_before_allocating_scratch(self):
        workflow = Path(__file__).resolve().parents[1] / ".github/workflows/collaboration-platform-probe.yml"
        job = workflow.read_text().split("  fd-go-cache:\n", 1)[1]
        after = job.split("      - name: Run the isolated file-descriptor probe\n", 1)[1].split("        run: |\n", 1)[1]
        lines = []
        for line in after.splitlines():
            if line and not line.startswith("          "):
                break
            lines.append(line)
        script = textwrap.dedent("\n".join(lines)) + "\n"
        shell = os.environ.get("BASH_FOR_TESTS", "/bin/bash")
        with tempfile.TemporaryDirectory(prefix="fdg-guards.") as directory:
            root = Path(directory)
            bin_dir = root / "bin"
            bin_dir.mkdir()
            (root / "fd-go-cache").mkdir()
            stubs = {
                "uname": "printf 'x86_64\\n'",
                "go": "printf '%s\\n' \"$FAKE_GO_VERSION\"",
                "shasum": "printf '%s  fixture\\n' \"$FAKE_SHA\"",
                "df": "printf 'Filesystem Blocks Used Available Capacity Mounted\\n%s 10 1 9 10%% /private/tmp\\n' \"$FAKE_DEVICE\"",
                "mktemp": "printf reached > \"$SCRATCH_REACHED\"; exit 1",
            }
            for name, command in stubs.items():
                tool = bin_dir / name
                tool.write_text("#!/bin/sh\n" + command + "\n")
                tool.chmod(0o755)
            env = os.environ | {"PATH": str(bin_dir) + os.pathsep + os.environ["PATH"], "RUNNER_TEMP": str(root), "PROBE_RUNNER_LABEL": "macos-15-intel", "RUNNER_ARCH": "X64", "FAKE_GO_VERSION": "go version go1.27.4 darwin/amd64", "FAKE_SHA": "30031c595872a19a0caa15f10c29fd427ac4ba056bd1bbfdbc7bab9440bc6754", "FAKE_DEVICE": "/dev/disk1", "SCRATCH_REACHED": str(root / "scratch-reached")}
            valid_disk = "cd() { return 0; }\npwd() { printf '/private/tmp\\n'; }\n"
            reached = subprocess.run([shell, "-e", "-o", "pipefail", "-c", valid_disk + script], cwd=root, env=env, capture_output=True, text=True)
            self.assertNotEqual(reached.returncode, 0)
            self.assertTrue((root / "scratch-reached").exists())
            (root / "scratch-reached").unlink()
            cases = [
                ({"RUNNER_ARCH": "ARM64"}, valid_disk),
                ({"FAKE_GO_VERSION": "go version go1.26 darwin/amd64"}, valid_disk),
                ({"FAKE_SHA": "wrong"}, valid_disk),
                ({"FAKE_DEVICE": "tmpfs"}, valid_disk),
                ({}, "cd() { return 0; }\n"),
            ]
            for changes, prefix in cases:
                with self.subTest(changes=changes, prefix=prefix):
                    result = subprocess.run([shell, "-e", "-o", "pipefail", "-c", prefix + script], cwd=root, env=env | changes, capture_output=True, text=True)
                    self.assertNotEqual(result.returncode, 0)
                    self.assertFalse((root / "scratch-reached").exists())

    def test_fd_probe_selector_cannot_run_product_verification(self):
        workflow = Path(__file__).resolve().parents[1] / ".github/workflows/collaboration-platform-probe.yml"
        text = workflow.read_text()
        product = text.split("  p03-macos:\n", 1)[1].split("\n  iterm-lifecycle:\n", 1)[0]
        self.assertNotIn("fd-go-cache", product)
        job = text.split("  fd-go-cache:\n", 1)[1].split("\n  p04-host-recovery:\n", 1)[0]
        self.assertIn("inputs.scenario == 'fd-go-cache'", job)
        self.assertIn("go-version: '1.27.4'", job)
        self.assertIn("verify-fd-go-proof.py", job)
        self.assertNotIn("verify-p03-macos.sh", job)
        self.assertNotIn("verify-pig-platforms.sh", job)
        for line in job.splitlines():
            condition = line.strip()
            if condition.startswith(("test ", "[[ ")):
                self.assertTrue(condition.endswith("|| exit 1"), condition)


if __name__ == "__main__":
    unittest.main()
