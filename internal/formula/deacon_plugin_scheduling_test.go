package formula

import (
	"strings"
	"testing"
)

// TestDeaconPatrolDoesNotSchedulePlugins pins the single-scheduler rule
// (gt-o1z7): the daemon heartbeat dispatches plugins — `dispatchPlugins` in
// internal/daemon/handler.go reads each gate and runs the open ones — and no
// patrol does. The deacon's plugin-run step used to send the agent through the
// same gate evaluation, against a run-state file (state.json) the daemon
// replaced with ephemeral receipts, so a deacon following it re-ran a plugin
// the daemon had already dispatched.
//
// The step stays in the formula: a manual-gate plugin has no automatic path,
// and running it is the one piece of scheduling the daemon cannot do. What is
// pinned here is that the step describes the daemon's ownership and points at
// the manual trigger, rather than re-deriving gates itself.
func TestDeaconPatrolDoesNotSchedulePlugins(t *testing.T) {
	raw, err := GetEmbeddedFormulaContent("mol-deacon-patrol")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent(mol-deacon-patrol): %v", err)
	}
	f, err := Parse(raw)
	if err != nil {
		t.Fatalf("parsing mol-deacon-patrol: %v", err)
	}

	var step *Step
	for i := range f.Steps {
		if f.Steps[i].ID == "plugin-run" {
			step = &f.Steps[i]
			break
		}
	}
	if step == nil {
		t.Fatal("plugin-run step not found: a gate the daemon does not dispatch has no other trigger")
	}

	// The daemon is named as the scheduler, so an agent reading the step knows
	// not to evaluate gates itself.
	for _, want := range []string{"daemon", "dispatchPlugins", "gt plugin run", "manual"} {
		if !strings.Contains(step.Description, want) {
			t.Errorf("plugin-run step does not mention %q; the step must name the daemon as the scheduler and the manual trigger as its own work", want)
		}
	}

	// The retired mechanism: a state file no longer written, and the gate scan
	// that read it. A step that still instructs either re-creates the second
	// scheduler.
	for _, banned := range []string{"state.json", "Compare against", "If gate is open, execute"} {
		if strings.Contains(step.Description, banned) {
			t.Errorf("plugin-run step still instructs %q, a mechanism the daemon replaced (gt-o1z7)", banned)
		}
	}

	// R7: the step ends on a condition the agent can check.
	if !strings.Contains(step.Description, "Exit criteria") {
		t.Error("plugin-run step has no exit criteria")
	}
}
