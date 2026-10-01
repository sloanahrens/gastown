package schedulerrun

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/scheduler/capacity"
)

// writeJSONFile writes v as indented JSON at path, creating parent dirs.
func writeJSONFile(t *testing.T, path string, v interface{}) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal JSON for %s: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func captureTo(write func(w io.Writer)) string {
	var b strings.Builder
	write(&b)
	return b.String()
}

func TestRunReportsHeldLock(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	runtimeDir := filepath.Join(townRoot, ".runtime")
	if err := os.MkdirAll(runtimeDir, 0755); err != nil {
		t.Fatalf("mkdir runtime: %v", err)
	}
	lockFile := filepath.Join(runtimeDir, "scheduler-dispatch.lock")
	lock := flock.New(lockFile)
	locked, err := lock.TryLock()
	if err != nil {
		t.Fatalf("TryLock: %v", err)
	}
	if !locked {
		t.Fatal("test could not acquire scheduler dispatch lock")
	}
	t.Cleanup(func() { _ = lock.Unlock() })

	_, err = Run(t.Context(), Options{TownRoot: townRoot, Actor: "test", BatchOverride: 1}, Deps{})
	if err == nil {
		t.Fatal("Run succeeded with held scheduler lock")
	}
	if !strings.Contains(err.Error(), "scheduler dispatch already in progress") || !strings.Contains(err.Error(), lockFile) {
		t.Fatalf("error = %q, want explicit held lock reason with path", err.Error())
	}
}

// gt-ifijm: `gt scheduler run` is reached from the daemon heartbeat, the
// witness on SLOT_OPEN, and by hand; Run is the one place all three pass
// through, so the operator hold is enforced there.

// With the hold in place nothing is read or slung: the dispatch lock is never
// taken, so the gate stops the run before the planner.
func TestRun_OperatorHold_DispatchesNothing(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(townRoot, "seat-refill.hold"), nil, 0644); err != nil {
		t.Fatal(err)
	}

	report, err := Run(t.Context(), Options{TownRoot: townRoot, Actor: "test", BatchOverride: 1}, Deps{})
	if err != nil || report.Dispatched != 0 {
		t.Fatalf("Run under a hold = (%+v, %v), want no dispatch and no error", report, err)
	}
	if _, err := os.Stat(filepath.Join(townRoot, ".runtime", "scheduler-dispatch.lock")); err == nil {
		t.Error("the dispatch lock was taken during a hold: the gate must come first")
	}
}

func TestDropRigHeldBeads_RigEstopRemovesOnlyThatRig(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(townRoot, "ESTOP.gastown"), []byte("manual\t2026-09-24T00:00:00Z\tt\n"), 0644); err != nil {
		t.Fatal(err)
	}
	plan := capacity.DispatchPlan{
		ToDispatch: []capacity.PendingBead{
			{ID: "ctx-1", WorkBeadID: "gt-a", TargetRig: "gastown"},
			{ID: "ctx-2", WorkBeadID: "om-b", TargetRig: "om"},
		},
		Skipped: 1,
	}

	r := &runner{}
	got := r.dropRigHeldBeads(townRoot, plan)

	if len(got.ToDispatch) != 1 || got.ToDispatch[0].WorkBeadID != "om-b" {
		t.Errorf("ToDispatch = %+v, want only om-b", got.ToDispatch)
	}
	if got.Skipped != 2 {
		t.Errorf("Skipped = %d, want 2 (held bead counted as skipped, not failed)", got.Skipped)
	}
}

func TestValidateDryRunMarksAllInvalidAsValidation(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeJSONFile(t, filepath.Join(townRoot, "mayor", "rigs.json"), &config.RigsConfig{
		Version: config.CurrentRigsVersion,
		Rigs: map[string]config.RigEntry{
			"testrig": {BeadsConfig: &config.BeadsConfig{Prefix: "gt"}},
		},
	})

	r := &runner{}
	plan := r.validateDryRun(townRoot, capacity.DispatchPlan{
		ToDispatch: []capacity.PendingBead{{ID: "ctx-1", WorkBeadID: "hq-one", TargetRig: "testrig"}},
		Reason:     "ready",
	})

	if len(plan.ToDispatch) != 0 || plan.Skipped != 1 || plan.Reason != "validation" {
		t.Fatalf("validated plan = %+v, want no dispatch, skipped=1, reason=validation", plan)
	}
}

func TestPrintDryRunPlanUsesCapacitySnapshot(t *testing.T) {
	t.Parallel()
	r := &runner{opts: Options{}}
	out := captureTo(func(w io.Writer) {
		r.opts.Out = w
		r.printDryRunPlanFor(capacity.DispatchPlan{
			ToDispatch: []capacity.PendingBead{{ID: "ctx-1", WorkBeadID: "gt-one", TargetRig: "gastown"}},
			Skipped:    2,
			Reason:     "capacity",
		}, Seats{
			Max:             2,
			Working:         1,
			RecoveryBlocked: 1,
			Reservations:    0,
			ReusableIdle:    3,
			Parked:          4,
			PendingMR:       2,
			Free:            0,
		}, 5)
	})
	for _, want := range []string{"0 free of 2", "working: 1", "recovery_blocked: 1", "reusable_idle: 3", "parked: 4", "pending_mr: 2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run output %q missing %q", out, want)
		}
	}
}

func TestPrintDryRunPlanValidationReasonNotCapacity(t *testing.T) {
	t.Parallel()
	r := &runner{}
	out := captureTo(func(w io.Writer) {
		r.opts.Out = w
		r.printDryRunPlanFor(capacity.DispatchPlan{
			Skipped: 2,
			Reason:  "validation",
		}, Seats{Max: 2, Free: 2}, 5)
	})
	if !strings.Contains(out, "validation failed for 2 candidate") {
		t.Fatalf("dry-run output %q missing validation reason", out)
	}
	if strings.Contains(out, "No capacity") {
		t.Fatalf("dry-run output %q should not report capacity for validation failures", out)
	}
}

func TestPrintDispatchNoOpReportsExplicitReason(t *testing.T) {
	t.Parallel()
	r := &runner{}
	out := captureTo(func(w io.Writer) {
		r.opts.Out = w
		r.printDispatchNoOp(capacity.DispatchReport{Reason: "none"}, Seats{})
	})
	if !strings.Contains(out, "No ready beads scheduled for dispatch") {
		t.Fatalf("none output = %q", out)
	}

	out = captureTo(func(w io.Writer) {
		r.opts.Out = w
		r.printDispatchNoOp(capacity.DispatchReport{Reason: "validation", Skipped: 1}, Seats{})
	})
	if !strings.Contains(out, "No dispatchable beads") || !strings.Contains(out, "validation") {
		t.Fatalf("validation output = %q", out)
	}
}
