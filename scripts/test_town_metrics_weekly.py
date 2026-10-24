"""Tests for build_weekly and moved_since_last."""
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from town_metrics import metrics, render  # noqa: E402


def W(week, v, thin=False, rig="ALL"):
    return {"week": week, "rig": rig, "size_median": v, "thin": thin}


class TestMoved(unittest.TestCase):
    HIST = [W("w%d" % i, v) for i, v in enumerate([10, 11, 9, 10, 11, 9, 10, 10])]

    def test_flags_two_sigma_move(self):
        got = render.moved_since_last(self.HIST, self.HIST + [W("w9", 30)], ["size_median"])
        self.assertEqual([(g["week"], g["column"]) for g in got], [("w9", "size_median")])

    def test_quiet_week_does_not_flag(self):
        self.assertEqual(render.moved_since_last(self.HIST, self.HIST + [W("w9", 10.5)], ["size_median"]), [])

    def test_thin_latest_never_flags(self):
        self.assertEqual(render.moved_since_last(self.HIST, self.HIST + [W("w9", 30, thin=True)], ["size_median"]), [])

    def test_short_history_never_flags(self):
        short = self.HIST[:4]
        self.assertEqual(render.moved_since_last(short, short + [W("w9", 30)], ["size_median"]), [])


def LAND(bead, week, rig, total, att=1, rej=0, route="daemon", model="m1", thr=0.7):
    return {"bead": bead, "week": week, "rig": rig, "lines_total": total, "file_entropy": 1.0, "package_entropy": 0.0,
            "attempts": att, "rejected_before_landing": rej, "manual_rescue": int(route != "daemon"), "unreadable": 0,
            "route": route, "om_threshold": thr, "dominant_model": model}


class TestBuildWeekly(unittest.TestCase):
    def rows(self):
        landings = [LAND("a", "2026-10-12", "gastown", 10), LAND("b", "2026-10-12", "devops", 20, att=2, rej=1,
                                                                 route="overseer-manual", model="m2")]
        beads = [
            {"id": "gt-1", "rig": "gastown", "created_at": "2026-10-13T00:00:00Z", "closed_at": "2026-10-14T00:00:00Z",
             "type": "task", "labels": ["handoff"], "dependency_count": 2},
            {"id": "gt-2", "rig": "gastown", "created_at": "2026-10-13T00:00:00Z", "closed_at": None,
             "type": "bug", "labels": [], "dependency_count": 0},
        ]
        return metrics.build_weekly(landings, beads, {"gt-1": 500}, {"gt-1": 3}, {("2026-10-12", "ALL"): ["conflict"]}, set())

    def test_rows_exist_for_each_rig_and_town_with_all_columns(self):
        rows = self.rows()
        self.assertEqual({(r["week"], r["rig"]) for r in rows},
                         {("2026-10-12", "gastown"), ("2026-10-12", "devops"), ("2026-10-12", "ALL")})
        for r in rows:
            self.assertEqual(set(metrics.WEEKLY_COLUMNS) - set(r), set())

    def test_town_row_values(self):
        town = next(r for r in self.rows() if r["rig"] == "ALL")
        self.assertEqual(town["rho_numerator"], 2)  # 1 non-daemon landing + 1 handoff bead closed
        self.assertEqual(town["rho_denominator"], 1)
        self.assertEqual(town["rho"], 2.0)
        self.assertEqual(town["dominant_model"], "m1")  # tie broken alphabetically
        self.assertEqual(town["om_threshold"], 0.7)
        self.assertEqual(town["handoff_bytes_total"], 500)
        self.assertEqual(town["dependency_density"], 2.0)  # only gt-1 closed that week
        self.assertEqual(town["conflict_gate_share"], 1.0)
        self.assertEqual(town["h_types"], 1.0)  # task and bug created that week

    def test_sorted_by_rig_then_week_and_mode_shift_is_none_for_one_week(self):
        rows = self.rows()
        self.assertEqual([(r["rig"], r["week"]) for r in rows], sorted((r["rig"], r["week"]) for r in rows))
        self.assertTrue(all(r["mode_shift"] is None for r in rows))

    def test_week_with_beads_but_no_landings_still_gets_a_row(self):
        beads = [{"id": "gt-9", "rig": "gastown", "created_at": "2026-10-20T00:00:00Z", "closed_at": None,
                  "type": "task", "labels": ["x"], "dependency_count": 0}]
        rows = metrics.build_weekly([], beads, {}, {}, {}, set())
        town = next(r for r in rows if r["rig"] == "ALL")
        self.assertEqual(town["week"], "2026-10-19")
        self.assertTrue(town["thin"])
        self.assertEqual(town["landings"], 0)
        self.assertIsNone(town["size_median"])


if __name__ == "__main__":
    unittest.main()
