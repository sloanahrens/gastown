package polecatimage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCleanupRemovesTheWorktree(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	worktree := filepath.Join(tr.Cfg.BuildRoot, "abc1234")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	tr.Worktree = worktree
	tr.Cleanup()
	tr.wantArgs(t, "git", "-C", tr.Cfg.RigRepo, "worktree", "remove", "--force", worktree)
	if tr.Worktree != "" {
		t.Errorf("Worktree = %q after Cleanup, want empty", tr.Worktree)
	}
}

func TestCleanupFallsBackWhenGitCannotRemoveTheWorktree(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	worktree := filepath.Join(tr.Cfg.BuildRoot, "abc1234")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	tr.Worktree = worktree
	tr.reply = func(c command) result {
		if c.name == "git" {
			return result{exit: 1}
		}
		return result{}
	}
	tr.Cleanup()
	if exists(worktree) {
		t.Error("the worktree git could not remove is still there")
	}
}

func TestCleanupLeavesAMissingWorktreeAlone(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.Worktree = filepath.Join(tr.Cfg.BuildRoot, "abc1234")
	tr.Cleanup()
	if calls := tr.callsTo("git"); len(calls) != 0 {
		t.Errorf("git was called for a worktree that does not exist: %q", calls)
	}
	if tr.Worktree != "" {
		t.Errorf("Worktree = %q after Cleanup, want empty", tr.Worktree)
	}
}

func TestCleanupRemovesThePublishScratch(t *testing.T) {
	t.Parallel()
	tr := newTestRun(t)
	tr.TarDir = filepath.Join(tr.root, "push.1234")
	if err := os.MkdirAll(tr.TarDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tr.Cleanup()
	if exists(tr.TarDir) {
		t.Error("the publish scratch directory is still there")
	}
	if tr.TarDir != "" {
		t.Errorf("TarDir = %q after Cleanup, want empty", tr.TarDir)
	}
}

// TestCleanupReleasesOnlyItsOwnLock is the takeover's other half: a run removes
// the lock only while its own pid is in it, so the lock another run took during
// the build is left for that run.
func TestCleanupReleasesOnlyItsOwnLock(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		haveLock bool
		pid      string
		want     bool
	}{
		{"a lock this run holds", true, "4242", false},
		{"a lock another run took", true, "9999", true},
		{"a lock with no pid file", true, "", true},
		{"a run that never held the lock", false, "4242", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tr := newTestRun(t)
			makeLock(t, tr.Cfg.LockDir, tc.pid, testNow.Add(-time.Minute))
			tr.HaveLock = tc.haveLock
			tr.Cleanup()
			if got := exists(tr.Cfg.LockDir); got != tc.want {
				t.Errorf("lock directory exists = %v, want %v", got, tc.want)
			}
		})
	}
}
