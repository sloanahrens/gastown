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

// TestAdviceExcludedLabelWarns pins the excluded-label advisory: a bead wearing
// a label the dispatcher excludes gets one line naming that label.
func TestAdviceExcludedLabelWarns(t *testing.T) {
	t.Parallel()
	s := adviceSpec("Add a column to the board.", "Go.", "- [ ] the column renders")
	s.Labels = []string{"bug", "overseer-only"}
	got := Advice(s)
	if len(got) != 1 {
		t.Fatalf("Advice() = %q, want one line", got)
	}
	if want := "the dispatcher will not take this bead: it wears label overseer-only"; got[0] != want {
		t.Errorf("Advice() = %q, want %q", got[0], want)
	}
}

// otherRigAdviceSpec is adviceSpec as a bead of the gastown rig in a town of the
// rigs named (gt-qotsst).
func otherRigAdviceSpec(constraints string, rigs ...string) Spec {
	s := adviceSpec("Fix the parser.", constraints, "- [ ] the parser reads the file")
	s.Rig = "gastown"
	s.TownRigs = rigs
	return s
}

// TestOtherRigAdviceOneLinePerRead pins the advisory: a Constraints line that
// reads something in another rig gets one advice line naming that rig, and the
// bead is not refused for it.
func TestOtherRigAdviceOneLinePerRead(t *testing.T) {
	t.Parallel()
	s := otherRigAdviceSpec("Read the code in the om rig before you change the parser.", "gastown", "om", "beads")
	got := Advice(s)
	if len(got) != 1 {
		t.Fatalf("Advice() = %q, want one line", got)
	}
	if !strings.Contains(got[0], "in the om rig") || !strings.Contains(got[0], "split that read") {
		t.Errorf("advice does not name the read and the split: %q", got[0])
	}
	if v := LintShape(s, defaultTemplate()); !v.Clean() {
		t.Errorf("a read in another rig refused the bead: %s", v.Line("gt-adv"))
	}
}

// TestOtherRigAdviceSilentForMention pins that a rig named as a tool the code
// calls gets neither advice nor a refusal.
func TestOtherRigAdviceSilentForMention(t *testing.T) {
	t.Parallel()
	s := otherRigAdviceSpec("Run om review on the diff before you land it.", "gastown", "om", "beads")
	if got := Advice(s); len(got) != 0 {
		t.Fatalf("Advice() = %q, want none", got)
	}
	if v := LintShape(s, defaultTemplate()); !v.Clean() {
		t.Errorf("a tool mention refused the bead: %s", v.Line("gt-adv"))
	}
}

// TestOtherRigAdviceSilentForOwnRigAndNoRead pins the two silent cases: a read
// in the bead's own rig, and a rig named with no read word.
func TestOtherRigAdviceSilentForOwnRigAndNoRead(t *testing.T) {
	t.Parallel()
	for _, constraints := range []string{
		"Read the code in the gastown rig.",
		"Use the code in the om rig.",
	} {
		if got := Advice(otherRigAdviceSpec(constraints, "gastown", "om")); len(got) != 0 {
			t.Errorf("Advice(%q) = %q, want none", constraints, got)
		}
	}
}

// TestOtherRigAdviceIgnoresOutOfScopeAndPartialNames pins that the advisory
// reads Goal and Constraints only, and that a rig name inside "from" or
// "command" is not the rig om.
func TestOtherRigAdviceIgnoresOutOfScopeAndPartialNames(t *testing.T) {
	t.Parallel()
	s := otherRigAdviceSpec("Go.", "gastown", "om")
	s.Description = strings.Replace(s.Description, "None.", "Read the code in the om rig.", 1)
	if got := Advice(s); len(got) != 0 {
		t.Fatalf("out-of-scope read advised: %q", got)
	}
	for _, constraints := range []string{
		"Read from the command log in the omega rig.",
		"Read the xom repo and the command output.",
	} {
		if got := Advice(otherRigAdviceSpec(constraints, "gastown", "om")); len(got) != 0 {
			t.Errorf("Advice(%q) = %q, want none", constraints, got)
		}
	}
}

// TestOtherRigAdviceSaysWhenTheListIsUnread pins that a prose read in another
// rig, with no rig list passed, gets one line saying the check did not run. A
// path is refused rather than advised, so it gets no advice line.
func TestOtherRigAdviceSaysWhenTheListIsUnread(t *testing.T) {
	t.Parallel()
	got := Advice(otherRigAdviceSpec("Read the code in the om rig."))
	if len(got) != 1 || got[0] != adviceRigUncheckedText {
		t.Errorf("Advice() = %q, want the not-checked line", got)
	}
	if got := Advice(otherRigAdviceSpec("Read ~/gt/om/internal/cli/cli.go for the flag.")); len(got) != 0 {
		t.Errorf("Advice() = %q, want none for a path the refusal names", got)
	}
}

// TestOtherRigAdviceUnlistedSilentForOwnRigAndProse pins the no-list silences:
// a read in the bead's own rig, and ordinary prose such as "the project repo"
// that names no rig.
func TestOtherRigAdviceUnlistedSilentForOwnRigAndProse(t *testing.T) {
	t.Parallel()
	for _, constraints := range []string{
		"Read the code in the gastown rig.",
		"Read the code in the project repo.",
		"Read the parser source in the repo before you change it.",
		"Run om review from the command line.",
	} {
		if got := Advice(otherRigAdviceSpec(constraints)); len(got) != 0 {
			t.Errorf("Advice(%q) with no list = %q, want none", constraints, got)
		}
	}
}

