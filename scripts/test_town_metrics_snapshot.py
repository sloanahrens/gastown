"""Tests for town_metrics.snapshot and its subcommand. Fixtures only: no bd, git, gt or network;
files go in a temp dir."""
import contextlib
import datetime as dt
import importlib
import io
import json
import os
import shutil
import stat
import sys
import tempfile
import unittest
from unittest import mock

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from town_metrics import cli, snapshot  # noqa: E402

NOW = dt.datetime(2026, 10, 24, 13, 0, tzinfo=dt.timezone.utc)
GROUPS = ["attention", "config", "host", "money", "queue", "review"]


def no_run(argv, cwd=None, timeout=120):
    raise AssertionError("a stub source ran a command: %r" % (argv,))


class TempCase(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="tm-snap-")
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.log = os.path.join(self.tmp, ".runtime", "metrics", "snapshots.jsonl")

    def fake_package(self, modules):
        """Write modules (name -> source) into a fresh package under tmp, import it and return it."""
        name = "fakesrc_" + os.path.basename(self.tmp).replace("-", "_")
        pkg_dir = os.path.join(self.tmp, name)
        os.makedirs(pkg_dir)
        with open(os.path.join(pkg_dir, "__init__.py"), "w"):
            pass
        for mod, text in modules.items():
            with open(os.path.join(pkg_dir, mod + ".py"), "w") as f:
                f.write(text)
        sys.path.insert(0, self.tmp)
        self.addCleanup(sys.path.remove, self.tmp)
        self.addCleanup(lambda: [sys.modules.pop(k) for k in list(sys.modules) if k.startswith(name)])
        return importlib.import_module(name)

    def read_lines(self):
        with open(self.log) as f:
            return f.read().splitlines()


OK_SOURCE = 'GROUP = "ok"\n\n\ndef collect(town, run, now):\n    return {"n": 1}\n'
BROKEN_SOURCE = ('GROUP = "broken"\n\n\ndef collect(town, run, now):\n'
                 '    raise RuntimeError("bd list timed out")\n')


class TestStubSources(unittest.TestCase):
    def test_six_stub_groups_are_discovered_and_report_no_columns(self):
        found, errors = snapshot.discover()
        self.assertEqual([g for g, _ in found], GROUPS)
        self.assertEqual(errors, [])
        for group, mod in found:
            self.assertEqual(mod.collect("/town", no_run, NOW), {}, group)


class TestTake(TempCase):
    def test_failing_source_is_null_with_an_error_and_the_line_is_still_written(self):
        pkg = self.fake_package({"ok": OK_SOURCE, "broken": BROKEN_SOURCE})
        rec = snapshot.take("/town", no_run, NOW, package=pkg)
        self.assertEqual(rec["ts"], "2026-10-24T13:00:00Z")
        self.assertEqual(rec["v"], 1)
        self.assertEqual(rec["ok"], {"n": 1})
        self.assertIsNone(rec["broken"])
        self.assertEqual(rec["errors"], [{"group": "broken", "error": "RuntimeError: bd list timed out"}])
        snapshot.append(self.log, rec)
        self.assertEqual(json.loads(self.read_lines()[0]), rec)

    def test_a_discovery_error_lands_in_the_record_without_losing_the_other_groups(self):
        pkg = self.fake_package({"bad": 'GROUP = "errors"\n\n\ndef collect(town, run, now):\n    return {}\n',
                                 "ok": OK_SOURCE})
        rec = snapshot.take("/town", no_run, NOW, package=pkg)
        self.assertEqual(rec["ok"], {"n": 1})
        self.assertEqual([e["group"] for e in rec["errors"]], ["bad"])

    def test_a_module_added_to_the_package_is_picked_up_with_no_other_change(self):
        pkg = self.fake_package({"ok": OK_SOURCE})
        self.assertEqual([g for g, _ in snapshot.discover(pkg)[0]], ["ok"])
        with open(os.path.join(os.path.dirname(pkg.__file__), "extra.py"), "w") as f:
            f.write('GROUP = "extra"\n\n\ndef collect(town, run, now):\n    return {}\n')
        importlib.invalidate_caches()  # the import finder caches directory listings by mtime
        self.assertEqual([g for g, _ in snapshot.discover(pkg)[0]], ["extra", "ok"])

    def test_a_source_named_after_a_reserved_key_is_an_error_and_the_rest_still_discover(self):
        pkg = self.fake_package({"bad": 'GROUP = "errors"\n\n\ndef collect(town, run, now):\n    return {}\n',
                                 "ok": OK_SOURCE})
        found, errors = snapshot.discover(pkg)
        self.assertEqual([g for g, _ in found], ["ok"])
        self.assertEqual(errors, [{"group": "bad", "error": "ValueError: %s.bad: group 'errors' is reserved or "
                                                          "already taken" % pkg.__name__}])

    def test_a_module_that_fails_to_import_is_an_error_and_the_rest_still_discover(self):
        pkg = self.fake_package({"broken": "raise RuntimeError('no module named bd_helper')\n", "ok": OK_SOURCE})
        found, errors = snapshot.discover(pkg)
        self.assertEqual([g for g, _ in found], ["ok"])
        self.assertEqual(errors, [{"group": "broken",
                                   "error": "RuntimeError: no module named bd_helper"}])

    def test_a_module_without_group_or_collect_is_an_error_and_the_rest_still_discover(self):
        pkg = self.fake_package({"nogroup": "VALUE = 1\n",
                                 "ok": OK_SOURCE})
        found, errors = snapshot.discover(pkg)
        self.assertEqual([g for g, _ in found], ["ok"])
        self.assertEqual(errors, [{"group": "nogroup", "error": "ValueError: %s.nogroup needs GROUP and collect()"
                                                               % pkg.__name__}])


