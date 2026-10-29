package polecat

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// writeSeatFile creates path with body, creating its parents.
func writeSeatFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// exists reports whether path is there.
func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	t.Fatalf("stat %s: %v", path, err)
	return false
}

func TestSandboxSeatHostDirsFor(t *testing.T) {
	t.Parallel()
	got := SandboxSeatHostDirsFor("/town/rig", "rig", "agate")
	want := SandboxSeatHostDirs{
		PolecatDir: "/town/rig/polecats/agate",
		ClonePath:  "/town/rig/polecats/agate/rig",
		PromptFile: "/town/rig/polecats/.claude/sandbox/system-prompt-agate.md",
	}
	if got != want {
		t.Errorf("SandboxSeatHostDirsFor = %+v, want %+v", got, want)
	}
}

// The seat's own directory, its clone and its rendered prompt go; every other
// seat and the shared settings beside the prompt stay.
func TestRemoveSandboxSeatHostDirsLeavesItsNeighbours(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	dirs := SandboxSeatHostDirsFor(rigPath, "rig", "agate")
	writeSeatFile(t, filepath.Join(dirs.ClonePath, "CLAUDE.md"), "seat\n")
	writeSeatFile(t, filepath.Join(dirs.PolecatDir, "target_clean_counter"), "3\n")
	writeSeatFile(t, dirs.PromptFile, "prompt\n")

	other := SandboxSeatHostDirsFor(rigPath, "rig", "basalt")
	writeSeatFile(t, filepath.Join(other.ClonePath, "CLAUDE.md"), "other\n")
	writeSeatFile(t, other.PromptFile, "other prompt\n")
	settings := filepath.Join(rigPath, "polecats", ".claude", "settings.json")
	writeSeatFile(t, settings, "{}\n")

	var detached []string
	remover := func(_, clonePath string) error {
		detached = append(detached, clonePath)
		return nil
	}
	deleted, err := RemoveSandboxSeatHostDirs(rigPath, "rig", "agate", remover)
	if err != nil {
		t.Fatalf("RemoveSandboxSeatHostDirs: %v", err)
	}
	if !deleted {
		t.Error("deleted = false, want the seat's directories reported removed")
	}

	if exists(t, dirs.PolecatDir) {
		t.Errorf("%s still exists", dirs.PolecatDir)
	}
	if exists(t, dirs.PromptFile) {
		t.Errorf("%s still exists", dirs.PromptFile)
	}
	if len(detached) != 1 || detached[0] != dirs.ClonePath {
		t.Errorf("detached = %v, want [%s]", detached, dirs.ClonePath)
	}
	for _, keep := range []string{other.PolecatDir, other.PromptFile, settings} {
		if !exists(t, keep) {
			t.Errorf("%s was removed, and it belongs to something else", keep)
		}
	}
}

// A seat whose directories are already gone is a no-op, so the daemon may run
// the step on every landing for it.
func TestRemoveSandboxSeatHostDirsIsIdempotent(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	dirs := SandboxSeatHostDirsFor(rigPath, "rig", "agate")
	writeSeatFile(t, filepath.Join(dirs.ClonePath, "CLAUDE.md"), "seat\n")
	writeSeatFile(t, dirs.PromptFile, "prompt\n")
	remover := func(_, _ string) error { return nil }
	for i := 0; i < 2; i++ {
		deleted, err := RemoveSandboxSeatHostDirs(rigPath, "rig", "agate", remover)
		if err != nil {
			t.Fatalf("run %d: RemoveSandboxSeatHostDirs: %v", i+1, err)
		}
		if want := i == 0; deleted != want {
			t.Errorf("run %d: deleted = %v, want %v", i+1, deleted, want)
		}
	}
	if exists(t, SandboxSeatHostDirsFor(rigPath, "rig", "agate").PolecatDir) {
		t.Error("the seat directory exists after two runs")
	}
}

