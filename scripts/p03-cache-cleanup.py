import json
import os
from pathlib import Path
import re
import subprocess
import sys


def identity(path):
    stat = path.lstat()
    if not path.is_dir() or path.is_symlink() or stat.st_uid != os.getuid():
        raise RuntimeError(f"Not an owned directory: {path}")
    return f"{stat.st_dev}:{stat.st_ino}"


def run_probe(command, evidence, name):
    result = subprocess.run(command, capture_output=True, text=True, check=False)
    (evidence / f"{name}.stdout").write_text(result.stdout)
    (evidence / f"{name}.stderr").write_text(result.stderr)
    if result.returncode != 1 or result.stdout or result.stderr:
        raise RuntimeError(f"Unable to establish that {name} has no users; exit {result.returncode}")


def check_go_processes(scratch, evidence):
    processes = subprocess.run(["pgrep", "-x", "go|compile|link|asm|cgo"], capture_output=True, text=True, check=False)
    if processes.returncode not in (0, 1) or processes.stderr:
        raise RuntimeError("Could not enumerate Go processes")
    users = []
    for pid in processes.stdout.splitlines():
        if not pid.isdigit():
            raise RuntimeError("Invalid Go process identity")
        process = subprocess.run(["ps", "eww", "-p", pid, "-o", "command="], capture_output=True, text=True, check=False)
        if process.returncode != 0:
            try:
                os.kill(int(pid), 0)
            except ProcessLookupError:
                continue
            raise RuntimeError(f"Could not inspect Go process {pid}")
        if not process.stdout.strip():
            raise RuntimeError(f"Go process {pid} has no visible command")
        if str(scratch) in process.stdout:
            users.append(pid)
    (evidence / "go-processes.txt").write_text("".join(f"{pid}\n" for pid in users))
    if users:
        raise RuntimeError("Go processes still reference the private scratch")


def cleanup(scratch, scratch_id, evidence):
    if identity(scratch) != scratch_id or scratch.resolve() != scratch:
        raise RuntimeError("Scratch identity changed")
    if scratch.stat().st_mode & 0o077:
        raise RuntimeError("Scratch is not private")

    platform_text, platform_id, cache_id = (evidence / "native-platform/cache-identity.txt").read_text().splitlines()
    platform = Path(platform_text)
    if platform.parent != scratch or not re.fullmatch(r"pig-platform\.[a-zA-Z0-9]{8}", platform.name):
        raise RuntimeError("Platform directory is outside the private scratch")
    cache = platform / "go-mod"
    if identity(platform) != platform_id or identity(cache) != cache_id:
        raise RuntimeError("Platform or module cache identity changed")

    run_probe(["lsof", "-nP", "+D", str(scratch)], evidence, "scratch-users")
    check_go_processes(scratch, evidence)

    if identity(scratch) != scratch_id or identity(platform) != platform_id or identity(cache) != cache_id:
        raise RuntimeError("Allocation identity changed before cache cleanup")
    environment = os.environ | {
        "GOMODCACHE": str(cache),
        "GOCACHE": str(platform / "go-build"),
        "GOTMPDIR": str(platform / "go-tmp"),
        "GOPATH": str(platform / "go"),
        "TMPDIR": str(platform / "tmp"),
        "GOENV": "off",
        "GOWORK": "off",
        "GOFLAGS": "",
        "GOTOOLCHAIN": "local",
        "GOPROXY": "off",
        "GOSUMDB": "off",
    }
    subprocess.run(["go", "clean", "-modcache"], env=environment, check=True)
    if cache.exists() or cache.is_symlink():
        raise RuntimeError("Module cache remains after go clean")
    if identity(scratch) != scratch_id or identity(platform) != platform_id:
        raise RuntimeError("Allocation identity changed before scratch removal")
    subprocess.run(["rm", "-rf", "--", str(scratch)], check=True)
    if scratch.exists() or scratch.is_symlink():
        raise RuntimeError("Scratch remains after removal")


def main():
    scratch = Path(sys.argv[1])
    scratch_id = sys.argv[2]
    evidence = Path(sys.argv[3])
    try:
        cleanup(scratch, scratch_id, evidence)
    except (OSError, RuntimeError, ValueError, subprocess.SubprocessError) as error:
        (evidence / "scratch-cleanup.json").write_text(json.dumps({"status": "failed", "error": str(error)}) + "\n")
        print(f"Scratch cleanup failed: {error}", file=sys.stderr)
        return 1
    (evidence / "scratch-cleanup.json").write_text(json.dumps({"status": "removed"}) + "\n")
    return 0


if __name__ == "__main__":
    sys.exit(main())
