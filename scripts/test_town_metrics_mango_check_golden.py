"""The Python holdout check against the golden cases the Go port also reads
(internal/metrics/testdata/mango_check_cases.json): one fixture, two implementations."""
import json
import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from town_metrics import mango_run  # noqa: E402

GOLDEN = os.path.join(HERE, "..", "internal", "metrics", "testdata", "mango_check_cases.json")
TOL = 1e-9
FIELDS = ("box", "train_n", "train_rate", "n", "k", "rate", "baseline", "p_holm", "replicates", "label")


def load():
    with open(GOLDEN) as f:
        return json.load(f)["cases"]


class TestMangoCheckGolden(unittest.TestCase):
    def test_the_golden_has_a_box_below_the_baseline_that_does_not_replicate(self):
        self.assertTrue(any(w["n"] and w["rate"] < w["baseline"] and not w["replicates"]
                            for c in load() for w in c["want"]))

    def test_check_holdout(self):
        for c in load():
            got = mango_run.check_holdout(c["boxes"], c["rows"], c["target"], c["min_support"], [])
            self.assertEqual(len(got), len(c["want"]), c["name"])
            for g, w in zip(got, c["want"]):
                for key in FIELDS:
                    if isinstance(w[key], float):
                        self.assertAlmostEqual(g[key], w[key], delta=TOL, msg="%s: %s" % (c["name"], key))
                    else:
                        self.assertEqual(g[key], w[key], "%s: %s" % (c["name"], key))


if __name__ == "__main__":
    unittest.main()
