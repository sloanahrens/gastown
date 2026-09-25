package polecat

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
)

// addCleanIdlePolecat creates a polecat whose worktree is clean, so the reuse
// gate would hand it out if nothing else stood in the way.
func addCleanIdlePolecat(t *testing.T, mgr *Manager, name string) *Polecat {
	t.Helper()
	p, err := mgr.AddWithOptions(name, AddOptions{})
	if err != nil {
		t.Fatalf("AddWithOptions(%s): %v", name, err)
	}
	_ = git.NewGit(p.ClonePath).CleanForce()
	return p
}

func parkPolecat(t *testing.T, mgr *Manager, name string) {
	t.Helper()
	if err := agentpause.Pause(mgr.townRoot, mgr.rig.Name, constants.RolePolecat, name,
		"operator parked", "human", "idle"); err != nil {
		t.Fatalf("Pause(%s): %v", name, err)
	}
}

func assertStillParked(t *testing.T, mgr *Manager, name string) {
	t.Helper()
	paused, _, err := agentpause.PauseGate(mgr.townRoot, mgr.rig.Name, constants.RolePolecat, name)
	if err != nil || !paused {
		t.Fatalf("pause marker for %s was cleared or broken (paused=%v, err=%v); reuse must never undo a park", name, paused, err)
	}
}

// TestFindIdlePolecat_SkipsParkedPolecat guards gt-0r29l: a polecat carrying
// an agentpause marker was handed new work by pool reuse, and every scanner
// that honours the marker then ignored the new session's crash. The parked
// slot sorts first, so without the gate it is the one picked.
func TestFindIdlePolecat_SkipsParkedPolecat(t *testing.T) {
	mgr, _ := setupCanonicalBranchManagerTest(t)
	addCleanIdlePolecat(t, mgr, "alpha")
	addCleanIdlePolecat(t, mgr, "bravo")
	parkPolecat(t, mgr, "alpha")

	found, err := mgr.FindIdlePolecat()
	if err != nil {
		t.Fatalf("FindIdlePolecat: %v", err)
	}
	if found == nil {
		t.Fatal("FindIdlePolecat returned nil; want the non-parked bravo")
	}
	if found.Name != "bravo" {
		t.Fatalf("FindIdlePolecat picked %q; want bravo (alpha is parked)", found.Name)
	}
	assertStillParked(t, mgr, "alpha")
}

// TestFindIdlePolecat_AllParkedYieldsNone: when every idle slot is parked
// there is nothing to reuse, so the sling falls back to allocating a new
// polecat exactly as it does with an empty pool.
func TestFindIdlePolecat_AllParkedYieldsNone(t *testing.T) {
	mgr, _ := setupCanonicalBranchManagerTest(t)
	addCleanIdlePolecat(t, mgr, "alpha")
	addCleanIdlePolecat(t, mgr, "bravo")
	parkPolecat(t, mgr, "alpha")
	parkPolecat(t, mgr, "bravo")

	found, err := mgr.FindIdlePolecat()
	if err != nil {
		t.Fatalf("FindIdlePolecat: %v", err)
	}
	if found != nil {
		t.Fatalf("FindIdlePolecat picked parked %q; want nil so the caller allocates", found.Name)
	}
	assertStillParked(t, mgr, "alpha")
	assertStillParked(t, mgr, "bravo")
}

// TestReuseDecisionForPolecat_ParkedNamesTheReason: admission planning reads
// the same verdict, so a parked slot must not count as reusable capacity, and
// the refusal must say why.
func TestReuseDecisionForPolecat_ParkedNamesTheReason(t *testing.T) {
	mgr, _ := setupCanonicalBranchManagerTest(t)
	addCleanIdlePolecat(t, mgr, "alpha")

	if d := mgr.ReuseDecisionForPolecat("alpha", StateIdle); !d.Reusable {
		t.Fatalf("precondition: clean idle alpha not reusable: %s", d.Reason)
	}
	parkPolecat(t, mgr, "alpha")

	d := mgr.ReuseDecisionForPolecat("alpha", StateIdle)
	if d.Reusable {
		t.Fatal("parked alpha reported reusable")
	}
	if !strings.Contains(d.Reason, "parked") || !strings.Contains(d.Reason, "operator parked") {
		t.Fatalf("reason = %q; want it to say parked and carry the marker's reason", d.Reason)
	}
}

