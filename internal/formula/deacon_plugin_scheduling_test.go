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

	// The step pins behaviour, not internals: the daemon owns dispatch, and
	// the step is the on-demand trigger for parked plugins. The pinned words
	// are what an agent must see to act correctly; pinning a Go identifier
	// would break on a rename, and the deacon runs in a town whose working
	// tree may not contain internal/daemon/handler.go.
	for _, want := range []string{"daemon", "gt plugin run", "manual", "nothing to run"} {
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

	// gt-o1z7 rework: a manual-gate plugin stays parked. The step must not
	// un-park every manual plugin every cycle; it runs one only when
	// something explicitly names it.
	if !strings.Contains(step.Description, "only when a bead, mail, or mayor") {
		t.Error("plugin-run step does not require explicit naming before running a manual plugin: every-patrol un-parking is the retired behaviour")
	}

	// gt-o1z7 rework: fail closed. `gt plugin run` prints instructions; the
	// recorded result must be the plugin's real outcome. A step whose exit
	// criterion is met by typing the command, whether or not the work
	// happened, is fail-open.
	if !strings.Contains(step.Description, "only prints the plugin's instructions") {
		t.Error("plugin-run step does not say `gt plugin run` only prints instructions that must then be executed")
	}
}
