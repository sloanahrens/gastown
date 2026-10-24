"""The Python replication maths against the golden cases the Go port also reads
(internal/metrics/testdata/replicate_cases.json): one fixture, two implementations."""
import json
import os
import sys
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)
from town_metrics import replicate  # noqa: E402

GOLDEN = os.path.join(HERE, "..", "internal", "metrics", "testdata", "replicate_cases.json")
TOL = 1e-9


def load():
    with open(GOLDEN) as f:
        return json.load(f)


class TestReplicateGolden(unittest.TestCase):
    def test_golden_file_is_not_empty(self):
        g = load()
        for key in ("binom", "holm", "matches", "describe"):
            self.assertTrue(g[key], key)

    def test_binom_two_sided(self):
        for c in load()["binom"]:
            got = replicate.binom_two_sided(c["k"], c["n"], c["p"])
            self.assertAlmostEqual(got, c["want"], delta=TOL, msg=str(c))

    def test_holm(self):
        for c in load()["holm"]:
            got = replicate.holm(c["p"])
            self.assertEqual(len(got), len(c["want"]), msg=str(c))
            for a, b in zip(got, c["want"]):
                self.assertAlmostEqual(a, b, delta=TOL, msg=str(c))

    def test_matches(self):
        for c in load()["matches"]:
            self.assertEqual(replicate.matches(c["cube"], c["row"]), c["want"], msg=str(c))

    def test_describe(self):
        for c in load()["describe"]:
            self.assertEqual(replicate.describe(c["cube"]), c["want"], msg=str(c))


if __name__ == "__main__":
    unittest.main()