// TestReuseIdlePolecat_RefusesParkedPolecat: the destructive reuse itself
// refuses a parked slot with ErrPolecatNeedsRecovery (which the sling treats
// as "allocate new"), leaves the marker in place, and leaves the worktree on
// its old branch.
func TestReuseIdlePolecat_RefusesParkedPolecat(t *testing.T) {
	mgr, _ := setupCanonicalBranchManagerTest(t)
	p := addCleanIdlePolecat(t, mgr, "alpha")
	branchBefore, err := git.NewGit(p.ClonePath).CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch: %v", err)
	}
	parkPolecat(t, mgr, "alpha")

	_, err = mgr.ReuseIdlePolecat("alpha", AddOptions{HookBead: "gt-next"})
	if !errors.Is(err, ErrPolecatNeedsRecovery) {
		t.Fatalf("ReuseIdlePolecat(parked) err = %v; want ErrPolecatNeedsRecovery", err)
	}
	if !strings.Contains(err.Error(), "parked") {
		t.Fatalf("refusal %q does not say the polecat is parked", err)
	}
	assertStillParked(t, mgr, "alpha")
	if _, statErr := os.Stat(p.ClonePath); statErr != nil {
		t.Fatalf("parked polecat worktree touched: %v", statErr)
	}
	branchAfter, err := git.NewGit(p.ClonePath).CurrentBranch()
	if err != nil {
		t.Fatalf("CurrentBranch after: %v", err)
	}
	if branchAfter != branchBefore {
		t.Fatalf("parked polecat re-branched %q -> %q", branchBefore, branchAfter)
	}
}

// TestReuseIdlePolecat_ParkedRefusalIsDistinguishable: callers tell a park
// apart from other recovery refusals (the named sling hints `gt agent resume`
// for it), while ErrPolecatNeedsRecovery still matches so the unnamed sling
// keeps allocating a new polecat.
func TestReuseIdlePolecat_ParkedRefusalIsDistinguishable(t *testing.T) {
	mgr, _ := setupCanonicalBranchManagerTest(t)
	addCleanIdlePolecat(t, mgr, "alpha")
	parkPolecat(t, mgr, "alpha")

	_, err := mgr.ReuseIdlePolecat("alpha", AddOptions{HookBead: "gt-next"})
	if !errors.Is(err, ErrPolecatParked) {
		t.Fatalf("err = %v; want ErrPolecatParked", err)
	}
	if !errors.Is(err, ErrPolecatNeedsRecovery) {
		t.Fatalf("err = %v; want it to still match ErrPolecatNeedsRecovery", err)
	}
}

// TestParkedReuseBlocker_UnreadableMarkerSurfacesTheError: a corrupt marker
// still blocks reuse (fail closed), but the refusal says the marker could not
// be read instead of passing for an ordinary park.
func TestParkedReuseBlocker_UnreadableMarkerSurfacesTheError(t *testing.T) {
	mgr, _ := setupCanonicalBranchManagerTest(t)
	addCleanIdlePolecat(t, mgr, "alpha")
	path := agentpause.FilePath(mgr.townRoot, mgr.rig.Name, constants.RolePolecat, "alpha")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	d := mgr.ReuseDecisionForPolecat("alpha", StateIdle)
	if d.Reusable {
		t.Fatal("polecat with a corrupt pause marker reported reusable; PauseGate must fail closed")
	}
	if !strings.Contains(d.Reason, "pause marker unreadable") || !strings.Contains(d.Reason, path) {
		t.Fatalf("reason = %q; want it to say the marker is unreadable and name %s", d.Reason, path)
	}
}
