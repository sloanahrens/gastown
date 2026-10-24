"""Tests for town_metrics.common. Run: python3 -I -m unittest discover -s scripts -p 'test_town_metrics_*.py'"""
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from town_metrics import common  # noqa: E402


class TestCommon(unittest.TestCase):
    def test_week_of_is_monday(self):
        self.assertEqual(common.week_of("2026-10-23T18:49:02Z"), "2026-10-19")  # Friday
        self.assertEqual(common.week_of("2026-10-19T00:00:00Z"), "2026-10-19")  # Monday
        self.assertEqual(common.week_of("2026-10-25T23:59:59.5Z"), "2026-10-19")  # Sunday

    def test_entropy(self):
        self.assertEqual(common.entropy([]), 0.0)
        self.assertEqual(common.entropy([7]), 0.0)
        self.assertEqual(common.entropy([1, 1]), 1.0)
        self.assertAlmostEqual(common.entropy([1, 1, 1, 1]), 2.0)
        self.assertEqual(common.entropy([0, 5]), 0.0)  # zero weights ignored

    def test_csv_round_trip_keeps_column_order_and_empty(self):
        with tempfile.TemporaryDirectory() as d:
            p = os.path.join(d, "t.csv")
            common.write_csv(p, [{"a": 1, "b": None}, {"a": 2, "b": "x,y"}], ["b", "a"])
            with open(p) as f:
                self.assertEqual(f.readline().strip(), "b,a")
            self.assertEqual(common.read_csv(p), [{"b": "", "a": "1"}, {"b": "x,y", "a": "2"}])

    def test_dropped_counts(self):
        d = common.Dropped()
        d.add("ledger:gastown", "bad json")
        self.assertEqual(d.count, 1)
        self.assertEqual(d.items, [("ledger:gastown", "bad json")])

    def test_fail_records_a_structured_failed_source(self):
        d = common.Dropped()
        d.fail("bd:/g", "bd list failed in /g")
        d.add("ledger:x", "bad")
        self.assertEqual(d.failed, ["bd:/g"])
        self.assertEqual(d.count, 2)  # a failure is also a logged gap


if __name__ == "__main__":
    unittest.main()
