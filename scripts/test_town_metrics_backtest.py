"""Tests for the usefulness backtest: synthetic series, hand-checked."""
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from town_metrics import backtest  # noqa: E402

WEEKS = ["2026-09-07", "2026-09-14", "2026-09-21", "2026-09-28", "2026-10-05", "2026-10-12", "2026-10-19", "2026-10-26"]


def rows(values, column="size_median", thin=None):
    thin = thin or [False] * len(values)
    return [{"week": w, "rig": "ALL", "thin": t, column: v} for w, v, t in zip(WEEKS, values, thin)]


class TestMovesAtEvent(unittest.TestCase):
    def test_jump_at_the_event_week_is_detected(self):
        r = rows([10, 10.2, 9.8, 10.1, 9.9, 10.0, 30, 30.1])  # the jump lands in the week of 2026-10-19
        self.assertTrue(backtest.moves_at_event(r, "size_median", "2026-10-21"))

    def test_same_jump_inside_normal_noise_is_not(self):
        r = rows([10, 30, 10, 30, 10, 30, 10, 30])
        self.assertFalse(backtest.moves_at_event(r, "size_median", "2026-10-19"))

    def test_too_few_changes_gives_none_not_false(self):
        self.assertIsNone(backtest.moves_at_event(rows([10, 11, 30]), "size_median", "2026-09-22"))

    def test_event_outside_the_series_is_none(self):
        self.assertIsNone(backtest.moves_at_event(rows([1, 2, 3, 4, 5, 6, 7, 8]), "size_median", "2026-01-01"))

    def test_thin_weeks_are_excluded_for_landing_metrics_only(self):
        thin = [True, True, True, False, False, False, False, False]
        self.assertEqual(len(backtest.series(rows([1] * 8, thin=thin), "size_median")), 5)
        # a bead-derived column is not thinned by the landing count
        self.assertEqual(len(backtest.series(rows([1] * 8, "h_labels", thin=thin), "h_labels")), 8)

    def test_flat_history_with_a_change_is_undefined_not_true(self):
        # every other week-to-week change is 0, so "two standard deviations" is 0 and the test says nothing
        r = rows([5, 5, 5, 5, 5, 5, 9, 9])
        self.assertIsNone(backtest.moves_at_event(r, "size_median", "2026-10-19"))


class TestCorrelationAndVerdict(unittest.TestCase):
    def test_perfectly_correlated_columns_are_found(self):
        r = [{"week": "w%d" % i, "rig": "gastown", "thin": False, "a": float(i), "b": 2.0 * i + 1, "c": float((i * 7) % 5)}
             for i in range(8)]
        pairs = backtest.correlated_pairs(r, ["a", "b", "c"], threshold=0.9)
        self.assertEqual([(x, y) for x, y, _ in pairs], [("a", "b")])
        self.assertAlmostEqual(pairs[0][2], 1.0)

    def test_pairs_with_fewer_than_six_shared_rows_are_skipped(self):
        r = [{"week": "w%d" % i, "rig": "x", "thin": False, "a": float(i), "b": float(i)} for i in range(5)]
        self.assertEqual(backtest.correlated_pairs(r, ["a", "b"]), [])

    def test_constant_column_never_correlates(self):
        r = [{"week": "w%d" % i, "rig": "x", "thin": False, "a": float(i), "b": 1.0} for i in range(8)]
        self.assertEqual(backtest.correlated_pairs(r, ["a", "b"]), [])

    def test_verdict_truth_table(self):
        v = backtest.verdict
        self.assertEqual(v(True, None, None), "keep")
        self.assertEqual(v(False, True, None), "keep")
        self.assertEqual(v(False, False, True), "keep")
        self.assertEqual(v(False, False, False), "drop")
        self.assertEqual(v(None, False, False), "undecided")   # no evidence yet is not evidence against
        self.assertEqual(v(False, None, False), "undecided")
        self.assertEqual(v(False, False, None), "undecided")   # Sloan not asked yet


class TestSection(unittest.TestCase):
    def test_section_lists_every_metric_and_says_when_history_is_too_short(self):
        weekly = rows([10, 10.2, 9.8, 10.1, 9.9, 10.0, 30, 30.1])
        events = [{"date": "2026-10-21", "what": "gate raised", "confirmed": False}]
        text = backtest.backtest_section(weekly, events, replicated={"rework depth": True}, sloan={})
        for name in backtest.METRICS:
            self.assertIn("| " + name + " |", text)
        self.assertIn("| rework depth |", text)
        self.assertIn("insufficient history", text)
        self.assertIn("chance of coincidence", text)
        self.assertIn("weak evidence", text)
        self.assertIn("unconfirmed", text)
        self.assertIn("2026-10-21", text)


class TestReplicatedFromSummary(unittest.TestCase):
    def test_maps_targets_to_metrics_and_keeps_unknown_as_none(self):
        summary = {"rejected_before_landing": {"boxes": 10, "replicating": 3, "holdout_positives": 158},
                   "manual_rescue": {"boxes": 6, "replicating": 0, "holdout_positives": 0},
                   "reached_overrun_stop": {"boxes": 0, "replicating": 0, "holdout_positives": 37}}
        got = backtest.replicated_from_summary(summary)
        self.assertEqual(got, {"rework depth": True})  # no holdout positives or no boxes: nothing to say

    def test_boxes_that_all_fail_the_holdout_are_false(self):
        got = backtest.replicated_from_summary({"rejected_before_landing": {"boxes": 4, "replicating": 0,
                                                                            "holdout_positives": 50}})
        self.assertEqual(got, {"rework depth": False})


class TestChance(unittest.TestCase):
    def test_chance_is_events_over_comparable_changes(self):
        r = rows([10, 10.2, 9.8, 10.1, 9.9, 10.0, 30, 30.1])  # 7 comparable changes
        events = [{"date": "2026-10-21"}, {"date": "2026-10-01"}]  # both fall in the series
        self.assertAlmostEqual(backtest.chance_of_coincidence(r, "size_median", events), 2 / 7)

    def test_none_when_the_series_is_too_short(self):
        self.assertIsNone(backtest.chance_of_coincidence(rows([1, 2, 3]), "size_median", [{"date": "2026-09-22"}]))


class TestWeakEvidence(unittest.TestCase):
    def test_a_move_with_high_chance_of_coincidence_is_shown_weak_and_does_not_keep(self):
        weekly = rows([10, 10.2, 9.8, 10.1, 9.9, 10.0, 30, 30.1])
        events = [{"date": "2026-10-21", "what": "x", "confirmed": True}, {"date": "2026-10-01", "what": "y", "confirmed": True},
                  {"date": "2026-09-22", "what": "z", "confirmed": True}]  # 3 events / 7 changes > 25%
        text = backtest.backtest_section(weekly, events, replicated={}, sloan={"landing size": False})
        line = next(l for l in text.splitlines() if l.startswith("| landing size |"))
        self.assertIn("yes (weak)", line)
        self.assertTrue(line.rstrip().endswith("| undecided |") or line.rstrip().endswith("| drop |"), line)
        self.assertNotIn("keep", line)

    def test_a_move_with_low_chance_still_keeps(self):
        weekly = rows([10, 10.2, 9.8, 10.1, 9.9, 10.0, 30, 30.1])
        text = backtest.backtest_section(weekly, [{"date": "2026-10-21", "what": "x", "confirmed": True}], {}, {})
        line = next(l for l in text.splitlines() if l.startswith("| landing size |"))
        self.assertIn("| yes |", line)
        self.assertTrue(line.rstrip().endswith("| keep |"), line)
