"""Tests for the ledger and reviews loaders in town_metrics.loaders (fixtures only; no ~/gt)."""
import json
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from town_metrics import common, loaders  # noqa: E402

LEDGER = [
    json.dumps({"bead": "gt-1", "rig": "gastown", "landed_commit": "abc", "landed_at": "2026-10-01T00:00:00Z",
                "route": "daemon", "om_verdict": "approve", "gate_result": "build+lint"}),
    "",
    "{not json",
    json.dumps({"rig": "gastown", "landed_at": "2026-10-01T00:00:00Z"}),  # no bead
]
REVIEWS = [
    json.dumps({"bead": "gt-1", "rig": "gastown", "attempt": 1, "score": 0.6, "verdict": "request_changes", "at": "2026-09-30T23:00:00Z"}),
    json.dumps({"bead": "gt-1", "rig": "gastown", "attempt": 2, "score": 0.9, "verdict": "approve", "at": "2026-09-30T23:30:00Z"}),
    json.dumps({"bead": "gt-2", "rig": "gastown", "attempt": 1, "score": 0.9, "verdict": "approve", "at": "2026-10-02T00:00:00Z"}),
    json.dumps({"bead": "gt-3", "rig": "gastown", "attempt": 1, "score": 0.5, "verdict": "request_changes", "at": "2026-10-05T00:00:00Z"}),
    json.dumps({"bead": "gt-5", "rig": "gastown", "attempt": 1, "score": 0.5, "verdict": "request_changes", "at": "2026-10-06T00:00:00Z"}),
]


class TestLedger(unittest.TestCase):
    def test_rows_and_drops_account_for_every_nonblank_line(self):
        d = common.Dropped()
        rows = loaders.load_ledger({"gastown": LEDGER}, d)
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]["bead"], "gt-1")
        self.assertEqual(d.count, 2)
        self.assertEqual(len(rows) + d.count, 3)  # 3 non-blank lines


class TestReviews(unittest.TestCase):
    def test_rework_by_bead(self):
        d = common.Dropped()
        rv = loaders.load_reviews(REVIEWS, d)
        landed = {"gt-1": "2026-10-01T00:00:00Z", "gt-2": "2026-10-02T01:00:00Z", "gt-4": "2026-10-03T00:00:00Z",
                  "gt-5": "2026-10-05T00:00:00Z"}
        r = loaders.rework_by_bead(rv, landed)
        self.assertEqual(r["gt-1"], {"attempts": 2, "rejected_before_landing": True})
        self.assertEqual(r["gt-2"], {"attempts": 1, "rejected_before_landing": False})
        self.assertEqual(r["gt-4"], {"attempts": 1, "rejected_before_landing": False})  # no review rows (om skipped)
        self.assertNotIn("gt-3", r)  # never landed
        # a rejection dated after the landing is not "before landing"
        self.assertFalse(r["gt-5"]["rejected_before_landing"])

    def test_note_rejections_cover_history_before_reviews_jsonl(self):
        landed = {"gt-7": "2026-09-20T00:00:00Z", "gt-8": "2026-09-20T00:00:00Z"}
        r = loaders.rework_by_bead([], landed, note_kinds={"gt-7": ["gate", "review"]})
        self.assertEqual(r["gt-7"], {"attempts": 3, "rejected_before_landing": True})
        self.assertEqual(r["gt-8"], {"attempts": 1, "rejected_before_landing": False})

    def test_attempts_is_the_larger_of_reviews_and_notes(self):
        rv = loaders.load_reviews(REVIEWS[:2], common.Dropped())  # gt-1 reaches attempt 2
        r = loaders.rework_by_bead(rv, {"gt-1": "2026-10-01T00:00:00Z"}, note_kinds={"gt-1": ["gate"]})
        self.assertEqual(r["gt-1"]["attempts"], 2)

    def test_bad_review_row_is_dropped_not_zeroed(self):
        d = common.Dropped()
        rows = loaders.load_reviews(['{"bead": "x"}', "nope"], d)
        self.assertEqual(rows, [])
        self.assertEqual(d.count, 2)


if __name__ == "__main__":
    unittest.main()