class TestAppend(TempCase):
    def test_two_appends_give_two_parseable_lines_at_mode_0600(self):
        snapshot.append(self.log, {"a": 1})
        snapshot.append(self.log, {"a": 2})
        self.assertEqual([json.loads(line) for line in self.read_lines()], [{"a": 1}, {"a": 2}])
        self.assertEqual(stat.S_IMODE(os.stat(self.log).st_mode), 0o600)

    def test_a_log_left_at_0644_is_tightened_to_0600(self):
        os.makedirs(os.path.dirname(self.log))
        with open(self.log, "w"):
            pass
        os.chmod(self.log, 0o644)
        snapshot.append(self.log, {"a": 1})
        self.assertEqual(stat.S_IMODE(os.stat(self.log).st_mode), 0o600)

    def test_a_log_directory_left_at_0755_is_tightened_to_0700(self):
        os.makedirs(os.path.dirname(self.log))
        os.chmod(os.path.dirname(self.log), 0o755)
        snapshot.append(self.log, {"a": 1})
        self.assertEqual(stat.S_IMODE(os.stat(os.path.dirname(self.log)).st_mode), 0o700)

    def test_a_half_written_last_line_does_not_corrupt_the_next_record(self):
        snapshot.append(self.log, {"a": 1})
        with open(self.log, "ab") as f:
            f.write(b'{"a": 2, "b": ')  # a short write: no trailing newline
        snapshot.append(self.log, {"a": 3})
        lines = self.read_lines()
        self.assertEqual(len(lines), 3)
        self.assertEqual(json.loads(lines[0]), {"a": 1})
        self.assertEqual(lines[1], '{"a": 2, "b": ')
        self.assertEqual(json.loads(lines[2]), {"a": 3})


class TestCliHelpers(unittest.TestCase):
    def test_town_root_is_the_argument_then_the_env_override_then_home(self):
        with mock.patch.dict(os.environ, {"TOWN_METRICS_TOWN": "/from/env"}):
            self.assertEqual(cli._town("/explicit"), "/explicit")
            self.assertEqual(cli._town(), "/from/env")
        env = {k: v for k, v in os.environ.items() if k != "TOWN_METRICS_TOWN"}
        with mock.patch.dict(os.environ, env, clear=True):
            self.assertEqual(cli._town(), os.path.expanduser("~/gt"))

    def test_run_returns_stdout_and_none_on_a_failing_command(self):
        self.assertEqual(cli._run(["sh", "-c", "echo out"]), "out\n")
        self.assertIsNone(cli._run(["sh", "-c", "echo err >&2; exit 3"]))
        self.assertIsNone(cli._run(["town-metrics-no-such-command"]))


class TestSnapshotCommand(TempCase):
    def test_snapshot_subcommand_appends_one_line_with_the_six_stub_groups(self):
        out = io.StringIO()
        with mock.patch.dict(os.environ, {"TOWN_METRICS_TOWN": self.tmp}), contextlib.redirect_stdout(out):
            rc = cli.main(["snapshot"])
        self.assertEqual(rc, 0)
        self.assertEqual(out.getvalue().strip(), "snapshot: appended to " + self.log)
        lines = self.read_lines()
        self.assertEqual(len(lines), 1)
        rec = json.loads(lines[0])
        self.assertEqual(sorted(k for k in rec if k not in ("ts", "v", "errors")), GROUPS)
        self.assertEqual(rec["errors"], [])

    def test_run_snapshot_writes_under_the_env_override(self):
        with mock.patch.dict(os.environ, {"TOWN_METRICS_TOWN": self.tmp}):
            self.assertEqual(cli.run_snapshot(), self.log)
        self.assertEqual(len(self.read_lines()), 1)


if __name__ == "__main__":
    unittest.main()
