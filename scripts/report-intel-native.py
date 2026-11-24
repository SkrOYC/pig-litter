#!/usr/bin/env python3
import argparse
from pathlib import Path


FILES = ["inputs.txt", "driver-commit.txt", "driver-tree.txt", "candidate-tree.txt", "summary.json"]
for case in ("startup-only", "deterministic-prompt"):
    for stage in ("before-exit", "after-exit", "after-close"):
        for source in ("iterm.txt", "ui.txt", "native.json"):
            FILES.append(f"{case}-{stage}-{source}")
FILES.append("after-control-cleanup-native.json")


def report(evidence):
    for name in FILES:
        path = evidence / name
        print(f"\n--- {name} ---")
        if path.is_file():
            print(path.read_text(errors="replace"), end="")
        else:
            print("unavailable")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--evidence", required=True, type=Path)
    args = parser.parse_args()
    report(args.evidence)


if __name__ == "__main__":
    main()
