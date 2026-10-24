"""Tests for the per-landing weekly metrics (1, 2, 4): synthetic rows with hand-computed answers."""
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from town_metrics import metrics  # noqa: E402


def L(bead, week, rig="gastown", total=10, fe=1.0, pe=0.0, att=1, rej=0, man=0):
    if total is None:  # an unreadable commit has no diff columns at all (tables.build_landings)
        fe = pe = None
    return {"bead": bead, "week": week, "rig": rig, "lines_total": total, "file_entropy": fe, "package_entropy": pe,
            "attempts": att, "rejected_before_landing": rej, "manual_rescue": man, "unreadable": int(total is None)}


class TestWeekly(unittest.TestCase):
    def test_percentile_interpolates(self):
        self.assertEqual(metrics.percentile([1, 2, 3, 4], 0.5), 2.5)
        self.assertEqual(metrics.percentile([5], 0.9), 5)
        self.assertAlmostEqual(metrics.percentile([0, 10], 0.9), 9.0)

    def test_size_and_rework_by_week_rig_and_town(self):
        rows = [L("a", "w1", total=10, att=1), L("b", "w1", total=30, att=3, rej=1),
                L("c", "w1", rig="devops", total=20, man=1), L("d", "w1", total=None)]
        out = metrics.weekly_landing_metrics(rows)
        g = out[("w1", "gastown")]
        self.assertEqual(g["landings"], 3)
        self.assertEqual(g["unreadable"], 1)
        self.assertEqual(g["size_median"], 20)  # readable: 10, 30
        self.assertAlmostEqual(g["attempts_mean"], 5 / 3)
        self.assertAlmostEqual(g["rejected_share"], 1 / 3)
        self.assertAlmostEqual(g["first_try_share"], 2 / 3)
        town = out[("w1", "ALL")]
        self.assertEqual(town["landings"], 4)
        self.assertEqual(town["manual_rescue_count"], 1)
        self.assertTrue(town["thin"])

    def test_no_readable_diff_gives_none_not_zero(self):
        out = metrics.weekly_landing_metrics([L("a", "w1", total=None)])
        self.assertIsNone(out[("w1", "gastown")]["size_median"])
        self.assertIsNone(out[("w1", "gastown")]["file_entropy_mean"])

    def test_ten_landings_is_not_thin(self):
        out = metrics.weekly_landing_metrics([L(str(i), "w1") for i in range(10)])
        self.assertFalse(out[("w1", "ALL")]["thin"])


if __name__ == "__main__":
    unittest.main()
