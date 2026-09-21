"""Failure-path checks for the immutable sharded unit gate inventory."""

import importlib.util
import json
import tempfile
import unittest
from pathlib import Path


SPEC = importlib.util.spec_from_file_location(
    "verify_sharded_gate", Path(__file__).with_name("verify-sharded-gate.py")
)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class ShardedGateVerifierTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.write("all.packages", "github.com/gastownhall/gascity/cmd/gc\nexample/other\n")
        self.write("noncmd.packages", "example/other\n")
        self.write("cmdgc.manifest", "TestA\nTestB\nTestC\nTestD\n")
        self.job("fsys-darwin-compile")
        self.job("unit-core")
        self.jsonl("unit-core.jsonl", [{"Package": "example/other", "Action": "pass"}])
        for index, names in ((1, ("TestA", "TestC")), (2, ("TestB", "TestD"))):
            self.job(f"unit-cmd-gc-{index}-of-2")
            events = [{"Package": "github.com/gastownhall/gascity/cmd/gc", "Test": name, "Action": "pass"} for name in names]
            events.append({"Package": "github.com/gastownhall/gascity/cmd/gc", "Action": "pass"})
            self.jsonl(f"cmdgc-{index}.jsonl", events)

    def write(self, name, content):
        (self.root / name).write_text(content)

    def jsonl(self, name, records):
        self.write(name, "".join(json.dumps(record) + "\n" for record in records))

    def job(self, label):
        self.write(f"{label}.exit", "0\n")
        self.write(f"{label}.log", "completed\n")

    def test_exact_partition_passes(self):
        result = MODULE.verify(self.root, 2)
        self.assertEqual(result["cmdgc_planned_top_level_tests"], 4)
        self.assertEqual(result["cmdgc_terminal_top_level"], {"pass": 4})

    def test_duplicate_or_missing_discovery_fails(self):
        self.write("cmdgc.manifest", "TestA\nTestA\n")
        with self.assertRaises(MODULE.GateError):
            MODULE.verify(self.root, 2)
        (self.root / "cmdgc.manifest").unlink()
        with self.assertRaises(MODULE.GateError):
            MODULE.verify(self.root, 2)

    def test_omitted_or_duplicate_partition_result_fails(self):
        self.jsonl("cmdgc-1.jsonl", [
            {"Package": "github.com/gastownhall/gascity/cmd/gc", "Test": "TestA", "Action": "pass"},
            {"Package": "github.com/gastownhall/gascity/cmd/gc", "Action": "pass"},
        ])
        with self.assertRaises(MODULE.GateError):
            MODULE.verify(self.root, 2)
        self.jsonl("cmdgc-1.jsonl", [
            {"Package": "github.com/gastownhall/gascity/cmd/gc", "Test": "TestA", "Action": "pass"},
            {"Package": "github.com/gastownhall/gascity/cmd/gc", "Test": "TestA", "Action": "pass"},
            {"Package": "github.com/gastownhall/gascity/cmd/gc", "Test": "TestC", "Action": "pass"},
            {"Package": "github.com/gastownhall/gascity/cmd/gc", "Action": "pass"},
        ])
        with self.assertRaises(MODULE.GateError):
            MODULE.verify(self.root, 2)

    def test_child_failure_or_missing_exit_fails(self):
        self.write("unit-cmd-gc-2-of-2.exit", "1\n")
        with self.assertRaises(MODULE.GateError):
            MODULE.verify(self.root, 2)
        (self.root / "unit-cmd-gc-2-of-2.exit").unlink()
        with self.assertRaises(MODULE.GateError):
            MODULE.verify(self.root, 2)

    def test_raw_logs_must_be_distinct(self):
        (self.root / "cmdgc-2.jsonl").unlink()
        (self.root / "cmdgc-2.jsonl").symlink_to(self.root / "cmdgc-1.jsonl")
        with self.assertRaises(MODULE.GateError):
            MODULE.verify(self.root, 2)


if __name__ == "__main__":
    unittest.main()
