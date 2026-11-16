package formula

import (
	"fmt"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/steveyegge/gastown/internal/taskfile"
)

// F7 of the no-bead-access plan (docs/plans/2026-11-14-no-bead-access-plan.md)
// drops from mol-polecat-work what a file-mode seat cannot run: the Dolt
// connectivity probe, the mail step, and every instruction that reads live bead
// state. The facts those reads produced arrive in the seat's TASK.md instead.
// These tests pin the shipped bytes.

// noBeadAccessPolecatSteps strict-decodes the shipped mol-polecat-work with
// the key set bd accepts (strict_keys_test.go).
func noBeadAccessPolecatSteps(t *testing.T) []bdStep {
	t.Helper()
	var f bdFormula
	if _, err := toml.Decode(embeddedFormulaText(t, "mol-polecat-work"), &f); err != nil {
		t.Fatalf("decode mol-polecat-work: %v", err)
	}
	return f.Steps
}

// TestNoBeadAccessPolecatFormulaDropsTheDoltProbeAndMailStep covers the bead's
// acceptance grep (`connectivity|gt mail`) and the live-read instructions the
// census named (docs/design/polecat-no-bead-access-census.md): neither may come
// back.
func TestNoBeadAccessPolecatFormulaDropsTheDoltProbeAndMailStep(t *testing.T) {
	t.Parallel()
	text := strings.ToLower(embeddedFormulaText(t, "mol-polecat-work"))
	for _, banned := range []string{
		"connectivity",
		"gt mail",
		"bd list --limit 1",
		"port 3307",
		"gt dolt status",
		"read the output of `bd show` carefully",
		"if the bead notes contain",
		"check the notes field has your findings",
		"prints the current criteria",
		"exit criterion: `bd show {{issue}}` shows no",
		"exit criterion: `bd show {{issue}}` no longer asserts",
	} {
		if i := strings.Index(text, banned); i >= 0 {
			t.Errorf("mol-polecat-work still carries %q at byte %d; a file-mode seat cannot run it", banned, i)
		}
	}
}

// TestNoBeadAccessPolecatFormulaDefinesTheSeatSourceOnce pins R2: the rule for
// where a seat reads its facts is defined in one step (load-context) and every
// other step points back to it, so the two copies cannot drift.
func TestNoBeadAccessPolecatFormulaDefinesTheSeatSourceOnce(t *testing.T) {
	t.Parallel()
	if problems := seatSourceProblems(noBeadAccessPolecatSteps(t)); len(problems) > 0 {
		t.Errorf("the shipped mol-polecat-work breaks the single-source rule:\n%s", strings.Join(problems, "\n"))
	}
	for _, tc := range []struct {
		name  string
		steps []bdStep
		want  string
	}{
		{
			name: "a later step names TASK.md",
			steps: []bdStep{
				{ID: "load-context", Description: "With bead access `bd show {{issue}}`; otherwise TASK.md and the outbox."},
				{ID: "implement", Description: "Read TASK.md for the description."},
			},
			want: `step "implement" names TASK.md instead of pointing at load-context`,
		},
		{
			name: "a later step names the outbox",
			steps: []bdStep{
				{ID: "load-context", Description: "With bead access `bd show {{issue}}`; otherwise TASK.md and the outbox."},
				{ID: "commit-changes", Description: "Write notes to the outbox."},
			},
			want: `step "commit-changes" names the outbox instead of pointing at load-context`,
		},
		{
			name: "load-context defines no file-mode source",
			steps: []bdStep{
				{ID: "load-context", Description: "With bead access `bd show {{issue}}`."},
				{ID: "implement", Description: "Do the work."},
			},
			want: "load-context does not define TASK.md and the outbox for a file-mode seat",
		},
		{
			name:  "no load-context step",
			steps: []bdStep{{ID: "implement", Description: "Do the work."}},
			want:  "no load-context step defines the seat's source",
		},
	} {
		probs := seatSourceProblems(tc.steps)
		got := strings.Join(probs, "\n")
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: problems %q, want one naming %q", tc.name, got, tc.want)
		}
	}
}

// seatSourceProblems lists every way a formula breaks the single-source rule:
// load-context must define both of a seat's sources, and no other step may name
// where facts live.
func seatSourceProblems(steps []bdStep) []string {
	var problems []string
	var load string
	for _, s := range steps {
		if s.ID == "load-context" {
			load = s.Description
			continue
		}
		if strings.Contains(s.Description, "TASK.md") {
			problems = append(problems, fmt.Sprintf("step %q names TASK.md instead of pointing at load-context", s.ID))
		}
		if strings.Contains(s.Description, "outbox") {
			problems = append(problems, fmt.Sprintf("step %q names the outbox instead of pointing at load-context", s.ID))
		}
	}
	if load == "" {
		return append(problems, "no load-context step defines the seat's source")
	}
	for _, want := range []string{"bead access", "bd show {{issue}}", "TASK.md", "outbox"} {
		if !strings.Contains(load, want) {
			problems = append(problems, fmt.Sprintf("load-context does not define TASK.md and the outbox for a file-mode seat; it lacks %q", want))
		}
	}
	return problems
}

