"""Integration tier (NOT in the default `test_town_metrics_*.py` pattern): runs `extract` read-only on the real town.

Run by hand, under nice, only when the host is quiet:
  nice -n 15 python3 -I -m unittest discover -s scripts -p 'integ_town_metrics_*.py'
Cost: one `git show` per landing not yet in the diff cache (the first run reads ~2,800 commits), one `bd list` per
directory and at most 150 capped `bd show` calls per kind. Every subprocess carries a timeout; nothing polls."""
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from town_metrics import cli, common  # noqa: E402


class TestRealTown(unittest.TestCase):
    def test_extract_keeps_every_ledger_line_accounted_for(self):
        env = cli.real_env("smoke")  # writes under ~/gt/.runtime/metrics/smoke, never into docs/
        result = cli.extract(env)    # raises RowCountError if a ledger line vanished without a logged drop
        cli.save_caches(env)
        rows = common.read_csv(os.path.join(env.out_dir, "landings.csv"))
        self.assertGreaterEqual(len(rows), 1000, "landings.csv suspiciously small: %d rows" % len(rows))
        self.assertEqual(len(rows), len(result["landings"]))
        read = result["footer"]["rows_read"]["ledger"]
        ledger_drops = sum(1 for s, _ in result["footer"]["rows_dropped"] if s.startswith("ledger:"))
        self.assertEqual(len(rows) + ledger_drops, read)


if __name__ == "__main__":
    unittest.main()