// A name or rig that is not one path component names a directory outside the
// seat, so nothing is removed and the refusal says which one.
func TestRemoveSandboxSeatHostDirsRefusesUnsafeNames(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	outside := filepath.Join(rigPath, "polecats", "keep")
	writeSeatFile(t, filepath.Join(outside, "file"), "keep\n")
	tests := []struct {
		name    string
		rigPath string
		rig     string
		polecat string
	}{
		{"climbing polecat", rigPath, "rig", "../keep"},
		{"separator in polecat", rigPath, "rig", "a/b"},
		{"empty polecat", rigPath, "rig", ""},
		{"hidden polecat", rigPath, "rig", ".claude"},
		{"climbing rig", rigPath, "../rig", "agate"},
		{"relative rig path", "town/rig", "rig", "agate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			remover := func(_, _ string) error {
				t.Error("the remover was called for a refused name")
				return nil
			}
			if deleted, err := RemoveSandboxSeatHostDirs(tt.rigPath, tt.rig, tt.polecat, remover); err == nil || deleted {
				t.Fatalf("RemoveSandboxSeatHostDirs = (deleted %v, %v), want a refusal that deleted nothing", deleted, err)
			}
			if !exists(t, outside) {
				t.Error("the neighbouring polecat directory was removed")
			}
		})
	}
}

// A seat path that cannot be read is reported, not read as absent: the caller
// must never hear "gone" from a check that did not run. A path under a plain
// file answers ENOTDIR, which is neither "present" nor "not there".
func TestRemoveSandboxSeatHostDirsReportsAnUnreadableSeatPath(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	writeSeatFile(t, filepath.Join(rigPath, "polecats"), "not a directory\n")
	dirs := SandboxSeatHostDirsFor(rigPath, "rig", "agate")
	remover := func(_, _ string) error {
		t.Error("the remover ran for a path that could not be read")
		return nil
	}

	deleted, err := RemoveSandboxSeatHostDirs(rigPath, "rig", "agate", remover)
	if err == nil || !strings.Contains(err.Error(), "checking "+dirs.ClonePath) {
		t.Fatalf("err = %v, want the unreadable clone path reported", err)
	}
	if deleted {
		t.Error("deleted = true for a seat whose paths could not be read")
	}
}

// A detach that fails does not stop the removal: the stale clone and its home
// directory still go, and the failure is reported rather than swallowed.
func TestRemoveSandboxSeatHostDirsRemovesAfterAFailedDetach(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	dirs := SandboxSeatHostDirsFor(rigPath, "rig", "agate")
	writeSeatFile(t, filepath.Join(dirs.ClonePath, "CLAUDE.md"), "seat\n")
	detachErr := errors.New("not a working tree")
	remover := func(_, _ string) error { return detachErr }

	deleted, err := RemoveSandboxSeatHostDirs(rigPath, "rig", "agate", remover)
	if !errors.Is(err, detachErr) {
		t.Fatalf("err = %v, want it to carry %v", err, detachErr)
	}
	if !deleted {
		t.Error("deleted = false after the clone and its home directory went")
	}
	if exists(t, dirs.PolecatDir) {
		t.Errorf("%s still exists after a failed detach", dirs.PolecatDir)
	}
}

