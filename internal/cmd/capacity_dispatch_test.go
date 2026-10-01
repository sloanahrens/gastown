package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/scheduler/capacity"
)

// TestSchedulerSlingParamsCarriesRawReviewOnlyContext: a scheduled raw
// review-only bead is slung as one, aimed at the town beads, with formula
// failures fatal and no convoy (rollback of a failed hook is pinned in
// sling_rollback_unit_test.go).
func TestSchedulerSlingParamsCarriesRawReviewOnlyContext(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := schedulerSlingParams(capacity.PendingBead{
		ID:         "gt-context",
		WorkBeadID: "gt-rawrollback",
		TargetRig:  "hq",
		Context: &capacity.SlingContextFields{
			WorkBeadID:  "gt-rawrollback",
			TargetRig:   "hq",
			HookRawBead: true,
			NoMerge:     true,
			ReviewOnly:  true,
		},
	}, townRoot)
	if err != nil {
		t.Fatal(err)
	}
	if p.BeadID != "gt-rawrollback" || !p.HookRawBead || !p.NoMerge || !p.ReviewOnly {
		t.Errorf("params lost the queued sling: %+v", p)
	}
	if !p.FormulaFailFatal || !p.NoConvoy || !p.NoBoot || p.CallerContext != "scheduler-dispatch" {
		t.Errorf("params lost the scheduler settings: %+v", p)
	}
	if p.BeadsDir != filepath.Join(townRoot, ".beads") || p.TownRoot != townRoot {
		t.Errorf("BeadsDir = %q TownRoot = %q, want the town beads", p.BeadsDir, p.TownRoot)
	}
}

// TestSchedulerSlingParamsRefusesUnresolvableRig: a target rig with no beads
// database is an error, never a sling into the town database.
func TestSchedulerSlingParamsRefusesUnresolvableRig(t *testing.T) {
	t.Parallel()
	_, err := schedulerSlingParams(capacity.PendingBead{
		ID: "gt-context", WorkBeadID: "gt-x", TargetRig: "nowhere",
		Context: &capacity.SlingContextFields{WorkBeadID: "gt-x", TargetRig: "nowhere"},
	}, t.TempDir())
	if err == nil {
		t.Fatal("schedulerSlingParams = nil error for a rig with no beads database")
	}
	if _, err := schedulerSlingParams(capacity.PendingBead{ID: "gt-context"}, t.TempDir()); err == nil {
		t.Fatal("schedulerSlingParams = nil error for a bead with no sling context")
	}
}
