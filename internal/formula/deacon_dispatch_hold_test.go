package formula

import (
	"strings"
	"testing"
)

func deaconPatrolStep(t *testing.T, id string) *Step {
	t.Helper()
	return embeddedFormulaStep(t, "mol-deacon-patrol", id)
}

func embeddedFormulaStep(t *testing.T, formula, id string) *Step {
	t.Helper()
	raw, err := GetEmbeddedFormulaContent(formula)
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent(%s): %v", formula, err)
	}
	f, err := Parse(raw)
	if err != nil {
		t.Fatalf("parsing %s: %v", formula, err)
	}
	for i := range f.Steps {
		if f.Steps[i].ID == id {
			return &f.Steps[i]
		}
	}
	t.Fatalf("step %s not found", id)
	return nil
}

// TestDeaconPatrolGatedDispatchHonorsHold pins gt-ifijm on the one dispatch
// the deacon agent performs by hand: the gated-molecule sling checks the
// operator hold (the file internal/dispatch.OperatorHold reads) and ESTOP
// before slinging.
func TestDeaconPatrolGatedDispatchHonorsHold(t *testing.T) {
	d := deaconPatrolStep(t, "dispatch-gated-molecules").Description
	for _, want := range []string{"seat-refill.hold", "GT_SEAT_REFILL_HOLD", "$GT_ROOT/ESTOP", "ESTOP.<rig>"} {
		if !strings.Contains(d, want) {
			t.Errorf("dispatch-gated-molecules does not mention %q", want)
		}
	}
	if strings.Index(d, "seat-refill.hold") > strings.Index(d, "gt sling <mol-id>") {
		t.Error("the hold check must come before the sling")
	}
}

// TestDeaconPatrolRedispatchExit2IsRetried pins the Go contract of `gt deacon
// redispatch`: exit 2 is cooldown or deferred (hold, pool full), and the
// formula keeps that message for the next patrol instead of archiving it.
func TestDeaconPatrolRedispatchExit2IsRetried(t *testing.T) {
	d := deaconPatrolStep(t, "inbox-check").Description
	for _, want := range []string{"2=try again later", "pool is full", "Leave an exit-2 message unarchived"} {
		if !strings.Contains(d, want) {
			t.Errorf("inbox-check RECOVERED_BEAD text does not contain %q", want)
		}
	}
	if strings.Contains(d, "archive the message regardless") {
		t.Error("inbox-check still archives every redispatch exit, dropping deferred beads")
	}
}

// TestConvoyFeedDispatchHonorsHold pins gt-ifijm on the feed dog: it slings
// convoy issues by hand, so it checks the operator hold, ESTOP and the target
// rig's ESTOP.<rig> before the first sling.
func TestConvoyFeedDispatchHonorsHold(t *testing.T) {
	d := embeddedFormulaStep(t, "mol-convoy-feed", "dispatch-work").Description
	for _, want := range []string{"seat-refill.hold", "GT_SEAT_REFILL_HOLD", "$GT_ROOT/ESTOP", "ESTOP.<rig>"} {
		if !strings.Contains(d, want) {
			t.Errorf("mol-convoy-feed dispatch-work does not mention %q", want)
		}
	}
	if strings.Index(d, "seat-refill.hold") > strings.Index(d, "gt sling <issue-id> <rig>") {
		t.Error("the hold check must come before the sling")
	}
}
