#!/usr/bin/env python3
import argparse
import json
from pathlib import Path


FILES = ["inputs.txt", "driver-commit.txt", "driver-tree.txt", "candidate-tree.txt", "summary.json"]
for case in ("startup-only", "deterministic-prompt"):
    for stage in ("before-exit", "after-exit", "after-close"):
        for source in ("iterm.txt", "ui.txt", "native.json"):
            FILES.append(f"{case}-{stage}-{source}")
for stage in ("before-owned-close", "after-owned-close", "owned-close-exception"):
    for source in ("iterm.txt", "ui.txt", "native.json"):
        FILES.append(f"startup-only-{stage}-{source}")
FILES.extend(("after-control-cleanup-native.json", "control-left-live-native.json"))


def report(evidence):
    for name in FILES:
        path = evidence / name
        print(f"\n--- {name} ---")
        if path.is_file():
            print(path.read_text(errors="replace"), end="")
        else:
            print("unavailable")
    events = evidence / "events.jsonl"
    print("\n--- owned-zero events ---")
    if events.is_file():
        for line in events.read_text().splitlines():
            try:
                record = json.loads(line)
            except json.JSONDecodeError:
                print("unparseable event")
                continue
            if record.get("stage", "").startswith("owned-zero."):
                print(json.dumps(record, ensure_ascii=False))
    else:
        print("unavailable")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--evidence", required=True, type=Path)
    args = parser.parse_args()
    report(args.evidence)


if __name__ == "__main__":
    main()
