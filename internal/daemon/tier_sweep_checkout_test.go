package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The sweep's checkout recovery: a directory at the fixed sweep path that is
// not a worktree - what an unclean shutdown leaves - must not wedge every
// cycle (gt-9yfgm). These drive tierSweepCheckoutWith's injected git, so the
// unit tier, which runs no git, exercises the recovery itself.

const checkoutTestSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// checkoutGitFake records each git call tierSweepCheckoutWith makes and answers
// it with the error the test set. A successful add leaves the path a worktree.
type checkoutGitFake struct {
	moveErr   error
	removeErr error
	addErr    error
	moved     []string
	removed   []string
	added     []checkoutAdd
	prunes    int
}

type checkoutAdd struct{ repo, dir, sha string }

func (f *checkoutGitFake) ops() tierSweepGitOps {
	return tierSweepGitOps{
		moveTo: func(dir, sha string) error {
			f.moved = append(f.moved, dir+" "+sha)
			return f.moveErr
		},
		remove: func(_, dir string) error {
			f.removed = append(f.removed, dir)
			return f.removeErr
		},
		prune: func(string) error { f.prunes++; return nil },
		add: func(repo, dir, sha string) error {
			f.added = append(f.added, checkoutAdd{repo, dir, sha})
			if f.addErr != nil {
				return f.addErr
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: "+repo+"\n"), 0o644)
		},
	}
}

// checkoutTestDir is the sweep's fixed path for gastown over a throwaway work
// root, so the test never writes where a real sweep would.
func checkoutTestDir(t *testing.T) (string, string) {
	t.Helper()
	d, _, _, _ := newTierSweepDaemon(t, atHour(time.Now(), 15), "gastown")
	d.patrolConfig.Patrols.LandingWorker = &LandingWorkerConfig{WorkRoot: t.TempDir()}
	dir, err := d.tierSweepWorkDir("gastown")
	if err != nil {
		t.Fatalf("tierSweepWorkDir: %v", err)
	}
	return dir, filepath.Join(d.config.TownRoot, "gastown", ".repo.git")
}

// A directory with files and no .git - an unclean shutdown's leftover - is
// removed and the worktree is re-added at the target sha.
func TestTierSweepCheckoutDropsADirectoryThatIsNotAWorktree(t *testing.T) {
	t.Parallel()
	dir, repo := checkoutTestDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	leftover := filepath.Join(dir, "leftover.txt")
	if err := os.WriteFile(leftover, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &checkoutGitFake{
		moveErr:   errors.New("not a git repository"),
		removeErr: errors.New("not a working tree"),
	}

	if _, err := tierSweepCheckoutWith(f.ops(), repo, dir, checkoutTestSHA); err != nil {
		t.Fatalf("tierSweepCheckoutWith: %v", err)
	}
	if _, err := os.Stat(leftover); !os.IsNotExist(err) {
		t.Errorf("leftover file survived: %v", err)
	}
	if len(f.added) != 1 || f.added[0].dir != dir || f.added[0].sha != checkoutTestSHA {
		t.Errorf("adds = %+v, want the worktree re-added at the sha", f.added)
	}
	if f.prunes != 1 {
		t.Errorf("prunes = %d, want 1", f.prunes)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		t.Errorf("the path is not a worktree after the recovery: %v", err)
	}
}

// A valid worktree on another sha is still reused: git moves it, so the sweep
// neither removes nor re-creates it.
func TestTierSweepCheckoutReusesAWorktreeOnAnotherSHA(t *testing.T) {
	t.Parallel()
	dir, repo := checkoutTestDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(dir, "kept.txt")
	if err := os.WriteFile(kept, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &checkoutGitFake{}

	if _, err := tierSweepCheckoutWith(f.ops(), repo, dir, checkoutTestSHA); err != nil {
		t.Fatalf("tierSweepCheckoutWith: %v", err)
	}
	if len(f.moved) != 1 || f.moved[0] != dir+" "+checkoutTestSHA {
		t.Errorf("moves = %v, want the worktree moved to the sha", f.moved)
	}
	if len(f.removed) != 0 || len(f.added) != 0 {
		t.Errorf("removes = %v, adds = %v; want the reused worktree untouched", f.removed, f.added)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("the reused worktree's files were disturbed: %v", err)
	}
}

// Only the path tierSweepWorkDir returned is removed: a sibling file beside it
// survives the recovery.
func TestTierSweepCheckoutRemovesOnlyTheSweepPath(t *testing.T) {
	t.Parallel()
	dir, repo := checkoutTestDir(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "leftover.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(filepath.Dir(dir), "sibling.txt")
	if err := os.WriteFile(sibling, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &checkoutGitFake{
		moveErr:   errors.New("not a git repository"),
		removeErr: errors.New("not a working tree"),
	}

	if _, err := tierSweepCheckoutWith(f.ops(), repo, dir, checkoutTestSHA); err != nil {
		t.Fatalf("tierSweepCheckoutWith: %v", err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Errorf("a sibling of the sweep path was removed: %v", err)
	}
}
