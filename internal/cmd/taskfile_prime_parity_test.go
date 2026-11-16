package cmd

import (
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/taskfile"
)

// The no-bead-access plan (docs/plans/2026-11-14-no-bead-access-plan.md, T2)
// makes TASK.md the whole assignment a sandboxed seat reads, so it must carry
// every fact gt prime prints for the same bead. These tests read prime's own
// renderers and compare their output with taskfile.Assemble's.

// primeHookedDescriptionLines is how many description lines gt prime's
// hooked-work section prints (outputHookedBeadDetails). A description line past
// this window is not a fact prime prints, so it names no parity fact.
const primeHookedDescriptionLines = 5

// primeTaskFact is one fact gt prime prints for a hooked bead: a name and the
// text prime writes for it. The name rather than the text is what lets a
// parity failure say which fact the task file lacks.
type primeTaskFact struct {
	name string
	text string
}

// primeTaskFacts returns every fact gt prime prints for bead and its formula
// steps: the bead id and title, the description lines inside the hooked-work
// window, and each step title. Notes and comments are TASK.md content that
// prime does not print, so they name no fact here.
func primeTaskFacts(bead *beads.Issue, steps []checklistStep) []primeTaskFact {
	facts := []primeTaskFact{
		{name: "bead id", text: bead.ID},
		{name: "title", text: bead.Title},
	}
	lines := strings.Split(bead.Description, "\n")
	if len(lines) > primeHookedDescriptionLines {
		lines = lines[:primeHookedDescriptionLines]
	}
	for i, line := range lines {
		// prime indents each line under "Description:"; the line's own text is
		// the fact, so a blank line names nothing.
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		facts = append(facts, primeTaskFact{name: fmt.Sprintf("description line %d", i+1), text: line})
	}
	for i, step := range steps {
		facts = append(facts, primeTaskFact{name: fmt.Sprintf("step %d: %s", i+1, step.Title), text: step.Title})
	}
	return facts
}

// renderPrimeTaskText is the prime text gt prime prints for a hooked bead with
// an attached formula: the hooked-work bead details followed by the formula
// checklist. The fixture bead carries no dependencies, so the dependency
// section reads no bead store and the renderers run in-process.
func renderPrimeTaskText(bead *beads.Issue, steps []checklistStep) string {
	var b strings.Builder
	outputHookedBeadDetails(&b, RoleContext{}, bead)
	b.WriteString(renderFormulaChecklist("mol-polecat-work", steps, 1))
	return b.String()
}

// parityTaskFile assembles the TASK.md that bead and its steps produce. The
// bead-to-Input mapping is spelled here because the production mapping
// (internal/polecat.taskInputFromBead) lands in its own bead; the assembler
// under test is taskfile.Assemble.
func parityTaskFile(bead *beads.Issue, steps []checklistStep) string {
	in := taskfile.Input{BeadID: bead.ID, Title: bead.Title, Description: bead.Description}
	for _, item := range beads.AcceptanceItems(bead.AcceptanceCriteria) {
		if item.Boxed {
			in.Boxes = append(in.Boxes, taskfile.Box{Text: item.Box, Ticked: item.Checked})
		}
	}
	for _, step := range steps {
		in.Steps = append(in.Steps, taskfile.Step{Text: step.Title})
	}
	return taskfile.Assemble(in)
}

