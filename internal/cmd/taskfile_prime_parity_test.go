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
// every fact the assignment for a bead holds: the facts gt prime prints for the
// same bead, and the bead content prime prints nowhere. These tests read
// prime's own renderers and compare their output with taskfile.Assemble's.

// taskFileSection names the part of TASK.md a fact must appear in. A fact is
// only carried when it is in its own section: the same text sitting elsewhere
// in the file is not the fact the seat needs.
type taskFileSection string

const (
	// taskFileHeading is the "# TASK ..." line above the first "## " heading.
	taskFileHeading     taskFileSection = ""
	taskFileDescription taskFileSection = "Description"
	taskFileAcceptance  taskFileSection = "Acceptance"
	taskFileNotes       taskFileSection = "Notes"
	taskFileComments    taskFileSection = "Comments"
	taskFileSteps       taskFileSection = "Steps"
)

// primeTaskFact is one fact the assignment for a bead holds: a name, the
// section of TASK.md that must carry it, and the text to look for. The name
// rather than the text is what lets a parity failure say which fact the task
// file lacks.
type primeTaskFact struct {
	name    string
	section taskFileSection
	text    string
}

// primeDescriptionFacts reads the description facts out of gt prime's own
// renderer output: the lines outputHookedBeadDetails prints under
// "  Description:", in order. Reading them from the output rather than counting
// them here is what keeps the fact list on prime's window: a line prime stops
// printing stops being a fact, and the window's size is never copied.
func primeDescriptionFacts(primeText string) []primeTaskFact {
	lines := strings.Split(primeText, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "Description:" {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return nil
	}
	// Prime indents each printed description line under the "Description:"
	// label, and the block ends at the first line it does not indent.
	var printed []string
	for _, line := range lines[start:] {
		if !strings.HasPrefix(line, "    ") {
			break
		}
		printed = append(printed, strings.TrimPrefix(line, "    "))
	}
	// A truncated description ends with prime's own "..." marker, which stands
	// for the lines it did not print and so is no fact.
	if n := len(printed); n > 0 && strings.TrimSpace(printed[n-1]) == "..." {
		printed = printed[:n-1]
	}
	var facts []primeTaskFact
	for i, text := range printed {
		if strings.TrimSpace(text) == "" {
			continue // a blank line names nothing to look for
		}
		// Prime prints the description from its first line, so a printed
		// line's position is its line number in the description.
		facts = append(facts, primeTaskFact{
			name:    fmt.Sprintf("description line %d", i+1),
			section: taskFileDescription,
			text:    text,
		})
	}
	return facts
}

// primePrintedFacts is every fact gt prime prints for bead and its formula
// steps: the bead id, its title, the description lines inside prime's window,
// and each step title.
func primePrintedFacts(primeText string, bead *beads.Issue, steps []checklistStep) []primeTaskFact {
	facts := []primeTaskFact{
		{name: "bead id", section: taskFileHeading, text: bead.ID},
		{name: "title", section: taskFileHeading, text: bead.Title},
	}
	facts = append(facts, primeDescriptionFacts(primeText)...)
	for i, step := range steps {
		facts = append(facts, primeTaskFact{
			name:    fmt.Sprintf("step %d: %s", i+1, step.Title),
			section: taskFileSteps,
			text:    step.Title,
		})
	}
	return facts
}

// beadNotesText is the bead's notes, one entry per non-blank line. The fixture's
// notes are one line each, and turning a bead's notes into taskfile.Note
// entries is the spawn mapping's job (its own bead). The assembler's
// newest-notes cap is pinned by taskfile's own tests, so the fixture stays well
// under it and every note it carries is a fact the task file must show.
func beadNotesText(bead *beads.Issue) []string {
	var notes []string
	for _, line := range strings.Split(bead.Notes, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			notes = append(notes, line)
		}
	}
	return notes
}

// acceptanceBoxText is the line the seat reads for one acceptance box: the box
// with its state. The state is part of the fact, because a box carried without
// its tick tells the seat something the bead does not say.
func acceptanceBoxText(item beads.AcceptanceItem) string {
	if item.Checked {
		return "- [x] " + item.Box
	}
	return "- [ ] " + item.Box
}

// beadContentFacts is the facts the seat needs that gt prime prints nowhere but
// the task file must carry: the bead's acceptance boxes with their state, its
// notes, and its open comments.
func beadContentFacts(bead *beads.Issue) []primeTaskFact {
	var facts []primeTaskFact
	for i, item := range beads.AcceptanceItems(bead.AcceptanceCriteria) {
		if !item.Boxed {
			continue
		}
		facts = append(facts, primeTaskFact{
			name:    fmt.Sprintf("acceptance box %d", i+1),
			section: taskFileAcceptance,
			text:    acceptanceBoxText(item),
		})
	}
	for i, note := range beadNotesText(bead) {
		facts = append(facts, primeTaskFact{name: fmt.Sprintf("note %d", i+1), section: taskFileNotes, text: note})
	}
	for i, comment := range bead.Comments {
		facts = append(facts, primeTaskFact{name: fmt.Sprintf("comment %d", i+1), section: taskFileComments, text: comment.Text})
	}
	return facts
}

