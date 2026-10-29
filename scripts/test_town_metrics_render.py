"""Golden-file test for render_report. The golden was written by hand from the layout in the plan, not generated."""
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from town_metrics import metrics, render  # noqa: E402

HERE = os.path.dirname(os.path.abspath(__file__))
GOLDEN = os.path.join(HERE, "..", "internal", "metrics", "testdata", "report_golden.md")


def row(week, **kw):
    r = {c: None for c in metrics.WEEKLY_COLUMNS}
    r.update({"week": week, "rig": "ALL", "thin": False})
    r.update(kw)
    return r


WEEKLY = [
    row("2026-10-05", landings=12, unreadable=0, size_median=40, size_p90=120.0, file_entropy_mean=1.2345,
        package_entropy_mean=0.5, rejected_share=0.1, rho=0.5),
    row("2026-10-12", landings=8, unreadable=1, size_median=50, size_p90=130.0, file_entropy_mean=1.5,
        package_entropy_mean=0.75, rejected_share=0.3, rho=0.25, mode_shift=1.8, thin=True),
    row("2026-10-12", rig="gastown", size_median=999),  # a per-rig row must not appear in the town tables
]
FLAGS = [{"rig": "ALL", "week": "2026-10-12", "column": "size_median", "value": 50, "mean": 40.0, "sd": 2.0}]
FOOTER = {"rows_read": {"reviews": 5, "ledger": 3}, "rows_dropped": [("ledger:gastown", "unparseable json")],
          "sources_unreadable": ["bd:/x"]}
EVENTS = [{"date": "2026-10-10", "what": "flash to Sonnet"}]


def render_it(**over):
    args = dict(date="2026-10-23", weekly=WEEKLY, flags=FLAGS, footer=FOOTER, mango_section="Not run.",
                regime_events=EVENTS)
    args.update(over)
    return render.render_report(**args)


class TestRender(unittest.TestCase):
    def test_matches_golden(self):
        with open(GOLDEN) as f:
            want = f.read()
        self.assertEqual(render_it(), want)

    def test_every_metric_heading_appears_once(self):
        out = render_it()
        for n in range(1, 10):
            self.assertEqual(out.count("## Metric %d:" % n), 1, "metric %d" % n)

    def test_quiet_run_and_clean_footer(self):
        out = render_it(flags=[], footer={"rows_read": {}, "rows_dropped": [], "sources_unreadable": []})
        self.assertIn("Nothing moved more than two standard deviations.", out)
        self.assertIn("Rows dropped: 0", out)
        self.assertIn("Sources unreadable: none", out)

    def test_backtest_text_replaces_the_default(self):
        out = render_it(backtest="| Metric | verdict |\n| rho | keep |")
        self.assertIn("## Usefulness backtest\n\n| Metric | verdict |", out)
        self.assertNotIn("## Usefulness backtest\n\nNot run.", out)

    def test_other_gaps_are_listed_apart_from_dropped_rows(self):
        footer = dict(FOOTER, gaps=[("git:gastown", "unreadable diff for gt-1")])
        out = render_it(footer=footer)
        self.assertIn("Other gaps: 1\n- git:gastown: unreadable diff for gt-1", out)
        self.assertIn("Rows dropped: 1\n- ledger:gastown: unparseable json", out)

    def test_dropped_reason_is_printed(self):
        self.assertIn("ledger:gastown: unparseable json", render_it())


if __name__ == "__main__":
    unittest.main()