// TestAdviceExcludedLabelOnePerLabel pins that each excluded label gets its
// own line rather than one line naming only the first.
func TestAdviceExcludedLabelOnePerLabel(t *testing.T) {
	t.Parallel()
	s := adviceSpec("Add a column to the board.", "Go.", "- [ ] the column renders")
	s.Labels = []string{"overseer-only", "crew-only"}
	got := Advice(s)
	if len(got) != 2 {
		t.Fatalf("Advice() = %q, want two lines", got)
	}
	joined := strings.ToLower(strings.Join(got, "\n"))
	for _, label := range []string{"overseer-only", "crew-only"} {
		if !strings.Contains(joined, label) {
			t.Errorf("Advice() = %q, does not name %q", got, label)
		}
	}
}

// TestAdviceExcludedLabelIgnoresCaseAndSpace pins that the label comparison
// matches as Spec.HasLabel does, so padded and differently-cased spellings
// still warn.
func TestAdviceExcludedLabelIgnoresCaseAndSpace(t *testing.T) {
	t.Parallel()
	s := adviceSpec("Add a column to the board.", "Go.", "- [ ] the column renders")
	s.Labels = []string{" Overseer-Only "}
	got := Advice(s)
	if len(got) != 1 {
		t.Fatalf("Advice() = %q, want one line", got)
	}
	if want := "the dispatcher will not take this bead: it wears label overseer-only"; got[0] != want {
		t.Errorf("Advice() = %q, want %q", got[0], want)
	}
}

// TestAdviceOrdinaryLabelsSilent pins that labels the dispatcher takes raise
// no excluded-label advice.
func TestAdviceOrdinaryLabelsSilent(t *testing.T) {
	t.Parallel()
	s := adviceSpec("Add a column to the board.", "Go.", "- [ ] the column renders")
	s.Labels = []string{"bug", "gt:agent-note"}
	if got := Advice(s); len(got) != 0 {
		t.Fatalf("Advice() = %q, want none", got)
	}
}

// TestAdviceExcludedLabelWithSilentFailure pins that a satisfied
// failure-behavior advisory does not swallow the excluded-label one: the bead
// still gets its label line.
func TestAdviceExcludedLabelWithSilentFailure(t *testing.T) {
	t.Parallel()
	s := adviceSpec("Add a probe that checks the seat list.", "Go.",
		"- [ ] the probe runs on a tick\n- [ ] an unreadable seat list is a failure")
	s.Labels = []string{"crew-only"}
	got := Advice(s)
	if len(got) != 1 {
		t.Fatalf("Advice() = %q, want one line", got)
	}
	if want := "the dispatcher will not take this bead: it wears label crew-only"; got[0] != want {
		t.Errorf("Advice() = %q, want %q", got[0], want)
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

// TestAdviceTriggerIsWholeWord pins that a trigger must be a whole word, not
// the start of a longer token: "lockfile" is not a lock, "installer" is not an
// install, "gcloud" is not a gc step.
func TestAdviceTriggerIsWholeWord(t *testing.T) {
	t.Parallel()
	for _, goal := range []string{
		"Write the lockfile.",
		"Route the installer.",
		"Run gcloud for the board.",
		"Keep the holdout set.",
	} {
		t.Run(goal, func(t *testing.T) {
			t.Parallel()
			s := adviceSpec(goal, "Go.", "- [ ] the column renders")
			if got := Advice(s); len(got) != 0 {
				t.Fatalf("Advice() = %q, want none", got)
			}
		})
	}
}

// TestAdviceTriggerFormsCount pins that the inflections a bead writes trigger:
// "probing", "leasing", "holding", "installs", "installation".
func TestAdviceTriggerFormsCount(t *testing.T) {
	t.Parallel()
	for _, goal := range []string{
		"Keep the probing loop running.",
		"Take a lease while leasing.",
		"Keep the lock while holding.",
		"Record each install.",
		"Record the installation.",
	} {
		t.Run(goal, func(t *testing.T) {
			t.Parallel()
			s := adviceSpec(goal, "Go.", "- [ ] the column renders")
			if got := Advice(s); len(got) != 1 {
				t.Fatalf("Advice() = %q, want one line", got)
			}
		})
	}
}

// TestAdviceFailureWordNeedsReadOrStep pins that a failure word alone does not
// silence the advisory: a box must name a failure of a read or a step. The
// error count on a probe and the refusal of a label name neither.
func TestAdviceFailureWordNeedsReadOrStep(t *testing.T) {
	t.Parallel()
	for _, box := range []string{
		"- [ ] the column shows the error count",
		"- [ ] the spec lint refuses a label",
	} {
		t.Run(box, func(t *testing.T) {
			t.Parallel()
			s := adviceSpec("Add a probe that checks the seat list.", "Go.", "- [ ] the probe runs on a tick\n"+box)
			if got := Advice(s); len(got) != 1 {
				t.Fatalf("Advice() = %q, want one line", got)
			}
		})
	}
}
