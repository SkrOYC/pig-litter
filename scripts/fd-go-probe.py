import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile

root = Path(tempfile.mkdtemp(prefix="fd-go.", dir=Path(__file__).parent))
platform = root / "scratch" / "pig-platform.A1b2C3d4"
cache = platform / "go-mod"
module = cache / "example.test" / "tiny@v1.0.0"
module.mkdir(parents=True)
(module / "module.go").write_text("package tiny\n")
module.chmod(0o555)
replacement = root / "outside" / "pig-platform"
sentinel = replacement / "go-mod" / "sentinel"
sentinel.parent.mkdir(parents=True)
sentinel.write_text("outside remains unchanged\n")
original_digest = hashlib.sha256(sentinel.read_bytes()).hexdigest()
fd = os.open(platform, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
saved = platform.with_name("saved-platform")
platform.rename(saved)
platform.symlink_to(replacement, target_is_directory=True)
fd_path = Path("/dev/fd") / str(fd)
env = os.environ | {
    "GOMODCACHE": str(fd_path / "go-mod"),
    "GOCACHE": str(saved / "go-build"),
    "GOTMPDIR": str(saved / "go-tmp"),
    "GOPATH": str(saved / "go"),
    "TMPDIR": str(saved / "tmp"),
    "GOENV": "off", "GOWORK": "off", "GOFLAGS": "", "GOTOOLCHAIN": "local", "GOPROXY": "off", "GOSUMDB": "off", "GOTELEMETRY": "off",
}
result = subprocess.run(["go", "clean", "-modcache"], env=env, pass_fds=(fd,), capture_output=True, text=True)
os.close(fd)
proof = {
    "fixture": str(root),
    "go_exit": result.returncode,
    "go_stdout": result.stdout,
    "go_stderr": result.stderr,
    "original_cache_exists": (saved / "go-mod").exists(),
    "replacement_sentinel_unchanged": sentinel.exists() and hashlib.sha256(sentinel.read_bytes()).hexdigest() == original_digest,
    "replacement_sentinel_sha256": original_digest,
}
print(json.dumps(proof, indent=2))
if result.returncode != 0 or proof["original_cache_exists"] or not proof["replacement_sentinel_unchanged"]:
    raise SystemExit(1)
