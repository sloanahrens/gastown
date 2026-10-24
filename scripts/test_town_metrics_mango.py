"""Tests for town_metrics.mango_run: split, bins, sidecar drop, holdout check, and the exec up to a fake binary."""
import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from town_metrics import common, mango_run, tables  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
FAKE = os.path.join(HERE, "testdata", "town_metrics", "fake-mango")


def landing(i, rig="a", rej=0, at=None, route="daemon", **kw):
    r = {c: "" for c in tables.LANDING_COLUMNS}
    r.update({"bead": "b%d" % i, "rig": rig, "landed_at": at or "2026-10-%02dT00:00:00Z" % (1 + i % 28),
              "route": route, "rejected_before_landing": str(rej), "manual_rescue": str(int(route != "daemon")),
              "reached_overrun_stop": "0", "lines_total": "10", "om_threshold": "0.7", "dominant_model": "m",
              "type": "task", "priority": "2", "shaped": "1", "label_count": str(i % 5)})
    r.update(kw)
    return r


class TestSplitAndBins(unittest.TestCase):
    def test_split_is_by_date_not_input_order(self):
        rows = [landing(i, at="2026-10-%02dT00:00:00Z" % d) for i, d in enumerate([20, 5, 15, 10, 25, 1, 30, 12, 8, 18])]
        train, hold = mango_run.split_by_date(rows, 0.7)
        self.assertEqual((len(train), len(hold)), (7, 3))
        self.assertLess(max(r["landed_at"] for r in train), min(r["landed_at"] for r in hold))

    def test_cuts_come_from_train_only(self):
        train = [{"x": str(v)} for v in (1, 2, 3, 4, 5, 6, 7, 8)]
        cuts = mango_run.quartile_cuts(train, "x")
        self.assertEqual(cuts, [3.0, 5.0, 7.0])  # a holdout outlier is never part of this input
        self.assertEqual([mango_run.bin_value(v, cuts) for v in ("1", "3", "5", "9", "")],
                         ["q1", "q2", "q3", "q4", "none"])

    def test_too_few_values_gives_no_cuts(self):
        self.assertEqual(mango_run.quartile_cuts([{"x": "1"}, {"x": ""}], "x"), [])


class TestPrepare(unittest.TestCase):
    def setUp(self):
        self.rows = [landing(i, rej=i % 2, at="2026-10-%02dT00:00:00Z" % (1 + i)) for i in range(10)]

    def test_predictors_exclude_post_hoc_other_targets_identity_and_leaks(self):
        p = mango_run.prepare(self.rows, tables.EXPLAIN_ONLY, "manual_rescue")
        preds = set(p["predictors"])
        self.assertFalse(preds & tables.EXPLAIN_ONLY)
        self.assertFalse(preds & {"rejected_before_landing", "reached_overrun_stop", "bead", "landed_at", "week", "model"})
        self.assertNotIn("route", preds)  # route defines manual_rescue: a box on it would restate the target
        self.assertIn("om_threshold", preds)  # regime columns are predictors
        self.assertEqual(p["columns"], p["predictors"] + ["manual_rescue"])

    def test_route_is_a_predictor_for_other_targets(self):
        self.assertIn("route", mango_run.prepare(self.rows, tables.EXPLAIN_ONLY, "rejected_before_landing")["predictors"])

    def test_train_and_holdout_partition_the_rows(self):
        p = mango_run.prepare(self.rows, tables.EXPLAIN_ONLY, "rejected_before_landing")
        self.assertEqual(len(p["train"]) + len(p["holdout"]), 10)

    def test_positives_in_train_are_counted(self):
        p = mango_run.prepare(self.rows, tables.EXPLAIN_ONLY, "rejected_before_landing")
        self.assertEqual(p["train_positives"], sum(r["rejected_before_landing"] == "1" for r in p["train"]))


