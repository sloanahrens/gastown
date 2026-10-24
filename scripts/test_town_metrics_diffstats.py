"""Tests for town_metrics.diffstats. The git case builds one tiny repo under ~/gt/claude/crew (never /tmp)."""
import os
import shutil
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from town_metrics import diffstats as ds  # noqa: E402

NUMSTAT = "10\t2\tcmd/a.go\n3\t0\tcmd/b.go\n0\t5\tinternal/x/{old => new}/c.go\n-\t-\tlogo.png\n1\t1\told.txt => new.txt\n"


class TestParse(unittest.TestCase):
    def test_parse_numstat_handles_binary_and_renames(self):
        self.assertEqual(ds.parse_numstat(NUMSTAT), [
            (10, 2, "cmd/a.go"), (3, 0, "cmd/b.go"), (0, 5, "internal/x/new/c.go"),
            (0, 0, "logo.png"), (1, 1, "new.txt")])

    def test_parse_numstat_empty(self):
        self.assertEqual(ds.parse_numstat(""), [])

    def test_brace_rename_with_empty_side(self):
        self.assertEqual(ds.parse_numstat("1\t1\tsrc/{ => sub}/f.go\n"), [(1, 1, "src/sub/f.go")])
        self.assertEqual(ds.parse_numstat("1\t1\tsrc/{sub => }/f.go\n"), [(1, 1, "src/f.go")])

    def test_diff_stats_single_file_has_zero_entropy(self):
        s = ds.diff_stats([(4, 1, "a/b.go")])
        self.assertEqual((s["lines_added"], s["lines_deleted"], s["files_touched"]), (4, 1, 1))
        self.assertEqual((s["file_entropy"], s["package_entropy"]), (0.0, 0.0))

    def test_diff_stats_two_equal_files_one_package(self):
        s = ds.diff_stats([(2, 0, "p/a.go"), (2, 0, "p/b.go")])
        self.assertEqual(s["file_entropy"], 1.0)
        self.assertEqual(s["package_entropy"], 0.0)

    def test_diff_stats_two_packages(self):
        s = ds.diff_stats([(3, 1, "p/a.go"), (2, 2, "q/b.go")])
        self.assertEqual(s["package_entropy"], 1.0)


class TestGitSource(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        base = os.path.expanduser("~/gt/claude/crew")
        cls.tmp = tempfile.mkdtemp(prefix="tm-git-", dir=base)

        def g(*a):
            return subprocess.run(["git", "-C", cls.tmp, "-c", "user.name=t", "-c", "user.email=t@t", *a],
                                  check=True, capture_output=True, text=True, timeout=30)
        g("init", "-q")
        os.makedirs(os.path.join(cls.tmp, "p"))
        with open(os.path.join(cls.tmp, "p", "a.txt"), "w") as f:
            f.write("1\n2\n3\n")
        g("add", "-A")
        g("commit", "-qm", "one")
        with open(os.path.join(cls.tmp, "p", "a.txt"), "a") as f:
            f.write("4\n")
        g("commit", "-qam", "two")
        cls.sha = g("rev-parse", "HEAD").stdout.strip()

    @classmethod
    def tearDownClass(cls):
        shutil.rmtree(cls.tmp, ignore_errors=True)

    def _src(self, cache):
        def run(args):
            # args[0:2] is ["--git-dir", <repo>]; the fixture is a worktree, so address it with -C instead.
            r = subprocess.run(["git", "-C", self.tmp, *args[2:]], capture_output=True, text=True, timeout=30)
            return r.stdout if r.returncode == 0 else None
        return ds.GitDiffSource(lambda rig: self.tmp, run, cache)

    def test_stats_for_real_commit(self):
        s = self._src({}).stats("r", self.sha)
        self.assertEqual((s["lines_added"], s["lines_deleted"], s["files_touched"]), (1, 0, 1))

    def test_unreadable_commit_is_none_and_not_cached(self):
        cache = {}
        self.assertIsNone(self._src(cache).stats("r", "deadbeef"))
        self.assertEqual(cache, {})

    def test_success_is_cached(self):
        cache = {}
        self._src(cache).stats("r", self.sha)
        self.assertIn(("r", self.sha), cache)


if __name__ == "__main__":
    unittest.main()
