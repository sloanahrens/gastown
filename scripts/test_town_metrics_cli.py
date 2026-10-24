"""Tests for cli.extract / cli.report over in-memory fake sources (no ~/gt, no bd, no git)."""
import json
import os
import shutil
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from town_metrics import cli, common, loaders  # noqa: E402

BASE = os.path.expanduser("~/gt/claude/crew")


def ledger_line(bead, rig="gastown", route="daemon", at="2026-10-14T00:00:00Z"):
    return json.dumps({"bead": bead, "rig": rig, "landed_commit": "c-" + bead, "landed_at": at, "route": route,
                       "om_verdict": "approve", "gate_result": "build"})


def review_line(bead, attempt, verdict, at):
    return json.dumps({"bead": bead, "rig": "gastown", "attempt": attempt, "score": 0.5, "verdict": verdict, "at": at})


class FakeDiff:
    cache = {}

    def stats(self, rig, commit):
        return {"lines_added": 5, "lines_deleted": 5, "files_touched": 1, "file_entropy": 0.0, "package_entropy": 0.0}


BD_LIST = json.dumps([
    {"id": "gt-a", "created_at": "2026-10-12T00:00:00Z", "closed_at": "2026-10-14T00:00:00Z", "issue_type": "task",
     "labels": ["handoff"], "dependency_count": 1, "comment_count": 0, "acceptance_criteria": "- [ ] x",
     "notes": "MERGE REJECTION (attempt 1): conflict"},
    {"id": "gt-b", "created_at": "2026-10-12T00:00:00Z", "closed_at": "2026-10-14T00:00:00Z", "issue_type": "task",
     "labels": [], "dependency_count": 3, "comment_count": 0},
])
BD_SHOW = json.dumps([{"notes": "handoff notes", "comments": []}])


def fake_bd(cwd, args):
    return BD_LIST if args[0] == "list" else BD_SHOW


class CliCase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="tm-cli-", dir=BASE)
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)

    def env(self, **over):
        args = dict(
            ledger_lines={"gastown": [ledger_line("gt-a"), ledger_line("gt-b", route="overseer-manual")]},
            review_lines=[],  # history before reviews.jsonl: the rejection comes from the bead notes only
            diff_source=FakeDiff(), run_bd=fake_bd, bead_dirs=["/g"], prefix_to_rig={"gt": "gastown"},
            usage_files={"gt-a": {"models": {"m": {"calls": 1}}, "sessions": [{}]}},
            overrun_states={"gt-a": {"gate_sling_at": "2026-10-13T00:00:00Z"},
                            "gt-b": {"stop_began_at": "2026-10-14T00:00:00Z"}},
            om_commits={"gastown": [("2026-09-01T00:00:00Z", '{"threshold": 0.7}')]},
            out_dir=os.path.join(self.tmp, "metrics", "2026-10-23"), docs_dir=os.path.join(self.tmp, "docs"),
            today="2026-10-23")
        args.update(over)
        return cli.Env(**args)


