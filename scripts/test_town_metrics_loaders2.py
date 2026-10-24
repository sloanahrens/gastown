"""Tests for the beads, usage, overrun and om-era loaders (fixtures only)."""
import json
import os
import sys
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from town_metrics import common, loaders  # noqa: E402

BD_LIST = json.dumps([
    {"id": "gt-1", "created_at": "2026-10-01T00:00:00Z", "closed_at": "2026-10-02T00:00:00Z", "issue_type": "task",
     "priority": 1, "labels": ["handoff", "x"], "dependency_count": 2, "comment_count": 3,
     "acceptance_criteria": "- [ ] a",
     "notes": "x\nMERGE REJECTION (attempt 1): conflict\nMERGE REJECTION (attempt 2): gate\n"},
    {"id": "gt-2", "created_at": "2026-10-03T00:00:00Z", "closed_at": None, "issue_type": "bug",
     "labels": None, "dependency_count": 0, "comment_count": 0},
])


class TestBeads(unittest.TestCase):
    def test_load_beads_parses_and_normalises_labels(self):
        calls = []

        def run_bd(cwd, args):
            calls.append((cwd, args))
            return BD_LIST
        rows = loaders.load_beads(run_bd, ["/g"], common.Dropped())
        self.assertEqual([r["id"] for r in rows], ["gt-1", "gt-2"])
        self.assertEqual(rows[0]["labels"], ["handoff", "x"])
        self.assertEqual(rows[0]["priority"], 1)
        self.assertEqual(rows[1]["labels"], [])
        self.assertEqual(rows[0]["rig"], "gt")
        self.assertEqual((rows[0]["dir"], rows[0]["shaped"], rows[1]["shaped"]), ("/g", True, False))
        self.assertEqual(rows[0]["rejection_kinds"], ["conflict", "gate"])
        self.assertEqual(rows[1]["rejection_kinds"], [])
        self.assertNotIn("notes", rows[0])  # text is parsed at load and not carried around
        self.assertEqual(len(calls), 1)  # one bd list per directory, never per bead
        self.assertEqual(calls[0][1], ["list", "--all", "--limit", "0", "--json"])

    def test_beads_seen_from_two_directories_are_counted_once(self):
        rows = loaders.load_beads(lambda cwd, args: BD_LIST, ["/g", "/h"], common.Dropped())
        self.assertEqual([r["id"] for r in rows], ["gt-1", "gt-2"])
        self.assertEqual(rows[0]["dir"], "/g")  # first directory wins

    def test_failed_bd_list_is_a_structured_failure_not_a_message_match(self):
        d = common.Dropped()
        loaders.load_beads(lambda cwd, args: None, ["/g"], d)
        self.assertEqual(d.failed, ["bd:/g"])

    def test_load_beads_retries_once_then_drops(self):
        n = {"c": 0}

        def run_bd(cwd, args):
            n["c"] += 1
            return None
        d = common.Dropped()
        self.assertEqual(loaders.load_beads(run_bd, ["/g"], d), [])
        self.assertEqual(n["c"], 2)
        self.assertEqual(d.count, 1)

    def test_handoff_bytes_sums_notes_and_comments(self):
        show = json.dumps([{"notes": "abcd", "comments": [{"text": "ef"}, {"text": "ghi"}]}])
        got = loaders.handoff_bytes(lambda cwd, args: show, {"gt-1": "/g"}, common.Dropped())
        self.assertEqual(got, {"gt-1": 9})

    def test_handoff_bytes_respects_the_call_cap(self):
        calls = []

        def run_bd(cwd, args):
            calls.append(args)
            return json.dumps([{"notes": "a", "comments": []}])
        d = common.Dropped()
        got = loaders.handoff_bytes(run_bd, {"a": "/g", "b": "/g", "c": "/g"}, d, cap=2)
        self.assertEqual(len(calls), 2)
        self.assertEqual(sorted(got), ["a", "b"])
        self.assertEqual(d.items, [("bd:c", "not fetched: call cap 2 reached")])

    def test_fetched_sizes_are_written_back_to_the_cache(self):
        cache = {}
        show = json.dumps([{"notes": "abcd", "comments": []}])
        loaders.handoff_bytes(lambda c, a: show, {"gt-1": "/g"}, common.Dropped(), cache=cache)
        self.assertEqual(cache, {"gt-1": 4})

    def test_handoff_bytes_cache_skips_calls(self):
        calls = []
        got = loaders.handoff_bytes(lambda c, a: calls.append(a), {"gt-1": "/g"}, common.Dropped(), cache={"gt-1": 7})
        self.assertEqual(got, {"gt-1": 7})
        self.assertEqual(calls, [])


class TestUsageOverrunEras(unittest.TestCase):
    def test_usage_picks_model_with_most_calls(self):
        u = loaders.load_usage({"al-1": {"models": {"a": {"calls": 3}, "b": {"calls": 9}}, "seat": "s", "end": "2026-10-01T00:00:00Z",
                                  "sessions": [{}, {}]}})
        self.assertEqual(u["al-1"]["model"], "b")
        self.assertEqual(u["al-1"]["sessions"], 2)

    def test_usage_without_models_is_absent(self):
        self.assertEqual(loaders.load_usage({"al-1": {"models": {}}}), {})

    def test_om_eras_skips_bad_json_and_orders(self):
        eras = loaders.om_eras([("2026-09-01T00:00:00Z", '{"threshold": 0.7}'),
                                ("2026-09-15T00:00:00Z", "oops"),
                                ("2026-10-10T00:00:00Z", '{"threshold": 0.75}')])
        self.assertEqual(eras, [("2026-09-01T00:00:00Z", 0.7), ("2026-10-10T00:00:00Z", 0.75)])
        self.assertEqual(loaders.era_of(eras, "2026-10-01T00:00:00Z"), 0.7)
        self.assertIsNone(loaders.era_of(eras, "2026-08-01T00:00:00Z"))

    def test_overrun_stop_means_the_stop_began_not_that_the_guard_tracked_the_bead(self):
        states = {"a": {"bead": "a", "gate_sling_at": "2026-10-13T00:00:00Z"},   # tracked only
                  "b": {"bead": "b", "stop_began_at": "2026-10-14T00:00:00Z"},   # stopped
                  "c": {"bead": "c", "stop_began_at": "0001-01-01T00:00:00Z"},   # zero time
                  "d": {"bead": "d", "nudged_at": "2026-10-14T00:00:00Z"}}       # nudged, not stopped
        self.assertEqual(loaders.load_overrun(states, {"a", "b", "c", "d", "z"}), {"b"})


if __name__ == "__main__":
    unittest.main()