// checkTaskFileParity returns an error naming every fact gt prime prints that
// taskText does not carry. A fact with no text cannot be looked for, so it is
// refused by name rather than passing as present.
func checkTaskFileParity(facts []primeTaskFact, taskText string) error {
	var missing []string
	for _, fact := range facts {
		if strings.TrimSpace(fact.text) == "" {
			return fmt.Errorf("fact %q carries no text to look for in the task file", fact.name)
		}
		if !strings.Contains(taskText, fact.text) {
			missing = append(missing, fact.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the task file lacks %d fact(s) gt prime prints: %s", len(missing), strings.Join(missing, ", "))
	}
	return nil
}

// parityFixtureBead is one dispatched task bead: a title, a five-line
// description whose acceptance section prime prints, and the same two boxes in
// the bead's acceptance field.
func parityFixtureBead() *beads.Issue {
	return &beads.Issue{
		ID:    "gt-fix42",
		Title: "Carry every prime fact into the task file",
		Description: strings.Join([]string{
			"The seat reads its assignment from the task file.",
			"",
			"## Acceptance",
			"- [x] the ticked box is carried",
			"- [ ] the open box is carried",
		}, "\n"),
		AcceptanceCriteria: "- [x] the ticked box is carried\n- [ ] the open box is carried",
	}
}

// parityFixtureSteps is the formula checklist the fixture bead carries.
func parityFixtureSteps() []checklistStep {
	return []checklistStep{
		{ID: "one", Title: "Read the task file", Description: "Open TASK.md and read it."},
		{ID: "two", Title: "Do the work", Description: "Change the code the bead asks for."},
	}
}

// TestNoBeadAccessPrimeFactListIsWhatPrimePrints is the wiring guard for the
// parity claim: the fact list names real facts and gt prime's own renderers
// print each one, so the parity test cannot narrow into comparing TASK.md with
// itself.
func TestNoBeadAccessPrimeFactListIsWhatPrimePrints(t *testing.T) {
	t.Parallel()
	bead := parityFixtureBead()
	steps := parityFixtureSteps()
	facts := primeTaskFacts(bead, steps)
	primeText := renderPrimeTaskText(bead, steps)

	byName := make(map[string]primeTaskFact, len(facts))
	for _, fact := range facts {
		byName[fact.name] = fact
	}
	for _, want := range []string{
		"bead id",
		"title",
		"description line 1",
		"description line 4",
		"step 1: Read the task file",
		"step 2: Do the work",
	} {
		fact, ok := byName[want]
		if !ok {
			t.Fatalf("fact list does not name %q", want)
		}
		if !strings.Contains(primeText, fact.text) {
			t.Fatalf("gt prime does not print fact %q (%q):\n%s", want, fact.text, primeText)
		}
	}
	if _, ok := byName["description line 2"]; ok {
		t.Fatalf("a blank description line named a fact")
	}
}

// TestNoBeadAccessTaskFileCarriesEveryPrimeFact shows each fact in the prime
// payload appears in the TASK.md assembled for the same bead.
func TestNoBeadAccessTaskFileCarriesEveryPrimeFact(t *testing.T) {
	t.Parallel()
	bead := parityFixtureBead()
	steps := parityFixtureSteps()
	taskText := parityTaskFile(bead, steps)
	if taskText == "" {
		t.Fatal("the fixture bead assembled no TASK.md")
	}
	if err := checkTaskFileParity(primeTaskFacts(bead, steps), taskText); err != nil {
		t.Fatal(err)
	}
}

// TestNoBeadAccessParityFailsNamingDeletedFact shows that deleting one fact
// from the fixture's TASK.md makes the parity check fail and name that fact.
func TestNoBeadAccessParityFailsNamingDeletedFact(t *testing.T) {
	t.Parallel()
	bead := parityFixtureBead()
	steps := parityFixtureSteps()
	facts := primeTaskFacts(bead, steps)
	taskText := parityTaskFile(bead, steps)

	for _, tc := range []struct {
		name     string
		deleted  string
		wantFact string
	}{
		{"title", bead.Title, "title"},
		{"description line", "The seat reads its assignment from the task file.", "description line 1"},
		{"step title", "Do the work", "step 2: Do the work"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			deleted := strings.Replace(taskText, tc.deleted, "", 1)
			if deleted == taskText {
				t.Fatalf("the fixture TASK.md does not carry %q to delete", tc.deleted)
			}
			err := checkTaskFileParity(facts, deleted)
			if err == nil {
				t.Fatalf("parity passed with %q deleted from the task file", tc.deleted)
			}
			if !strings.Contains(err.Error(), tc.wantFact) {
				t.Fatalf("error does not name %q: %v", tc.wantFact, err)
			}
		})
	}
}

// TestNoBeadAccessParityRefusesFactWithNoText covers the checker's refusal
// branch: a fact with no text cannot be found, and must not pass as present.
func TestNoBeadAccessParityRefusesFactWithNoText(t *testing.T) {
	t.Parallel()
	err := checkTaskFileParity([]primeTaskFact{{name: "empty fact"}}, "any task text")
	if err == nil {
		t.Fatal("a fact with no text was accepted")
	}
	if !strings.Contains(err.Error(), "empty fact") {
		t.Fatalf("refusal does not name the fact: %v", err)
	}
}