class TestHoldout(unittest.TestCase):
    def rows(self, n_box, k_box, n_rest, k_rest):
        rows = []
        for i in range(n_box):
            rows.append({"rig": "a", "om_threshold": "0.7", "t": "1" if i < k_box else "0", "lines_total": "100"})
        for i in range(n_rest):
            rows.append({"rig": "b", "om_threshold": "0.7", "t": "1" if i < k_rest else "0", "lines_total": "10"})
        return rows

    def box(self, column="rig", cats=("a",)):
        return {"ranges": [{"column": column, "categories": list(cats)}], "n": 50, "purity": 0.5}

    def test_small_box_does_not_replicate_and_numbers_match_hand_calc(self):
        # holdout: 10 rows, 4 positive overall (baseline 0.4); box a has 4 rows, 3 positive.
        out = mango_run.check_holdout([self.box()], self.rows(4, 3, 6, 1), "t", min_support=2, explain=["lines_total"])
        b = out[0]
        self.assertEqual((b["n"], b["k"]), (4, 3))
        self.assertAlmostEqual(b["rate"], 0.75)
        self.assertAlmostEqual(b["baseline"], 0.4)
        self.assertAlmostEqual(b["p_holm"], 0.3088, places=3)  # 0.1296 + 0.1536 + 0.0256 by hand
        self.assertFalse(b["replicates"])

    def test_strong_box_replicates_and_is_explained_by_means(self):
        out = mango_run.check_holdout([self.box()], self.rows(20, 17, 20, 0), "t", min_support=20, explain=["lines_total"])
        b = out[0]
        self.assertTrue(b["replicates"])
        self.assertEqual(b["explain"]["lines_total"], (100.0, 55.0))  # box mean vs whole-holdout mean

    def test_support_floor_blocks_replication(self):
        out = mango_run.check_holdout([self.box()], self.rows(20, 17, 20, 0), "t", min_support=30, explain=[])
        self.assertFalse(out[0]["replicates"])

    def test_box_on_regime_columns_is_labelled_period(self):
        out = mango_run.check_holdout([self.box("om_threshold", ("0.7",))], self.rows(4, 3, 6, 1), "t", 2, [])
        self.assertEqual(out[0]["label"], "period")
        out = mango_run.check_holdout([self.box()], self.rows(4, 3, 6, 1), "t", 2, [])
        self.assertEqual(out[0]["label"], "")


class TestReplicateApi(unittest.TestCase):
    def test_the_reused_script_still_offers_what_check_holdout_calls(self):
        mod = mango_run._replicate_module()
        for name in ("matches", "binom_two_sided", "holm", "describe"):
            self.assertTrue(callable(getattr(mod, name, None)), name)


class TestExec(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="tm-mango-")
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.log = os.path.join(self.tmp, "argv.log")
        self.result = os.path.join(self.tmp, "canned.json")
        with open(self.result, "w") as f:
            json.dump({"target": "t", "positive": 1, "baseline": 0.1, "rows": 100, "hypercubes": [],
                       "stats": {"generate": {"iterations": 10, "germsTried": 200, "generated": 200, "valid": 0,
                                              "significant": 0, "unique": 0,
                                              "skippedColumns": [{"column": "x", "reason": "too few distinct values"}]},
                                 "candidates": 200}}, f)
        with open(os.path.join(self.tmp, "train.csv"), "w") as f:
            f.write("a,t\n1,0\n")

    def runner(self, argv):
        env = dict(os.environ, FAKE_MANGO_LOG=self.log, FAKE_MANGO_RESULT=self.result)
        r = subprocess.run(argv, capture_output=True, text=True, env=env, timeout=30)
        return r.returncode, r.stdout

    def test_widen_flag_is_left_out_when_off(self):
        mango_run.run_mango(FAKE, self.tmp, "t", self.runner, min_support=30, widen=False)
        with open(self.log) as f:
            explore = [ln.split() for ln in f.read().splitlines()][1]
        self.assertNotIn("--widen", explore)

    def test_argv_sequence_and_parsed_search_stats(self):
        out = mango_run.run_mango(FAKE, self.tmp, "t", self.runner, min_support=30)
        with open(self.log) as f:
            calls = [ln.split() for ln in f.read().splitlines()]
        self.assertEqual([c[0] for c in calls], ["ingest", "explore", "report"])
        explore = calls[1]
        self.assertEqual(explore[explore.index("--min-size") + 1], "30")
        self.assertEqual(explore[explore.index("--target") + 1], "t")
        self.assertIn("--widen", explore)
        self.assertEqual((out["iterations"], out["germs"], out["candidates"], out["valid"]), (10, 200, 200, 0))
        self.assertEqual(out["skipped_columns"], [("x", "too few distinct values")])
        self.assertEqual(out["boxes"], [])
        self.assertIn("# fake report", out["report_md"])

    def test_nonzero_exit_raises_with_output_tail(self):
        with self.assertRaises(mango_run.MangoError) as cm:
            mango_run.run_mango(FAKE, self.tmp, "t", lambda argv: (2, "boom: bad parquet"), min_support=30)
        self.assertIn("boom: bad parquet", str(cm.exception))

    def test_missing_binary_is_reported_not_raised(self):
        self.assertIsNone(mango_run.find_binary("definitely-not-a-binary-xyz", path=""))


