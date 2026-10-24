"""Tests for the bead-side metrics (3, 5, 6, 7, 8, 9): synthetic inputs, hand-computed answers."""
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from town_metrics import metrics  # noqa: E402


class TestKinds(unittest.TestCase):
    def test_classify_maps_the_kind_word(self):
        c = metrics.classify_rejection
        self.assertEqual(c("conflict"), "conflict")
        self.assertEqual(c("gate"), "gate")
        self.assertEqual(c("review"), "review")
        self.assertEqual(c("policy"), "policy")
        self.assertEqual(c("race"), "race")
        self.assertEqual(c("GATE"), "gate")
        self.assertEqual(c("mystery"), "unknown")

    def test_non_conservative_share(self):
        out = metrics.non_conservative(["conflict", "gate", "review", "unknown"])
        self.assertEqual(out["rejections"], 4)
        self.assertEqual(out["conflict_gate_share"], 0.5)
        self.assertEqual(out["unknown_share"], 0.25)
        self.assertIsNone(metrics.non_conservative([])["conflict_gate_share"])


class TestEntropyAndDensity(unittest.TestCase):
    def test_label_entropy_two_buckets(self):
        # "b" is rare, so it folds into "other": labels are {a: 1, other: 1} -> 1 bit.
        beads = [{"labels": ["a"], "rig": "gt", "type": "task"}, {"labels": ["b"], "rig": "dv", "type": "task"}]
        out = metrics.label_entropy(beads, rare={"b"})
        self.assertEqual(out["h_labels"], 1.0)  # {a:1, other:1}
        self.assertEqual(out["h_rigs"], 1.0)
        self.assertEqual(out["h_types"], 0.0)

    def test_dependency_density(self):
        self.assertEqual(metrics.dependency_density([{"dependency_count": 2}, {"dependency_count": 4}]), 3.0)
        self.assertIsNone(metrics.dependency_density([]))

    def test_rho(self):
        out = metrics.rho([{"route": "daemon"}, {"route": "daemon"}, {"route": "overseer-manual"}], 1)
        self.assertEqual((out["numerator"], out["denominator"]), (2, 2))
        self.assertEqual(out["rho"], 1.0)
        self.assertIsNone(metrics.rho([{"route": "overseer-manual"}], 0)["rho"])

    def test_handoff_size(self):
        out = metrics.handoff_size(["a", "b", "c"], {"a": 100, "b": 300}, {"a": 1, "b": 3})
        self.assertEqual(out["handoff_bytes_total"], 400)
        self.assertEqual(out["handoff_bytes_median"], 200)
        self.assertEqual(out["sessions_median"], 2)
        self.assertIsNone(metrics.handoff_size([], {}, {})["handoff_bytes_median"])


class TestModeShift(unittest.TestCase):
    def test_distance_between_consecutive_weeks(self):
        series = [{"x": 0.0, "y": 0.0}, {"x": 1.0, "y": 0.0}, {"x": 1.0, "y": 0.0}]
        out = metrics.mode_shift(series, [False, False, False])
        self.assertIsNone(out[0])
        self.assertGreater(out[1], 0)
        self.assertEqual(out[2], 0.0)

    def test_thin_weeks_do_not_inflate_the_scale(self):
        # the noisy thin week (x=100) must not widen sd: the 0 -> 1 step stays large relative to the non-thin spread
        series = [{"x": 0.0}, {"x": 1.0}, {"x": 100.0}, {"x": 0.0}, {"x": 1.0}]
        with_thin = metrics.mode_shift(series, [False, False, True, False, False])
        without = metrics.mode_shift([series[0], series[1]], [False, False])
        self.assertAlmostEqual(with_thin[1], without[1])

    def test_a_gap_in_the_weeks_gives_none(self):
        series = [{"x": 0.0}, {"x": 5.0}, {"x": 6.0}]
        out = metrics.mode_shift(series, [False] * 3, weeks=["2026-09-07", "2026-09-14", "2026-10-05"])
        self.assertIsNotNone(out[1])
        self.assertIsNone(out[2])  # 3 weeks between the last two points

    def test_thin_or_missing_week_gives_none(self):
        series = [{"x": 0.0}, {"x": 5.0}, {"x": None}]
        out = metrics.mode_shift(series, [False, True, False])
        self.assertEqual(out, [None, None, None])


if __name__ == "__main__":
    unittest.main()
