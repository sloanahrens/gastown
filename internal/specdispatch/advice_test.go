package specdispatch

import (
	"strings"
	"testing"
)

// adviceSpec builds a shaped bead whose Goal, Constraints and acceptance list
// are the only text Advice reads.
func adviceSpec(goal, constraints, acceptance string) Spec {
	return Spec{
		ID:    "gt-adv",
		Type:  "task",
		Title: "advice fixture",
		Description: "## Goal\n" + goal + "\n\n" +
			"## Constraints\n" + constraints + "\n\n" +
			"## Out of scope\nNone.\n\n" +
			"## Gate\nmake gate\n\n" +
			"## Size\none worker, one MR",
		Acceptance: acceptance,
	}
}

// TestAdviceFailureBehaviourFires pins the advisory: a bead whose Goal names a
// probe and whose acceptance list says nothing about a failure gets one line
// telling it to add a box for the failure behavior.
func TestAdviceFailureBehaviourFires(t *testing.T) {
	t.Parallel()
	s := adviceSpec("Add a probe that checks the seat list.", "Go.", "- [ ] the probe runs on a tick")
	got := Advice(s)
	if len(got) != 1 {
		t.Fatalf("Advice() = %q, want one line", got)
	}
	if !strings.Contains(got[0], `"probe"`) {
		t.Errorf("advice does not name the triggering word: %q", got[0])
	}
	if !strings.Contains(got[0], "add a box") || !strings.Contains(got[0], "fails") {
		t.Errorf("advice does not say to add a box naming the failure behavior: %q", got[0])
	}
}

// TestAdviceFailureBehaviourSilentWithErrorBox pins the other side: the same
// bead is silent once one acceptance box names what happens when the read
// fails.
func TestAdviceFailureBehaviourSilentWithErrorBox(t *testing.T) {
	t.Parallel()
	s := adviceSpec("Add a probe that checks the seat list.", "Go.",
		"- [ ] the probe runs on a tick\n- [ ] an unreadable seat list is a failure")
	if got := Advice(s); len(got) != 0 {
		t.Fatalf("Advice() = %q, want none", got)
	}
}

// TestAdviceSilentWithoutTrigger pins that a bead naming none of the trigger
// words gets no advice, whatever its acceptance list says.
func TestAdviceSilentWithoutTrigger(t *testing.T) {
	t.Parallel()
	s := adviceSpec("Add a column to the board.", "Go.", "- [ ] the column renders")
	if got := Advice(s); len(got) != 0 {
		t.Fatalf("Advice() = %q, want none", got)
	}
}

// TestAdviceTriggerIsAWord pins the word boundary: a word that merely contains
// a trigger ("block", "threshold") is not one.
func TestAdviceTriggerIsAWord(t *testing.T) {
	t.Parallel()
	s := adviceSpec("Unblock the threshold.", "Go.", "- [ ] the column renders")
	if got := Advice(s); len(got) != 0 {
		t.Fatalf("Advice() = %q, want none", got)
	}
}

// TestAdviceReadsTheConstraints pins the second trigger source: a word in the
// Constraints fires the advisory even when the Goal has none.
func TestAdviceReadsTheConstraints(t *testing.T) {
	t.Parallel()
	s := adviceSpec("Add a column to the board.", "Take a lease before writing.", "- [ ] the column renders")
	got := Advice(s)
	if len(got) != 1 {
		t.Fatalf("Advice() = %q, want one line", got)
	}
	if !strings.Contains(got[0], `"lease"`) {
		t.Errorf("advice does not name the triggering word: %q", got[0])
	}
}
