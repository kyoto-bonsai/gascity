#!/usr/bin/env python3
"""Reconcile a sharded fast gate against its frozen package and test inventory."""

from __future__ import annotations

import collections
import json
import sys
from pathlib import Path


class GateError(Exception):
    pass


def lines(path: Path) -> list[str]:
    if not path.is_file():
        raise GateError(f"missing inventory: {path}")
    result = path.read_text().splitlines()
    if not result or any(not item for item in result) or len(result) != len(set(result)):
        raise GateError(f"empty or duplicate inventory: {path}")
    return result


def events(path: Path):
    if not path.is_file() or path.stat().st_size == 0:
        raise GateError(f"missing or empty raw JSONL: {path}")
    with path.open() as stream:
        for number, line in enumerate(stream, 1):
            try:
                yield json.loads(line)
            except (json.JSONDecodeError, UnicodeDecodeError) as error:
                raise GateError(f"malformed {path}:{number}: {error}") from error


def require_exit(log_dir: Path, label: str):
    path = log_dir / f"{label}.exit"
    if not path.is_file():
        raise GateError(f"missing child exit: {path}")
    if path.read_text().strip() != "0":
        raise GateError(f"nonzero child exit: {path}: {path.read_text().strip()}")
    log = log_dir / f"{label}.log"
    if not log.is_file():
        raise GateError(f"missing child output log: {log}")


def terminal_inventory(path: Path, expected_packages: set[str], expected_tests: set[str] | None):
    package_actions: dict[str, list[str]] = collections.defaultdict(list)
    test_actions: dict[str, list[str]] = collections.defaultdict(list)
    for event in events(path):
        package = event.get("Package")
        test = event.get("Test")
        action = event.get("Action")
        if action not in ("pass", "skip", "fail"):
            continue
        if package not in expected_packages:
            raise GateError(f"unexpected package in {path}: {package}")
        if action == "fail":
            raise GateError(f"terminal FAIL in {path}: {package} {test or '<package>'}")
        if test is None:
            package_actions[package].append(action)
        elif expected_tests is not None and "/" not in test:
            test_actions[test].append(action)
    if set(package_actions) != expected_packages:
        raise GateError(f"package terminal mismatch in {path}: missing={sorted(expected_packages - set(package_actions))} extra={sorted(set(package_actions) - expected_packages)}")
    if any(actions != ["pass"] and actions != ["skip"] for actions in package_actions.values()):
        raise GateError(f"duplicate or invalid package terminal in {path}")
    if expected_tests is not None:
        if set(test_actions) != expected_tests:
            raise GateError(f"test terminal mismatch in {path}: missing={sorted(expected_tests - set(test_actions))[:10]} extra={sorted(set(test_actions) - expected_tests)[:10]}")
        if any(len(actions) != 1 for actions in test_actions.values()):
            raise GateError(f"duplicate top-level terminal in {path}")
    return collections.Counter(actions[0] for actions in package_actions.values()), collections.Counter(actions[0] for actions in test_actions.values())


def verify(log_dir: Path, total: int):
    if total < 1:
        raise GateError("shard total must be positive")
    all_packages = lines(log_dir / "all.packages")
    noncmd = lines(log_dir / "noncmd.packages")
    manifest = lines(log_dir / "cmdgc.manifest")
    cmdgc = "github.com/gastownhall/gascity/cmd/gc"
    if set(all_packages) != set(noncmd) | {cmdgc} or cmdgc in noncmd:
        raise GateError("package partition does not cover discovered inventory exactly once")
    if any(not name.startswith("Test") for name in manifest):
        raise GateError("unsupported entry in cmd/gc manifest")

    require_exit(log_dir, "fsys-darwin-compile")
    require_exit(log_dir, "unit-core")
    raw_paths = [log_dir / "unit-core.jsonl"] + [log_dir / f"cmdgc-{index}.jsonl" for index in range(1, total + 1)]
    if len({path.resolve() for path in raw_paths}) != len(raw_paths):
        raise GateError("raw JSONL paths overlap")
    package_counts, _ = terminal_inventory(raw_paths[0], set(noncmd), None)
    cmdgc_counts = collections.Counter()
    selected_union = []
    for index in range(1, total + 1):
        require_exit(log_dir, f"unit-cmd-gc-{index}-of-{total}")
        selected = manifest[index - 1::total]
        if not selected:
            raise GateError(f"empty cmd/gc shard {index}")
        selected_union.extend(selected)
        package_result, test_result = terminal_inventory(raw_paths[index], {cmdgc}, set(selected))
        package_counts.update(package_result)
        cmdgc_counts.update(test_result)
    if len(selected_union) != len(manifest) or set(selected_union) != set(manifest):
        raise GateError("cmd/gc partition omits or duplicates discovered tests")
    summary = {
        "schema": 1,
        "all_packages": len(all_packages),
        "cmdgc_planned_top_level_tests": len(manifest),
        "cmdgc_terminal_top_level": dict(cmdgc_counts),
        "package_terminal": dict(package_counts),
        "raw_jsonl": [str(path) for path in raw_paths],
    }
    (log_dir / "gate-inventory.json").write_text(json.dumps(summary, indent=2) + "\n")
    return summary


def main():
    if len(sys.argv) != 3:
        print("usage: verify-sharded-gate.py <log-dir> <shard-total>", file=sys.stderr)
        return 2
    try:
        summary = verify(Path(sys.argv[1]), int(sys.argv[2]))
    except (GateError, OSError, ValueError) as error:
        print(f"gate: verification failed: {error}", file=sys.stderr)
        return 1
    print(f"gate: verified {summary['all_packages']} packages and {summary['cmdgc_planned_top_level_tests']} cmd/gc tests", file=sys.stderr)
    return 0


if __name__ == "__main__":
    sys.exit(main())