// primeTaskFacts is every fact the assignment for bead holds, in one list to
// check the task file against.
func primeTaskFacts(townRoot string, bead *beads.Issue, steps []checklistStep) []primeTaskFact {
	facts := primePrintedFacts(renderPrimeTaskText(townRoot, bead, steps), bead, steps)
	return append(facts, beadContentFacts(bead)...)
}

// renderPrimeTaskText is the prime text gt prime prints for a hooked bead with
// an attached formula: the hooked-work bead details followed by the formula
// checklist. townRoot is a caller-provided temp dir, so any bead-store read the
// dependency section would make lands outside the live tree and the package
// directory rather than in them.
func renderPrimeTaskText(townRoot string, bead *beads.Issue, steps []checklistStep) string {
	var b strings.Builder
	outputHookedBeadDetails(&b, RoleContext{TownRoot: townRoot}, bead)
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
	for _, note := range beadNotesText(bead) {
		in.Notes = append(in.Notes, taskfile.Note{Text: note})
	}
	for _, comment := range bead.Comments {
		in.Comments = append(in.Comments, taskfile.Comment{Author: comment.Author, Text: comment.Text})
	}
	for _, step := range steps {
		in.Steps = append(in.Steps, taskfile.Step{Text: step.Title})
	}
	return taskfile.Assemble(in)
}

// taskFileSections splits a TASK.md text into the text above its first "## "
// heading and one body per "## Name" heading. A section the text does not have
// is absent from the map, so a fact that names it is reported missing.
func taskFileSections(taskText string) map[taskFileSection]string {
	sections := make(map[taskFileSection]string)
	current := taskFileHeading
	for _, line := range strings.Split(taskText, "\n") {
		if name, ok := strings.CutPrefix(line, "## "); ok {
			current = taskFileSection(strings.TrimSpace(name))
		}
		sections[current] += line + "\n"
	}
	return sections
}