// The lock the seat's end takes is the one a sling takes before it builds the
// worktree, so it must be held for the whole callback and free afterwards.
func TestWithSandboxSeatLockHoldsThePolecatLock(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	lockPath := filepath.Join(rigPath, ".runtime", "locks", "polecat-agate.lock")
	inside := 0
	err := WithSandboxSeatLock(rigPath, "agate", func() error {
		inside++
		other := flock.New(lockPath)
		locked, err := other.TryLock()
		if err != nil {
			return err
		}
		defer func() { _ = other.Unlock() }()
		if locked {
			t.Error("another process's lock was granted while the callback ran")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithSandboxSeatLock: %v", err)
	}
	if inside != 1 {
		t.Fatalf("the callback ran %d times, want once", inside)
	}
	other := flock.New(lockPath)
	locked, err := other.TryLock()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Unlock() }()
	if !locked {
		t.Error("the lock is still held after the callback returned")
	}
}

// A sling takes the polecat lock through Manager.lockPolecat before it builds
// the worktree, and the seat's end takes it through WithSandboxSeatLock. Both
// must name the same file, or the end does not exclude a sling. While the seat
// holds the lock, the file the sling's lock is on is busy; once the seat is
// done, the sling's own lock is on that same file.
func TestWithSandboxSeatLockIsTheLockASlingTakes(t *testing.T) {
	t.Parallel()
	const name = "agate"
	mgr, _, _, _ := canonicalRig(t)
	rigPath := mgr.rig.Path
	seatPath := polecatLockPath(rigPath, name)
	err := WithSandboxSeatLock(rigPath, name, func() error {
		probe := flock.New(seatPath)
		locked, err := probe.TryLock()
		if err != nil {
			return err
		}
		if locked {
			_ = probe.Unlock()
			t.Error("the seat's end ran without the polecat lock a sling takes")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithSandboxSeatLock: %v", err)
	}
	fl, err := mgr.lockPolecat(name)
	if err != nil {
		t.Fatalf("lockPolecat: %v", err)
	}
	defer func() { _ = fl.Unlock() }()
	if fl.Path() != seatPath {
		t.Fatalf("a sling locks %s, the seat's end locks %s", fl.Path(), seatPath)
	}
}

// Only the two names a seat's directories are built from are checked, and an
// unsafe one is refused before anything is read under it.
func TestCheckSandboxSeatNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		rig, name string
		wantErr   bool
	}{
		{"gastown", "agate", false},
		{"gastown", "../agate", true},
		{"gastown", "", true},
		{"gastown", ".", true},
		{"..", "agate", true},
		{"a/b", "agate", true},
		{".hidden", "agate", true},
	}
	for _, tt := range tests {
		err := CheckSandboxSeatNames(tt.rig, tt.name)
		if (err != nil) != tt.wantErr {
			t.Errorf("CheckSandboxSeatNames(%q, %q) = %v, want error %v", tt.rig, tt.name, err, tt.wantErr)
		}
	}
}

// A refusal is not a callback: an unsafe name or a relative rig path runs
// nothing at all.
func TestWithSandboxSeatLockRefusesUnsafeInputs(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	tests := []struct {
		name    string
		rigPath string
		polecat string
	}{
		{"climbing polecat", rigPath, "../agate"},
		{"separator in polecat", rigPath, "a/agate"},
		{"empty polecat", rigPath, ""},
		{"relative rig path", "town/rig", "agate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := WithSandboxSeatLock(tt.rigPath, tt.polecat, func() error {
				t.Error("the callback ran for refused input")
				return nil
			})
			if err == nil {
				t.Fatal("WithSandboxSeatLock accepted unsafe input")
			}
		})
	}
}

// A rig with no repository has no worktree entry to detach or prune, so the
// production remover reads no repository and only the paths are deleted.
func TestRemoveSandboxSeatHostDirsWithoutARigRepository(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	dirs := SandboxSeatHostDirsFor(rigPath, "rig", "agate")
	writeSeatFile(t, filepath.Join(dirs.ClonePath, "CLAUDE.md"), "seat\n")
	writeSeatFile(t, dirs.PromptFile, "prompt\n")

	deleted, err := RemoveSandboxSeatHostDirs(rigPath, "rig", "agate", nil)
	if err != nil {
		t.Fatalf("RemoveSandboxSeatHostDirs: %v", err)
	}
	if !deleted {
		t.Error("deleted = false, want the seat's directories reported removed")
	}
	if exists(t, dirs.PolecatDir) {
		t.Errorf("%s still exists", dirs.PolecatDir)
	}
	if exists(t, dirs.PromptFile) {
		t.Errorf("%s still exists", dirs.PromptFile)
	}
}