class TestSection(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="tm-section-")
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        rows = [landing(i, rej=i % 2, at="2026-10-%02dT00:00:00Z" % (1 + i % 28)) for i in range(40)]
        common.write_csv(os.path.join(self.tmp, "landings.csv"), rows, tables.LANDING_COLUMNS)
        tables.write_landing_sidecar(os.path.join(self.tmp, "landings.sidecar.json"))
        common.write_csv(os.path.join(self.tmp, "weekly.csv"), [{"week": "w", "rig": "ALL"}], ["week", "rig"])
        self.log = os.path.join(self.tmp, "argv.log")
        self.result = os.path.join(self.tmp, "canned.json")
        with open(self.result, "w") as f:
            json.dump({"target": "t", "positive": 1, "baseline": 0.1, "rows": 28, "hypercubes": [],
                       "stats": {"generate": {"iterations": 10, "germsTried": 200, "generated": 200, "valid": 0,
                                              "significant": 0, "skippedColumns": []}, "candidates": 200}}, f)

    def runner(self, argv):
        env = dict(os.environ, FAKE_MANGO_LOG=self.log, FAKE_MANGO_RESULT=self.result)
        r = subprocess.run(argv, capture_output=True, text=True, env=env, timeout=30)
        return r.returncode, r.stdout

    def test_no_binary_says_so_and_names_the_csv_dir(self):
        text = mango_run.mango_section(self.tmp, None, self.runner)
        self.assertIn("mango binary not found", text)
        self.assertIn(self.tmp, text)

    def test_zero_valid_boxes_is_not_reported_as_nothing_predictive(self):
        text = mango_run.mango_section(self.tmp, FAKE, self.runner, min_positives=1)
        self.assertIn("### rejected_before_landing", text)
        self.assertIn("200 cubes generated", text)
        self.assertIn("0 valid", text)
        self.assertIn("not evidence that nothing is predictive", text)
        self.assertIn("profile of", text)  # weekly profile embedded
        with open(os.path.join(self.tmp, "mango-summary.json")) as f:
            summary = json.load(f)
        self.assertEqual(summary["rejected_before_landing"], {"boxes": 0, "replicating": 0, "holdout_positives": 6})

    def test_profile_output_does_not_embed_the_local_run_path(self):
        text = mango_run.mango_section(self.tmp, FAKE, self.runner, min_positives=1)
        self.assertNotIn(self.tmp, text.split("### rejected_before_landing")[0])
        self.assertIn("profile of <run>/weekly.csv", text)

    def test_target_with_too_few_positives_is_skipped_and_named(self):
        text = mango_run.mango_section(self.tmp, FAKE, self.runner, min_positives=30)
        self.assertIn("reached_overrun_stop", text)
        self.assertIn("Skipped: fewer than 30 positives in train", text)

    def test_holdout_without_positives_says_so_instead_of_checking(self):
        rows = [landing(i, route="overseer-manual" if i < 10 else "daemon", at="2026-10-%02dT00:00:00Z" % (1 + i % 28))
                for i in range(40)]
        # positives only in the first ten dates -> all in train under a date split
        for i, r in enumerate(rows):
            r["landed_at"] = "2026-10-%02dT00:00:00Z" % (1 + i) if i < 28 else "2026-11-%02dT00:00:00Z" % (i - 27)
        common.write_csv(os.path.join(self.tmp, "landings.csv"), rows, tables.LANDING_COLUMNS)
        with open(self.result, "w") as f:
            json.dump({"target": "manual_rescue", "positive": 1, "baseline": 0.1, "rows": 28,
                       "hypercubes": [{"ranges": [{"column": "rig", "categories": ["a"]}], "n": 28, "purity": 0.4}],
                       "stats": {"generate": {"iterations": 10, "germsTried": 200, "generated": 50, "valid": 1,
                                              "significant": 1, "skippedColumns": []}, "candidates": 1}}, f)
        text = mango_run.mango_section(self.tmp, FAKE, self.runner, min_positives=1)
        part = text[text.index("### manual_rescue"):text.index("### reached_overrun_stop")]
        self.assertIn("Holdout has no positives", part)
        self.assertIn("Last positive landed 2026-10-10", part)
        self.assertNotIn("| replicates |", part)

    def test_a_mango_failure_is_reported_in_the_section(self):
        text = mango_run.mango_section(self.tmp, FAKE, lambda argv: (2, "kaboom"), min_positives=1)
        self.assertIn("mango ingest failed (exit 2): kaboom", text)


if __name__ == "__main__":
    unittest.main()
