import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
import zipfile


HELPER = Path(__file__).with_name("p03-cache-cleanup.py")


def identity(path):
    stat = path.stat()
    return f"{stat.st_dev}:{stat.st_ino}"


class CacheCleanupTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="p03-cache-test.", dir=os.environ["P03_CACHE_TEST_DIR"])
        self.root = Path(self.temp.name).resolve()
        self.scratch = self.root / "p03.fixture1"
        self.platform = self.scratch / "pig-platform.A1b2C3d4"
        self.cache = self.platform / "go-mod"
        self.evidence = self.root / "evidence"
        self.sentinel = self.root / "unrelated-cache/sentinel"
        self.scratch.mkdir(mode=0o700)
        for path in (self.cache, self.platform / "go-build", self.platform / "go-tmp", self.platform / "tmp", self.platform / "go", self.evidence / "native-platform", self.sentinel.parent):
            path.mkdir(parents=True)
        self.sentinel.write_text("untouched\n")
        proxy = self.root / "proxy/example.test/tiny/@v"
        proxy.mkdir(parents=True)
        mod = "module example.test/tiny\n\ngo 1.20\n"
        (proxy / "v1.0.0.mod").write_text(mod)
        (proxy / "v1.0.0.info").write_text(json.dumps({"Version": "v1.0.0", "Time": "2020-01-01T00:00:00Z"}))
        with zipfile.ZipFile(proxy / "v1.0.0.zip", "w") as archive:
            archive.writestr("example.test/tiny@v1.0.0/go.mod", mod)
            archive.writestr("example.test/tiny@v1.0.0/tiny.go", "package tiny\n")
        self.env = os.environ | {
            "GOMODCACHE": str(self.cache), "GOCACHE": str(self.platform / "go-build"),
            "GOTMPDIR": str(self.platform / "go-tmp"), "GOPATH": str(self.platform / "go"),
            "GOPROXY": (self.root / "proxy").as_uri(), "GOSUMDB": "off", "GOENV": "off",
            "GOWORK": "off", "GOFLAGS": "", "GOTOOLCHAIN": "local",
        }
        subprocess.run(["go", "mod", "download", "example.test/tiny@v1.0.0"], env=self.env, check=True, capture_output=True, text=True)
        self.unpacked = self.cache / "example.test/tiny@v1.0.0"
        self.assertEqual(self.unpacked.stat().st_mode & 0o777, 0o555)
        self.scratch_id = identity(self.scratch)
        self.platform_id = identity(self.platform)
        self.cache_id = identity(self.cache)
        self.record()

    def record(self, cache_id=None):
        (self.evidence / "native-platform/cache-identity.txt").write_text(f"{self.platform}\n{self.platform_id}\n{cache_id or self.cache_id}\n")

    def tearDown(self):
        if self.cache.is_dir() and not self.cache.is_symlink() and identity(self.cache) == self.cache_id:
            subprocess.run(["go", "clean", "-modcache"], env=self.env, check=True, capture_output=True, text=True)
        self.temp.cleanup()

    def invoke(self):
        return subprocess.run([sys.executable, str(HELPER), str(self.scratch), self.scratch_id, str(self.evidence)], capture_output=True, text=True, check=False)

    def invoke_finish(self, product_exit):
        environment = os.environ | {
            "PATH": str(Path(sys.executable).parent) + os.pathsep + os.environ["PATH"],
            "TEST_SCRATCH": str(self.scratch), "TEST_SCRATCH_ID": self.scratch_id,
            "TEST_EVIDENCE": str(self.evidence), "TEST_DRIVER": str(HELPER.parent.parent),
            "TEST_PRODUCT_EXIT": str(product_exit),
        }
        script = '''set -euo pipefail
driver=$TEST_DRIVER
scratch=$TEST_SCRATCH
scratch_id=$TEST_SCRATCH_ID
evidence=$TEST_EVIDENCE
eval "$(sed -n '/^finish() {$/,/^}$/p' "$driver/scripts/verify-p03-macos.sh")"
trap finish EXIT
exit "$TEST_PRODUCT_EXIT"
'''
        return subprocess.run(["bash", "-c", script], env=environment, capture_output=True, text=True, check=False)

    def test_removes_readonly_cache_and_private_scratch_only(self):
        result = self.invoke()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads((self.evidence / "scratch-cleanup.json").read_text()), {"status": "removed"})
        self.assertFalse(self.scratch.exists())
        self.assertEqual(self.sentinel.read_text(), "untouched\n")

    def test_refuses_a_changed_cache_identity(self):
        self.record("0:0")
        result = self.invoke()
        self.assertEqual(result.returncode, 1)
        self.assertIn("identity changed", result.stderr)
        self.assertTrue((self.unpacked / "tiny.go").is_file())
        self.assertEqual(self.sentinel.read_text(), "untouched\n")

    def test_refuses_a_cache_replaced_by_a_symlink(self):
        saved = self.platform / "saved-cache"
        self.cache.rename(saved)
        self.cache.symlink_to(self.sentinel.parent)
        try:
            result = self.invoke()
            self.assertEqual(result.returncode, 1)
            self.assertIn("Not an owned directory", result.stderr)
            self.assertEqual(self.sentinel.read_text(), "untouched\n")
            self.assertTrue((saved / "example.test/tiny@v1.0.0/tiny.go").is_file())
        finally:
            self.cache.unlink()
            saved.rename(self.cache)

    def test_refuses_cache_with_a_live_user(self):
        holder = subprocess.Popen([sys.executable, "-c", "import sys; f = open(sys.argv[1]); print('ready', flush=True); sys.stdin.readline()", str(self.unpacked / "tiny.go")], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
        try:
            self.assertEqual(holder.stdout.readline(), "ready\n")
            result = self.invoke()
            self.assertEqual(result.returncode, 1)
            self.assertIn("scratch-users", result.stderr)
            self.assertTrue((self.unpacked / "tiny.go").is_file())
            self.assertEqual(self.sentinel.read_text(), "untouched\n")
        finally:
            holder.stdin.close()
            holder.wait(timeout=5)
            holder.stdout.close()

    def test_refuses_a_go_process_with_the_cache_in_its_environment(self):
        executable = self.root / "go"
        source = self.root / "wait.c"
        source.write_text("#include <unistd.h>\nint main(void) { char c; return read(0, &c, 1) < 0; }\n")
        subprocess.run(["cc", str(source), "-o", str(executable)], check=True, capture_output=True, text=True)
        holder = subprocess.Popen([str(executable)], env=self.env, stdin=subprocess.PIPE)
        try:
            result = self.invoke()
            self.assertEqual(result.returncode, 1, result.stderr)
            self.assertIn("Go processes still reference", result.stderr)
            self.assertTrue((self.unpacked / "tiny.go").is_file())
        finally:
            holder.stdin.close()
            holder.wait(timeout=5)

    def test_finish_keeps_product_failure_and_scratch(self):
        (self.evidence / "cleanup-ok").write_text("terminal processes released\n")
        result = self.invoke_finish(7)
        self.assertEqual(result.returncode, 7, result.stderr)
        self.assertEqual((self.evidence / "product-exit.txt").read_text(), "7\n")
        self.assertEqual((self.evidence / "runner-exit.txt").read_text(), "7\n")
        self.assertEqual(json.loads((self.evidence / "scratch-cleanup.json").read_text())["status"], "retained")
        self.assertTrue((self.unpacked / "tiny.go").is_file())

    def test_finish_reports_success_only_after_removal(self):
        (self.evidence / "cleanup-ok").write_text("terminal processes released\n")
        result = self.invoke_finish(0)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual((self.evidence / "product-exit.txt").read_text(), "0\n")
        self.assertEqual((self.evidence / "runner-exit.txt").read_text(), "0\n")
        self.assertEqual(json.loads((self.evidence / "scratch-cleanup.json").read_text())["status"], "removed")
        self.assertFalse(self.scratch.exists())
        self.assertEqual(self.sentinel.read_text(), "untouched\n")

    def test_finish_reports_cleanup_failure_after_product_success(self):
        (self.evidence / "cleanup-ok").write_text("terminal processes released\n")
        self.record("0:0")
        result = self.invoke_finish(0)
        self.assertEqual(result.returncode, 1, result.stderr)
        self.assertEqual((self.evidence / "product-exit.txt").read_text(), "0\n")
        self.assertEqual((self.evidence / "runner-exit.txt").read_text(), "1\n")
        self.assertEqual(json.loads((self.evidence / "scratch-cleanup.json").read_text())["status"], "failed")
        self.assertTrue((self.unpacked / "tiny.go").is_file())


if __name__ == "__main__":
    unittest.main()