// A prune that fails is reported, and the seat's paths are still deleted: the
// repository's worktree entry is the only thing left, and its owner is git.
func TestRemoveSandboxSeatHostDirsReportsAFailedPrune(t *testing.T) {
	t.Parallel()
	rigPath := t.TempDir()
	// A repository-shaped directory the fake world does not know, so its
	// worktree prune answers "not a git repository".
	if err := os.MkdirAll(filepath.Join(rigPath, ".repo.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	dirs := SandboxSeatHostDirsFor(rigPath, "rig", "agate")
	writeSeatFile(t, filepath.Join(dirs.ClonePath, "CLAUDE.md"), "seat\n")

	deleted, err := removeSandboxSeatHostDirs(newWorld().opener(), rigPath, "rig", "agate", func(_, _ string) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "pruning the worktrees") {
		t.Fatalf("err = %v, want the failed prune", err)
	}
	if !deleted {
		t.Error("deleted = false, want the paths the failed prune deleted reported")
	}
	if exists(t, dirs.PolecatDir) {
		t.Errorf("%s still exists after a failed prune", dirs.PolecatDir)
	}
}

// A lock directory that cannot be made or opened, and a lock another process
// holds, are answers the seat's end must get as errors rather than as a run
// with no lock.
func TestWithSandboxSeatLockReportsLockFailures(t *testing.T) {
	t.Parallel()
	held := func(t *testing.T) (string, func()) {
		t.Helper()
		rigPath := t.TempDir()
		lockDir := filepath.Join(rigPath, ".runtime", "locks")
		if err := os.MkdirAll(lockDir, 0o755); err != nil {
			t.Fatal(err)
		}
		fl := flock.New(filepath.Join(lockDir, "polecat-agate.lock"))
		if err := fl.Lock(); err != nil {
			t.Fatal(err)
		}
		return rigPath, func() { _ = fl.Unlock() }
	}
	t.Run("a busy lock", func(t *testing.T) {
		t.Parallel()
		rigPath, release := held(t)
		defer release()
		err := withSandboxSeatLockTimeout(rigPath, "agate", 20*time.Millisecond, func() error {
			t.Error("the callback ran without the lock")
			return nil
		})
		if !errors.Is(err, ErrSandboxSeatLockBusy) {
			t.Fatalf("err = %v, want ErrSandboxSeatLockBusy", err)
		}
	})
	t.Run("a lock path that cannot be opened", func(t *testing.T) {
		t.Parallel()
		rigPath := t.TempDir()
		lockPath := filepath.Join(rigPath, ".runtime", "locks", "polecat-agate.lock")
		if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
			t.Fatal(err)
		}
		// A dangling symlink whose target directory does not exist cannot be
		// opened for locking on any OS; a directory at the lock path can be
		// locked on macOS, so it would pass there and fail in Linux CI.
		if err := os.Symlink(filepath.Join(rigPath, "missing", "polecat-agate.lock"), lockPath); err != nil {
			t.Fatal(err)
		}
		err := WithSandboxSeatLock(rigPath, "agate", func() error {
			t.Error("the callback ran without the lock")
			return nil
		})
		if err == nil || errors.Is(err, ErrSandboxSeatLockBusy) {
			t.Fatalf("err = %v, want the lock-open failure", err)
		}
	})
	t.Run("a lock directory that cannot be made", func(t *testing.T) {
		t.Parallel()
		rigPath := t.TempDir()
		writeSeatFile(t, filepath.Join(rigPath, ".runtime"), "not a directory\n")
		err := WithSandboxSeatLock(rigPath, "agate", func() error {
			t.Error("the callback ran without the lock")
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "creating lock dir") {
			t.Fatalf("err = %v, want the lock-dir failure", err)
		}
	})
}