// checkTaskFileParity returns an error naming every fact the assignment holds
// that its own section of taskText does not carry. A fact with no text cannot
// be looked for, so it is refused by name rather than passing as present.
func checkTaskFileParity(facts []primeTaskFact, taskText string) error {
	sections := taskFileSections(taskText)
	var missing []string
	for _, fact := range facts {
		if strings.TrimSpace(fact.text) == "" {
			return fmt.Errorf("fact %q carries no text to look for in the task file", fact.name)
		}
		if !strings.Contains(sections[fact.section], fact.text) {
			missing = append(missing, fact.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the task file lacks %d fact(s) the assignment carries: %s", len(missing), strings.Join(missing, ", "))
	}
	return nil
}

// parityFixtureBead is one dispatched task bead: a seven-line description (two
// lines past the window gt prime prints), two acceptance boxes in different
// states, two notes and one open comment.
func parityFixtureBead() *beads.Issue {
	return &beads.Issue{
		ID:    "gt-fix42",
		Title: "Carry every prime fact into the task file",
		Description: strings.Join([]string{
			"The seat reads its assignment from the task file.",
			"",
			"Line three is prose, not a heading.",
			"Line four is prose the window reaches.",
			"Line five is the window's last line.",
			"Line six is past the window prime prints.",
			"Line seven is past the window too.",
		}, "\n"),
		AcceptanceCriteria: "- [x] the ticked box is carried\n- [ ] the open box is carried",
		Notes:              "The first note is landed.\nThe second note is the newest.",
		Comments: []beads.Comment{
			{Author: "overseer", Text: "the reviewer asked for the notes tail"},
		},
	}
}

// parityFixtureSteps is the formula checklist the fixture bead carries.
func parityFixtureSteps() []checklistStep {
	return []checklistStep{
		{ID: "one", Title: "Read the task file", Description: "Open TASK.md and read it."},
		{ID: "two", Title: "Do the work", Description: "Change the code the bead asks for."},
	}
}

// TestNoBeadAccessParityFixtureCarriesNoDependency keeps the renderer call
// hermetic: the fixture carries no dependency, so the hooked-work section's
// dependency lookup returns before it can read a bead store.
func TestNoBeadAccessParityFixtureCarriesNoDependency(t *testing.T) {
	t.Parallel()
	bead := parityFixtureBead()
	if bead.DependencyCount != 0 || len(bead.DependsOn) != 0 || len(bead.BlockedBy) != 0 {
		t.Fatalf("fixture bead carries dependencies, so rendering it would read a bead store")
	}
}

// TestNoBeadAccessPrimeFactListIsWhatPrimePrints is the wiring guard for the
// parity claim: the fact list names real facts, gt prime's own renderers print
// each fact it claims to print, and the list carries the bead content prime
// prints nowhere, so the parity test cannot narrow into comparing TASK.md with
// itself.
func TestNoBeadAccessPrimeFactListIsWhatPrimePrints(t *testing.T) {
	t.Parallel()
	bead := parityFixtureBead()
	steps := parityFixtureSteps()
	townRoot := t.TempDir()
	primeText := renderPrimeTaskText(townRoot, bead, steps)

	for _, fact := range primePrintedFacts(primeText, bead, steps) {
		if !strings.Contains(primeText, fact.text) {
			t.Fatalf("gt prime does not print fact %q (%q):\n%s", fact.name, fact.text, primeText)
		}
	}

	facts := primeTaskFacts(townRoot, bead, steps)
	byName := make(map[string]primeTaskFact, len(facts))
	for _, fact := range facts {
		byName[fact.name] = fact
	}
	for _, want := range []string{
		"bead id",
		"title",
		"description line 1",
		"description line 3",
		"description line 5",
		"acceptance box 1",
		"acceptance box 2",
		"note 1",
		"note 2",
		"comment 1",
		"step 1: Read the task file",
		"step 2: Do the work",
	} {
		if _, ok := byName[want]; !ok {
			t.Fatalf("fact list does not name %q", want)
		}
	}
	// The fixture's description is longer than prime's window, so a fact list
	// that counted lines itself would name these lines prime never prints.
	for _, past := range []string{"description line 6", "description line 7"} {
		if _, ok := byName[past]; ok {
			t.Fatalf("fact list names %q, a description line gt prime does not print", past)
		}
	}
	if fact := byName["acceptance box 1"]; fact.text != "- [x] the ticked box is carried" {
		t.Fatalf("acceptance box fact does not carry its ticked state: %q", fact.text)
	}
}

// TestNoBeadAccessTaskFileCarriesEveryPrimeFact shows each fact the assignment
// holds appears in its own section of the TASK.md assembled for the same bead.
func TestNoBeadAccessTaskFileCarriesEveryPrimeFact(t *testing.T) {
	t.Parallel()
	bead := parityFixtureBead()
	steps := parityFixtureSteps()
	taskText := parityTaskFile(bead, steps)
	if taskText == "" {
		t.Fatal("the fixture bead assembled no TASK.md")
	}
	if err := checkTaskFileParity(primeTaskFacts(t.TempDir(), bead, steps), taskText); err != nil {
		t.Fatal(err)
	}
}

// TestNoBeadAccessParityFailsNamingDeletedFact shows that deleting one fact, or
// flipping an acceptance box's state, in the fixture's TASK.md makes the parity
// check fail and name that fact.
func TestNoBeadAccessParityFailsNamingDeletedFact(t *testing.T) {
	t.Parallel()
	bead := parityFixtureBead()
	steps := parityFixtureSteps()
	facts := primeTaskFacts(t.TempDir(), bead, steps)
	taskText := parityTaskFile(bead, steps)

	for _, tc := range []struct {
		name     string
		from     string
		to       string
		wantFact string
	}{
		{"title", bead.Title, "", "title"},
		{"description line", "The seat reads its assignment from the task file.", "", "description line 1"},
		{"last description line in the window", "Line five is the window's last line.", "", "description line 5"},
		{"acceptance box state", "- [x] the ticked box is carried", "- [ ] the ticked box is carried", "acceptance box 1"},
		{"note", "The second note is the newest.", "", "note 2"},
		{"comment", "the reviewer asked for the notes tail", "", "comment 1"},
		{"step title", "Do the work", "", "step 2: Do the work"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mutated := strings.Replace(taskText, tc.from, tc.to, 1)
			if mutated == taskText {
				t.Fatalf("the fixture TASK.md does not carry %q to remove", tc.from)
			}
			err := checkTaskFileParity(facts, mutated)
			if err == nil {
				t.Fatalf("parity passed with %q removed from the task file", tc.from)
			}
			if !strings.Contains(err.Error(), tc.wantFact) {
				t.Fatalf("error does not name %q: %v", tc.wantFact, err)
			}
		})
	}
}

// TestNoBeadAccessParityIgnoresTextOutsideTheFactSection covers the checker's
// scoping: a fact's text somewhere else in TASK.md is not that fact, so moving
// a note's text into the description must still fail.
func TestNoBeadAccessParityIgnoresTextOutsideTheFactSection(t *testing.T) {
	t.Parallel()
	bead := parityFixtureBead()
	steps := parityFixtureSteps()
	facts := primeTaskFacts(t.TempDir(), bead, steps)
	taskText := parityTaskFile(bead, steps)

	note := beadNotesText(bead)[1]
	moved := strings.Replace(taskText, "- "+note+"\n", "", 1)
	if moved == taskText {
		t.Fatalf("the fixture TASK.md does not carry note %q in its notes section", note)
	}
	moved = strings.Replace(moved, bead.Description, bead.Description+"\n"+note, 1)
	if !strings.Contains(moved, note) {
		t.Fatalf("the moved note text is not in the task file to be matched elsewhere")
	}

	err := checkTaskFileParity(facts, moved)
	if err == nil {
		t.Fatalf("parity passed with note %q only in the description section", note)
	}
	if !strings.Contains(err.Error(), "note 2") {
		t.Fatalf("error does not name the note left out of its section: %v", err)
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
