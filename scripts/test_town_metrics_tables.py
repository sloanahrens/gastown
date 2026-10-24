"""Tests for town_metrics.tables: the per-landing join (synthetic inputs, hand-checked)."""
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from town_metrics import common, tables  # noqa: E402


class FakeDiff:
    def stats(self, rig, commit):
        if commit == "bad":
            return None
        return {"lines_added": 6, "lines_deleted": 4, "files_touched": 2, "file_entropy": 1.0, "package_entropy": 0.0}


LEDGER = [
    {"bead": "gt-1", "rig": "gastown", "landed_commit": "c1", "landed_at": "2026-10-01T00:00:00Z", "route": "daemon", "om_verdict": "approve", "gate_result": "build"},
    {"bead": "gt-2", "rig": "gastown", "landed_commit": "c2", "landed_at": "2026-10-14T00:00:00Z", "route": "overseer-manual", "om_verdict": "skipped", "gate_result": "build"},
    {"bead": "gt-3", "rig": "gastown", "landed_commit": "bad", "landed_at": "2026-10-15T00:00:00Z", "route": "daemon", "om_verdict": "approve", "gate_result": "build"},
]
REWORK = {"gt-1": {"attempts": 2, "rejected_before_landing": True}, "gt-2": {"attempts": 1, "rejected_before_landing": False},
          "gt-3": {"attempts": 1, "rejected_before_landing": False}}
BEADS = {b: {"id": b, "type": "task", "priority": 2, "labels": ["a", "b"]} for b in ("gt-1", "gt-2", "gt-3")}
USAGE = {"gt-1": {"model": "deepseek-flash", "seat": "s", "end": "x"}}
ERAS = {"gastown": [("2026-09-01T00:00:00Z", 0.7), ("2026-10-10T00:00:00Z", 0.75)]}


class TestLandings(unittest.TestCase):
    def build(self, dropped=None):
        return tables.build_landings(LEDGER, REWORK, FakeDiff(), BEADS, USAGE, {"gt-2"}, ERAS, {"gt-1"},
                                     dropped or common.Dropped())

    def test_one_row_per_ledger_row(self):
        self.assertEqual([r["bead"] for r in self.build()], ["gt-1", "gt-2", "gt-3"])

    def test_flags_and_regime(self):
        r = {x["bead"]: x for x in self.build()}
        self.assertEqual(r["gt-1"]["rejected_before_landing"], 1)
        self.assertEqual(r["gt-2"]["manual_rescue"], 1)
        self.assertEqual(r["gt-1"]["manual_rescue"], 0)
        self.assertEqual(r["gt-2"]["reached_overrun_stop"], 1)
        self.assertEqual(r["gt-1"]["om_threshold"], 0.7)
        self.assertEqual(r["gt-2"]["om_threshold"], 0.75)
        self.assertEqual(r["gt-1"]["dominant_model"], "deepseek-flash")
        self.assertEqual(r["gt-1"]["week"], "2026-09-28")
        self.assertEqual(r["gt-1"]["shaped"], 1)
        self.assertEqual(r["gt-1"]["lines_total"], 10)

    def test_unreadable_commit_is_flagged_not_zeroed(self):
        d = common.Dropped()
        r = {x["bead"]: x for x in self.build(d)}["gt-3"]
        self.assertEqual(r["unreadable"], 1)
        self.assertIsNone(r["lines_total"])
        self.assertEqual(d.count, 1)  # named in the footer, row still kept

    def test_explain_only_contains_only_post_hoc_columns(self):
        self.assertTrue(tables.EXPLAIN_ONLY <= set(tables.LANDING_COLUMNS))
        self.assertFalse(tables.EXPLAIN_ONLY & {"om_threshold", "dominant_model", "shaped", "type", "priority",
                                               "label_count", "model"})

    def test_sidecar_lists_explain_only(self):
        import json
        import tempfile
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "s.json")
            tables.write_landing_sidecar(p)
            with open(p) as f:
                self.assertEqual(set(json.load(f)["explain_only"]), tables.EXPLAIN_ONLY)


if __name__ == "__main__":
    unittest.main()
