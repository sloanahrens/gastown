package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/scheduler/capacity"
)

func TestDispatchScheduledWorkReportsHeldLock(t *testing.T) {
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

	_, err = dispatchScheduledWork(townRoot, "test", 1, false)
	if err == nil {
		t.Fatal("dispatchScheduledWork succeeded with held scheduler lock")
	}
	if !strings.Contains(err.Error(), "scheduler dispatch already in progress") || !strings.Contains(err.Error(), lockFile) {
		t.Fatalf("error = %q, want explicit held lock reason with path", err.Error())
	}
}

func TestValidateDryRunDispatchPlanMarksAllInvalidAsValidation(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	writeJSONFile(t, filepath.Join(townRoot, "mayor", "rigs.json"), &config.RigsConfig{
		Version: config.CurrentRigsVersion,
		Rigs: map[string]config.RigEntry{
			"testrig": {BeadsConfig: &config.BeadsConfig{Prefix: "gt"}},
		},
	})

	plan := validateDryRunDispatchPlan(townRoot, capacity.DispatchPlan{
		ToDispatch: []capacity.PendingBead{{ID: "ctx-1", WorkBeadID: "hq-one", TargetRig: "testrig"}},
		Reason:     "ready",
	})

	if len(plan.ToDispatch) != 0 || plan.Skipped != 1 || plan.Reason != "validation" {
		t.Fatalf("validated plan = %+v, want no dispatch, skipped=1, reason=validation", plan)
	}
}
