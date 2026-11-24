#!/usr/bin/env python3
import argparse
import hashlib
import json
import os
from pathlib import Path
import stat
import sys


SENTINEL = b"outside remains unchanged\n"


def verify(scratch, proof_path):
    scratch = scratch.resolve(strict=True)
    proof = json.loads(proof_path.read_text())
    fixture = Path(proof["fixture"])
    if not fixture.is_absolute() or fixture.parent != scratch or not fixture.name.startswith("fd-go."):
        raise RuntimeError("Fixture is outside the private allocation")
    path = fixture
    for component in (None, "outside", "pig-platform", "go-mod", "sentinel"):
        if component:
            path = path / component
        info = path.lstat()
        if stat.S_ISLNK(info.st_mode) or info.st_uid != os.getuid():
            raise RuntimeError(f"Unowned or symlinked fixture component: {path}")
        if component == "sentinel":
            if not stat.S_ISREG(info.st_mode):
                raise RuntimeError("Outside sentinel is not a regular file")
        elif not stat.S_ISDIR(info.st_mode):
            raise RuntimeError(f"Fixture component is not a directory: {path}")
    expected = hashlib.sha256(SENTINEL).hexdigest()
    actual = hashlib.sha256(path.read_bytes()).hexdigest()
    if actual != expected or proof.get("replacement_sentinel_sha256") != expected:
        raise RuntimeError("Outside sentinel differs from its original content")
    return {"status": "verified", "fixture": str(fixture), "outside_sentinel": str(path), "sha256": actual}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--scratch", required=True, type=Path)
    parser.add_argument("--proof", required=True, type=Path)
    parser.add_argument("--report", required=True, type=Path)
    args = parser.parse_args()
    try:
        result = verify(args.scratch, args.proof)
    except (OSError, ValueError, KeyError, TypeError, RuntimeError) as error:
        result = {"status": "failed", "error": str(error)}
    args.report.write_text(json.dumps(result, indent=2) + "\n")
    if result["status"] != "verified":
        print(f"Outside sentinel verification failed: {result['error']}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
