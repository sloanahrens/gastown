package land

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// slotCar makes the fixture's work a car in slot and returns it with its
// cleanup.
func slotCar(t *testing.T, f *landFixture, slot int) (*car, func()) {
	t.Helper()
	l := f.lander()
	issue := &beads.Issue{ID: f.work.BeadID, Title: "add b"}
	c, cleanup, _, err := l.mergeCar(context.Background(), l.open(f.repo), issue, f.work, f.base, false, slot)
	if err != nil || c == nil || cleanup == nil {
		t.Fatalf("mergeCar(slot %d): car non-nil = %t, cleanup non-nil = %t, err = %v; want a car and its cleanup", slot, c != nil, cleanup != nil, err)
	}
	return c, cleanup
}

// TestCarSlotPaths is gt-xwlcrw: each car names its worktree slot. Slot 0
// keeps every landing's path, WorkRoot/wt; slot 1 gets WorkRoot/wt-1, so a
// second car works beside the first instead of over it.
func TestCarSlotPaths(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	zero, cleanupZero := slotCar(t, f, 0)
	defer cleanupZero()
	one, cleanupOne := slotCar(t, f, 1)
	defer cleanupOne()
	if want := filepath.Join(f.workRoot, landWorktreeName); zero.dir != want {
		t.Errorf("slot 0 worktree = %s, want %s", zero.dir, want)
	}
	if want := filepath.Join(f.workRoot, landWorktreeName+"-1"); one.dir != want {
		t.Errorf("slot 1 worktree = %s, want %s", one.dir, want)
	}
}

// TestCarSlotCleanupLeavesTheOther: a car's cleanup removes only its own
// worktree, and its add cuts only its own path, so the slot it does not name
// keeps landing.
func TestCarSlotCleanupLeavesTheOther(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	_, cleanupZero := slotCar(t, f, 0)
	defer cleanupZero()
	_, cleanupOne := slotCar(t, f, 1)
	cleanupOne()
	if _, err := os.Stat(filepath.Join(f.workRoot, landWorktreeName)); err != nil {
		t.Errorf("slot 0's worktree after slot 1's work: stat err = %v, want it in place", err)
	}
	if _, err := os.Stat(filepath.Join(f.workRoot, landWorktreeName+"-1")); !os.IsNotExist(err) {
		t.Errorf("slot 1's worktree after its cleanup: stat err = %v, want it removed", err)
	}
}

// TestCarSlotClearsAStaleSlotDirectory: a leftover at slot 1's path, from a
// dead landing, is cleared before the add, so the slot still gets a worktree.
func TestCarSlotClearsAStaleSlotDirectory(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	stale := filepath.Join(f.workRoot, landWorktreeName+"-1")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(stale, "stale")
	if err := os.WriteFile(marker, []byte("left by a dead landing"), 0o600); err != nil {
		t.Fatal(err)
	}
	one, cleanupOne := slotCar(t, f, 1)
	defer cleanupOne()
	if one.dir != stale {
		t.Errorf("slot 1 worktree = %s, want %s", one.dir, stale)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("stale file in slot 1 after its add: stat err = %v, want it cleared", err)
	}
}

// TestCarSlotRefusesANegativeSlot: a slot below 0 is not a valid slot, so the
// add refuses it and names no path, leaving slot 0's worktree untouched.
func TestCarSlotRefusesANegativeSlot(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	issue := &beads.Issue{ID: f.work.BeadID, Title: "add b"}
	_, cleanup, _, err := l.mergeCar(context.Background(), l.open(f.repo), issue, f.work, f.base, false, -1)
	if cleanup != nil {
		cleanup()
	}
	var infra *InfraError
	if !errors.As(err, &infra) || infra.Stage != "worktree" {
		t.Fatalf("mergeCar(slot -1) = %T %v, want *InfraError at worktree", err, err)
	}
	if _, statErr := os.Stat(filepath.Join(f.workRoot, landWorktreeName)); !os.IsNotExist(statErr) {
		t.Errorf("slot -1 left %s (stat err %v), want slot 0's path untouched", filepath.Join(f.workRoot, landWorktreeName), statErr)
	}
}
