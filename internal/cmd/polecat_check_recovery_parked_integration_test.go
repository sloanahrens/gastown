//go:build integration

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
)

// TestIntegrationCheckRecoveryReportsParkedAsNotReusable is the gt-b2jk0
// regression, driven through checkRecoveryForPolecat — the per-polecat body
// both `gt polecat check-recovery <rig>/<polecat>` and
// `gt polecat check-recovery-batch <rig>` run. The batch differs only in that
// it arms the preloaded agent-bead map first, so the "batch preloaded" case
// below drives that same branch with a non-nil map.
//
// Before the fix, an operator (or script) reading check-recovery for a parked
// seat was told reusable=true / idle-preserved while the allocator refused to
// reuse that seat and `gt polecat list` showed it idle-recovery-needed. The
// workstate verdict does not model the agentpause marker, so this body has to
// overlay the same park refusal the list applies (polecat.ParkedReuseBlocker
// via WorkstateDisposition.WithParked).
//
// The park is a reuse gate only: verdict, safe_to_nuke, needs_recovery and
// counts_toward_capacity must stay exactly as DecideWorkstate computed them, so
// a clean landed parked seat is still SAFE_TO_NUKE. The worktree is a real
// repository (the verdict re-derives from live git) but the database is a
// beadsfake and the pause marker lives in the test's own temp town, so this
// file costs no Docker slot of its own.
func TestIntegrationCheckRecoveryReportsParkedAsNotReusable(t *testing.T) {
	t.Parallel()

	const (
		rigName     = "gastown"
		polecatName = "slate"
		branch      = "polecat/slate/gt-2knum"
	)
	agentBeadID := "gt-" + rigName + "-polecat-" + polecatName

	// newFixture builds one isolated seat: an idle polecat whose agent bead
	// recorded a clean cleanup_status, in a fresh clean worktree on a
	// polecat/-prefixed branch. This is the exact baseline the allocator calls
	// reusable and the list calls idle-preserved.
	newFixture := func(t *testing.T) (*rig.Rig, string, *recoveryDB, *polecat.Polecat) {
		t.Helper()
		townRoot := t.TempDir()
		r := &rig.Rig{Name: rigName, Path: filepath.Join(townRoot, rigName)}

		repo := setupGitStateRemoteRepo(t)
		runGitCmd(t, repo, "switch", "-c", branch, "main")

		db := &recoveryDB{Fake: beadsfake.New()}
		db.Seed(beads.Issue{
			ID: agentBeadID, Title: "Polecat " + polecatName, Status: "open",
			Labels:      []string{"gt:agent"},
			Description: "role_type: polecat\nrig: " + rigName + "\nagent_state: idle\ncleanup_status: clean\nactive_mr: null\n",
		})

		p := &polecat.Polecat{
			Name: polecatName, Rig: rigName, State: polecat.StateIdle,
			ClonePath: repo, Branch: branch,
		}
		return r, townRoot, db, p
	}

	parkMarker := func(t *testing.T, townRoot string) {
		t.Helper()
		if err := agentpause.Pause(townRoot, rigName, constants.RolePolecat, polecatName, "operator hold", "human", "idle"); err != nil {
			t.Fatalf("agentpause.Pause: %v", err)
		}
	}

	// wantParked asserts the fields the park owns and the fields it must not
	// touch, for the same seat shape under three marker states.
	assertStatus := func(t *testing.T, status RecoveryStatus, wantReusable bool, wantReuseStatus string) {
		t.Helper()
		if status.Reusable != wantReusable {
			t.Errorf("reusable = %v, want %v (status %+v)", status.Reusable, wantReusable, status)
		}
		if status.ReuseStatus != wantReuseStatus {
			t.Errorf("reuse_status = %q, want %q (status %+v)", status.ReuseStatus, wantReuseStatus, status)
		}
		// The park is a reuse gate, not a lifecycle fact: these stay as the
		// workstate decision computed them.
		if status.Verdict != polecat.WorkstateVerdictSafeToNuke {
			t.Errorf("verdict = %q, want %q", status.Verdict, polecat.WorkstateVerdictSafeToNuke)
		}
		if !status.SafeToNuke {
			t.Errorf("safe_to_nuke = false, want true (status %+v)", status)
		}
		if status.NeedsRecovery {
			t.Errorf("needs_recovery = true, want false (status %+v)", status)
		}
		if status.CountsTowardCapacity {
			t.Errorf("counts_toward_capacity = true, want false (status %+v)", status)
		}
	}

	t.Run("an unparked seat is unchanged", func(t *testing.T) {
		t.Parallel()
		r, _, db, p := newFixture(t)
		status := checkRecoveryForPolecat(db, nil, r, rigName, polecatName, p, false)
		assertStatus(t, status, true, "idle-preserved")
	})

	t.Run("a parked seat is not reusable", func(t *testing.T) {
		t.Parallel()
		r, townRoot, db, p := newFixture(t)
		parkMarker(t, townRoot)

		status := checkRecoveryForPolecat(db, nil, r, rigName, polecatName, p, false)
		assertStatus(t, status, false, polecat.WorkstateReuseStatusParked)
		if status.Reason != polecat.WorkstateReasonParked {
			t.Errorf("reason = %q, want %q", status.Reason, polecat.WorkstateReasonParked)
		}
		// The park reason the allocator reports must be surfaced, the way the
		// list reports it, not just a bare "parked".
		if !containsSubstring(status.Blockers, "operator hold") {
			t.Errorf("blockers = %v, want one naming the park reason", status.Blockers)
		}
	})

	t.Run("the batch preloaded path reports the same", func(t *testing.T) {
		t.Parallel()
		r, townRoot, db, p := newFixture(t)
		parkMarker(t, townRoot)

		// check-recovery-batch shares one bulk agent-bead read and hands the
		// map down; the single command passes nil. Both must answer alike.
		preloaded := map[string]*beads.Issue{}
		issue, err := db.Show(agentBeadID)
		if err != nil {
			t.Fatalf("seed lookup: %v", err)
		}
		preloaded[agentBeadID] = issue

		status := checkRecoveryForPolecat(db, preloaded, r, rigName, polecatName, p, false)
		assertStatus(t, status, false, polecat.WorkstateReuseStatusParked)
	})

	t.Run("an unreadable marker fails closed with the reason shown", func(t *testing.T) {
		t.Parallel()
		r, townRoot, db, p := newFixture(t)

		// A marker that exists but cannot be parsed reads as a pause AND an
		// error (agentpause.readMarker fails closed); ParkedReuseBlocker then
		// carries the read error so a corrupt marker is not mistaken for an
		// operator's park.
		markerPath := agentpause.FilePath(townRoot, rigName, constants.RolePolecat, polecatName)
		if err := os.MkdirAll(filepath.Dir(markerPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(markerPath, []byte("{not json"), 0o644); err != nil {
			t.Fatal(err)
		}

		status := checkRecoveryForPolecat(db, nil, r, rigName, polecatName, p, false)
		assertStatus(t, status, false, polecat.WorkstateReuseStatusParked)
		if !containsSubstring(status.Blockers, "unreadable") {
			t.Errorf("blockers = %v, want one naming the unreadable marker", status.Blockers)
		}
	})
}

func containsSubstring(values []string, want string) bool {
	for _, v := range values {
		if strings.Contains(v, want) {
			return true
		}
	}
	return false
}