// TestNoBeadAccessPolecatFormulaCitesTheTaskfileHeadings keeps the formula's
// pointers at TASK.md headings honest: every section the formula sends a
// file-mode seat to is a heading the taskfile assembler writes, so the formula
// cannot cite a section TASK.md never carries.
func TestNoBeadAccessPolecatFormulaCitesTheTaskfileHeadings(t *testing.T) {
	t.Parallel()
	text := embeddedFormulaText(t, "mol-polecat-work")
	assembled := taskfile.Assemble(taskfile.Input{
		BeadID:      "gt-fgnho3",
		Title:       "a titled bead",
		Description: "the description",
		Boxes:       []taskfile.Box{{Text: "a box", Ticked: true}},
		Notes:       []taskfile.Note{{Text: "a note"}},
		Comments:    []taskfile.Comment{{Author: "overseer", Text: "a comment"}},
		Rework: &taskfile.Rework{
			Attempts: []taskfile.Rejection{{
				Attempt:  1,
				Kind:     "review",
				Findings: []taskfile.Finding{{ID: "f1", Severity: "major", Path: "a.go", Line: 1, Title: "a finding"}},
			}},
			Unchanged: &taskfile.UnchangedRejection{Attempt: 1, Kind: "review", Head: "abcdef1"},
		},
	})
	for _, heading := range []string{
		"Description",
		"Acceptance",
		"Notes",
		"Comments",
		"Earlier rejections",
		"Rejection unchanged",
	} {
		if !strings.Contains(text, "`"+heading+"`") {
			t.Errorf("mol-polecat-work no longer cites the TASK.md %q section; if the formula stops using it, drop it from this test", heading)
		}
		if !strings.Contains(assembled, "## "+heading) {
			t.Errorf("the taskfile assembler writes no %q heading, but mol-polecat-work sends a file-mode seat to it:\n%s", heading, assembled)
		}
	}
}

// TestNoBeadAccessPolecatBranchLookupPassesTheIDToGitAsAGlob keeps the rework
// branch lookup from splicing the bead id into a grep pattern: the id is a
// formula var, and git's glob is the one matcher that needs no escaping.
func TestNoBeadAccessPolecatBranchLookupPassesTheIDToGitAsAGlob(t *testing.T) {
	t.Parallel()
	text := embeddedFormulaText(t, "mol-polecat-work")
	if !strings.Contains(text, "git branch -r --list '*{{issue}}*'") {
		t.Error("mol-polecat-work no longer finds the prior branch with `git branch -r --list '*{{issue}}*'`")
	}
	if strings.Contains(text, "git branch -r | grep {{issue}}") {
		t.Error("mol-polecat-work splices the bead id into a grep pattern; pass it to git as a glob instead")
	}
}

// TestNoBeadAccessPolecatFormulaStillCooks pins the other half of F7: bd's
// strict decode accepts the formula's keys and shape, and every step that
// remains is renderable (an id, a title and a body) and unique.
func TestNoBeadAccessPolecatFormulaStillCooks(t *testing.T) {
	t.Parallel()
	if problems := bdStrictProblems("mol-polecat-work", []byte(embeddedFormulaText(t, "mol-polecat-work"))); len(problems) > 0 {
		t.Fatalf("bd would refuse to cook mol-polecat-work: %s", strings.Join(problems, "; "))
	}
	steps := noBeadAccessPolecatSteps(t)
	if len(steps) == 0 {
		t.Fatal("mol-polecat-work decoded no steps")
	}
	seen := make(map[string]bool, len(steps))
	for i, step := range steps {
		if step.ID == "" {
			t.Errorf("step %d has no id", i+1)
		}
		if seen[step.ID] {
			t.Errorf("step id %q appears twice", step.ID)
		}
		seen[step.ID] = true
		if strings.TrimSpace(step.Title) == "" {
			t.Errorf("step %q has no title for the checklist to render", step.ID)
		}
		if strings.TrimSpace(step.Description) == "" {
			t.Errorf("step %q has no body", step.ID)
		}
	}
	if steps[0].ID != "load-context" {
		t.Errorf("first step is %q; the seat's source is defined in load-context", steps[0].ID)
	}
}