class TestExtract(CliCase):
    def test_writes_the_four_files_with_one_row_per_ledger_line(self):
        env = self.env()
        cli.extract(env)
        for name in ("landings.csv", "landings.sidecar.json", "beads.csv", "weekly.csv"):
            self.assertTrue(os.path.exists(os.path.join(env.out_dir, name)), name)
        rows = common.read_csv(os.path.join(env.out_dir, "landings.csv"))
        self.assertEqual([r["bead"] for r in rows], ["gt-a", "gt-b"])
        by = {r["bead"]: r for r in rows}
        self.assertEqual(by["gt-a"]["rejected_before_landing"], "1")  # from notes: reviews.jsonl is empty here
        self.assertEqual(by["gt-b"]["manual_rescue"], "1")
        self.assertEqual(by["gt-b"]["reached_overrun_stop"], "1")
        self.assertEqual(by["gt-a"]["reached_overrun_stop"], "0")  # tracked by the guard, never stopped
        self.assertEqual(by["gt-a"]["shaped"], "1")

    def test_corrupt_ledger_line_is_dropped_and_named(self):
        env = self.env(ledger_lines={"gastown": [ledger_line("gt-a"), "{broken"]})
        res = cli.extract(env)
        self.assertEqual(len(res["landings"]), 1)
        self.assertIn(("ledger:gastown", "unparseable json"), res["footer"]["rows_dropped"])
        self.assertEqual(res["footer"]["rows_read"]["ledger"], 2)

    def test_rejection_history_comes_from_notes_without_extra_bd_calls(self):
        calls = []

        def counting_bd(cwd, args):
            calls.append(args[0])
            return fake_bd(cwd, args)
        res = cli.extract(self.env(run_bd=counting_bd))
        a = next(l for l in res["landings"] if l["bead"] == "gt-a")
        self.assertEqual((a["rejected_before_landing"], a["attempts"]), (1, 2))
        self.assertEqual(calls.count("list"), 1)
        self.assertEqual(calls.count("show"), 1)  # only the handoff bead; rejections need no bd show

    def test_rejection_kind_reaches_the_weekly_table(self):
        res = cli.extract(self.env())
        town = next(r for r in res["weekly"] if r["rig"] == "ALL" and r["week"] == "2026-10-12")
        self.assertEqual(town["rejections"], 1)
        self.assertEqual(town["conflict_gate_share"], 1.0)

    def test_a_successful_bd_list_with_zero_beads_still_writes_beads_csv(self):
        env = self.env(run_bd=lambda cwd, args: "[]")
        res = cli.extract(env)
        self.assertTrue(os.path.exists(os.path.join(env.out_dir, "beads.csv")))
        self.assertEqual(res["footer"]["sources_unreadable"], [])

    def test_unreadable_diffs_are_gaps_not_dropped_rows(self):
        class NoDiff:
            cache = {}

            def stats(self, rig, commit):
                return None
        res = cli.extract(self.env(diff_source=NoDiff()))
        self.assertEqual(len(res["landings"]), 2)
        self.assertEqual(res["footer"]["rows_dropped"], [])
        self.assertEqual(len(res["footer"]["gaps"]), 2)

    def test_a_reviews_loader_that_loses_a_row_trips_the_assertion(self):
        lines = [review_line("gt-a", 1, "request_changes", "2026-10-13T00:00:00Z"), review_line("gt-b", 1, "approve", "2026-10-14T00:00:00Z")]

        def lossy(lines, dropped):
            return loaders.load_reviews(lines, dropped)[:-1]
        with self.assertRaises(cli.RowCountError) as cm:
            cli.extract(self.env(review_lines=lines), review_loader=lossy)
        self.assertIn("reviews", str(cm.exception))

    def test_failing_bd_leaves_no_beads_csv_and_names_the_source(self):
        env = self.env(run_bd=lambda cwd, args: None)
        res = cli.extract(env)
        self.assertFalse(os.path.exists(os.path.join(env.out_dir, "beads.csv")))
        self.assertTrue(os.path.exists(os.path.join(env.out_dir, "weekly.csv")))
        self.assertEqual(res["footer"]["sources_unreadable"], ["bd:/g"])
        town = next(r for r in res["weekly"] if r["rig"] == "ALL")
        self.assertIsNone(town["h_labels"])  # unknown, not zero
        self.assertIsNone(town["dependency_density"])

    def test_a_loader_that_loses_a_row_silently_trips_the_assertion(self):
        def lossy(sources, dropped):
            return loaders.load_ledger(sources, dropped)[:-1]
        with self.assertRaises(cli.RowCountError) as cm:
            cli.extract(self.env(), ledger_loader=lossy)
        self.assertIn("1 rows + 0 logged drops != 2 source lines", str(cm.exception))


class TestReport(CliCase):
    def test_report_is_written_under_docs_dir(self):
        env = self.env()
        res = cli.extract(env)
        path = cli.report(env, res)
        self.assertEqual(path, os.path.join(env.docs_dir, "2026-10-23-town-metrics.md"))
        with open(path) as f:
            text = f.read()
        self.assertTrue(text.startswith("# Town metrics 2026-10-23\n"))
        self.assertIn("Rows dropped: 0", text)

    def test_typed_weekly_round_trips_through_csv(self):
        env = self.env()
        res = cli.extract(env)
        back = cli.typed_weekly(common.read_csv(os.path.join(env.out_dir, "weekly.csv")))
        a = next(r for r in res["weekly"] if r["rig"] == "ALL")
        b = next(r for r in back if r["rig"] == "ALL")
        for k in ("landings", "size_median", "rho", "thin", "week"):
            self.assertEqual(a[k], b[k], k)

    def test_standalone_report_rebuilds_from_the_output_directory(self):
        env = self.env()
        res = cli.extract(env)
        again = cli.load_result(env.out_dir)
        self.assertEqual(again["footer"], res["footer"] | {"rows_dropped": [tuple(d) for d in res["footer"]["rows_dropped"]]})
        self.assertEqual(len(again["weekly"]), len(res["weekly"]))

    def test_report_embeds_the_mango_section_when_one_was_written(self):
        env = self.env()
        res = cli.extract(env)
        with open(os.path.join(env.out_dir, "mango-section.md"), "w") as f:
            f.write("### rejected_before_landing\n\nNo boxes: marker-text")
        with open(cli.report(env, res)) as f:
            self.assertIn("No boxes: marker-text", f.read())

    def test_report_says_not_run_without_a_section(self):
        env = self.env()
        with open(cli.report(env, cli.extract(env))) as f:
            self.assertIn("## Mango findings\n\nNot run.", f.read())

    def test_backtest_section_is_written_and_embedded_in_the_report(self):
        env = self.env()
        res = cli.extract(env)
        os.makedirs(env.docs_dir)
        with open(os.path.join(env.docs_dir, "regime-events.json"), "w") as f:
            json.dump([{"date": "2026-10-14", "what": "gate raised", "confirmed": False}], f)
        text = cli.write_backtest(env)
        self.assertIn("| rework depth |", text)
        self.assertIn("gate raised [unconfirmed]", text)
        with open(cli.report(env, res)) as f:
            self.assertIn("## Usefulness backtest\n\nEvents (from", f.read())

    def test_previous_run_is_the_newest_earlier_directory(self):
        first = self.env(out_dir=os.path.join(self.tmp, "metrics", "2026-10-16"))
        cli.extract(first)
        second = self.env()
        self.assertTrue(cli.previous_weekly(second.out_dir))
        self.assertEqual(cli.previous_weekly(first.out_dir), [])


if __name__ == "__main__":
    unittest.main()
