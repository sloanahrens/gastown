package polecat

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

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
	if err := RemoveSandboxSeatHostDirs(rigPath, "rig", "agate", remover); err != nil {
		t.Fatalf("RemoveSandboxSeatHostDirs: %v", err)
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
	remover := func(_, _ string) error { return nil }
	for i := 0; i < 2; i++ {
		if err := RemoveSandboxSeatHostDirs(rigPath, "rig", "agate", remover); err != nil {
			t.Fatalf("run %d: RemoveSandboxSeatHostDirs: %v", i+1, err)
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
			if err := RemoveSandboxSeatHostDirs(tt.rigPath, tt.rig, tt.polecat, remover); err == nil {
				t.Fatal("RemoveSandboxSeatHostDirs accepted an unsafe name")
			}
			if !exists(t, outside) {
				t.Error("the neighbouring polecat directory was removed")
			}
		})
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

	err := RemoveSandboxSeatHostDirs(rigPath, "rig", "agate", remover)
	if !errors.Is(err, detachErr) {
		t.Fatalf("err = %v, want it to carry %v", err, detachErr)
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

	if err := RemoveSandboxSeatHostDirs(rigPath, "rig", "agate", nil); err != nil {
		t.Fatalf("RemoveSandboxSeatHostDirs: %v", err)
	}
	if exists(t, dirs.PolecatDir) {
		t.Errorf("%s still exists", dirs.PolecatDir)
	}
	if exists(t, dirs.PromptFile) {
		t.Errorf("%s still exists", dirs.PromptFile)
	}
}
