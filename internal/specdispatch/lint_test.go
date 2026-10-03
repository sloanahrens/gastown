package specdispatch

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const goodDescription = `## Goal
A thing exists.

## Constraints
Go only.

## Out of scope
Nothing else.

## Gate
make gate

## Size
one worker, one MR`

const goodAcceptance = `- [ ] one
- [ ] two
- [ ] three`

// goodSpec is a plain work bead: no spec label, type task. The shape is the
// whole gate (gt-mmsr2), so it must lint clean as it stands.
func goodSpec() Spec {
	return Spec{
		ID:          "gt-good",
		Title:       "Add a thing",
		Type:        "task",
		Status:      "open",
		Description: goodDescription,
		Acceptance:  goodAcceptance,
	}
}

func defaultTemplate() Template { return Template{Sections: DefaultSections, Source: "built-in"} }

func TestLintCleanSpec(t *testing.T) {
	t.Parallel()
	v := Lint(goodSpec(), defaultTemplate())
	if !v.Clean() {
		t.Fatalf("clean spec refused: %s", v.Line("gt-good"))
	}
	if got := v.Line("gt-good"); got != "gt-good: spec lint ok" {
		t.Errorf("Line = %q", got)
	}
}

// Every required field, removed one at a time, must refuse and name that field.
func TestLintNamesEveryMissingField(t *testing.T) {
	t.Parallel()
	removeSection := func(name string) func(*Spec) {
		return func(s *Spec) {
			var kept []string
			skip := false
			for _, line := range strings.Split(s.Description, "\n") {
				if strings.HasPrefix(line, "## ") {
					skip = strings.EqualFold(strings.TrimPrefix(line, "## "), name)
				}
				if !skip {
					kept = append(kept, line)
				}
			}
			s.Description = strings.Join(kept, "\n")
		}
	}
	emptySection := func(name, body string) func(*Spec) {
		return func(s *Spec) {
			s.Description = strings.Replace(s.Description, "## "+name+"\n"+body, "## "+name+"\n", 1)
		}
	}
	cases := []struct {
		name  string
		edit  func(*Spec)
		field string
		route Route
	}{
		{"no goal", removeSection("Goal"), "## Goal", RouteRefuse},
		{"no constraints", removeSection("Constraints"), "## Constraints", RouteRefuse},
		{"no out of scope", removeSection("Out of scope"), "## Out of scope", RouteRefuse},
		{"no gate", removeSection("Gate"), "## Gate", RouteRefuse},
		{"no size", removeSection("Size"), "## Size", RouteRefuse},
		{"empty goal", emptySection("Goal", "A thing exists."), "## Goal", RouteRefuse},
		{"empty gate", emptySection("Gate", "make gate"), "## Gate", RouteRefuse},
		{"no acceptance", func(s *Spec) { s.Acceptance = "" }, "acceptance", RouteRefuse},
		{"size not one worker", func(s *Spec) {
			s.Description = strings.Replace(s.Description, "one worker, one MR", "two weeks", 1)
		}, "size", RouteRefuse},
		{"size needs planning", func(s *Spec) {
			s.Description = strings.Replace(s.Description, "one worker, one MR", "needs planning: three packages", 1)
		}, "size", RoutePlanning},
		{"needs-planning label", func(s *Spec) { s.Labels = append(s.Labels, "needs-planning") }, "size", RoutePlanning},
		{"too many acceptance items", func(s *Spec) {
			s.Acceptance = strings.Repeat("- [ ] item\n", MaxAcceptance+1)
		}, "size", RoutePlanning},
		{"epic", func(s *Spec) { s.Type = "epic" }, "not a work bead", RouteRefuse},
		{"agent bead by type", func(s *Spec) { s.Type = "agent" }, "not a work bead", RouteRefuse},
		{"agent bead by label", func(s *Spec) { s.Labels = []string{"gt:agent"} }, "not a work bead", RouteRefuse},
		{"wisp by type", func(s *Spec) { s.Type = "wisp" }, "not a work bead", RouteRefuse},
		{"wisp by label", func(s *Spec) { s.Labels = []string{"gt:wisp"} }, "not a work bead", RouteRefuse},
		// A molecule wisp reads as a plain bead: only ephemeral says what it
		// is (bd show on a live wisp, 2026-10-02).
		{"wisp by ephemeral", func(s *Spec) { s.Type = "molecule"; s.Ephemeral = true }, "not a work bead", RouteRefuse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := goodSpec()
			tc.edit(&s)
			v := Lint(s, defaultTemplate())
			if v.Route != tc.route || v.Field != tc.field {
				t.Fatalf("Lint = %+v, want route %s field %q", v, tc.route, tc.field)
			}
			line := v.Line(s.ID)
			if strings.Contains(line, "\n") || !strings.HasPrefix(line, s.ID+": ") {
				t.Errorf("report is not one line naming the bead: %q", line)
			}
			if tc.route == RouteRefuse && !strings.Contains(line, tc.field) {
				t.Errorf("refusal %q does not name field %q", line, tc.field)
			}
		})
	}
}

// Shape is property of the bead, not its type: every work-bead type lints the
// same, and neither the type nor a missing spec label is a refusal (gt-mmsr2).
func TestLintChecksEveryWorkBeadType(t *testing.T) {
	t.Parallel()
	for _, typ := range []string{"task", "bug", "feature", "", "TASK"} {
		t.Run("type "+typ, func(t *testing.T) {
			s := goodSpec()
			s.Type = typ
			if v := Lint(s, defaultTemplate()); !v.Clean() {
				t.Fatalf("type %q refused a good shape: %s", typ, v.Line(s.ID))
			}
		})
	}
	s := goodSpec()
	s.Labels = []string{"spec"} // the retired label is accepted and ignored
	if v := Lint(s, defaultTemplate()); !v.Clean() {
		t.Fatalf("retired spec label changed the verdict: %s", v.Line(s.ID))
	}
}

// A bead that is not work is refused before its shape is read, and the reason
// names what it is.
func TestLintRefusesNonWorkBeadsWithoutReadingShape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		edit func(*Spec)
		why  string
	}{
		{"epic", func(s *Spec) { s.Type = "epic" }, "type epic"},
		{"agent type", func(s *Spec) { s.Type = "agent" }, "type agent"},
		{"agent label", func(s *Spec) { s.Labels = []string{"gt:agent"} }, "label gt:agent"},
		{"wisp type", func(s *Spec) { s.Type = "wisp" }, "type wisp"},
		{"wisp label", func(s *Spec) { s.Labels = []string{"gt:wisp"} }, "label gt:wisp"},
		{"wisp ephemeral", func(s *Spec) { s.Type = "molecule"; s.Ephemeral = true }, "wisp"},
		{"message", func(s *Spec) { s.Type = "message" }, "type message"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := goodSpec()
			tc.edit(&s)
			// A shape that would otherwise be clean must not save it: the
			// sections are never read.
			v := Lint(s, defaultTemplate())
			if v.Route != RouteRefuse || v.Field != "not a work bead" || !strings.Contains(v.Reason, tc.why) {
				t.Fatalf("Lint = %+v, want refuse not a work bead ~%q", v, tc.why)
			}
			if len(v.Refusals) != 1 {
				t.Fatalf("refusals = %+v, want exactly the one", v.Refusals)
			}
			line := v.Line(s.ID)
			if strings.Contains(line, "\n") || !strings.Contains(line, "not a work bead") {
				t.Errorf("line = %q, want one line saying not a work bead", line)
			}
		})
	}
}

func TestNotWorkBead(t *testing.T) {
	t.Parallel()
	if why := NotWorkBead(goodSpec()); why != "" {
		t.Errorf("task is a work bead: %q", why)
	}
	types := NonWorkBeadTypes()
	for _, want := range []string{EpicType, "wisp", "agent"} {
		if !slices.Contains(types, want) {
			t.Errorf("NonWorkBeadTypes = %v, missing %q", types, want)
		}
	}
	// The list is freshly built, not a shared slice a caller could mutate.
	types[0] = "task"
	if got := NotWorkBead(Spec{Type: EpicType}); got != "type epic" {
		t.Errorf("NonWorkBeadTypes aliases the shared list: %q", got)
	}
}

// Every failure is reported, in check order, with the first as Field/Reason:
// the one-line report is unchanged and --json can act on the rest.
func TestLintListsEveryRefusalInOrder(t *testing.T) {
	t.Parallel()
	s := goodSpec()
	s.Description = "## Goal\nA thing."
	s.Acceptance = ""
	v := Lint(s, defaultTemplate())
	if v.Route != RouteRefuse || v.Field != "## Constraints" {
		t.Fatalf("first refusal = %+v, want ## Constraints", v)
	}
	var fields []string
	for _, r := range v.Refusals {
		fields = append(fields, r.Field)
	}
	if got := strings.Join(fields, "|"); got != "## Constraints|## Out of scope|## Gate|## Size|acceptance" {
		t.Fatalf("refusals = %s", got)
	}
	if v.Line(s.ID) != "gt-good: spec lint refused: ## Constraints: section missing" {
		t.Errorf("line = %q", v.Line(s.ID))
	}
}

// ShapeNote is the comment a warn-mode shape gate leaves on a bead: every
// refusal, or the planning route, in seat-refill's own text (gt-cq5gb).
func TestVerdictShapeNote(t *testing.T) {
	t.Parallel()
	if got := (Verdict{Route: RouteDispatch}).ShapeNote(); got != "" {
		t.Errorf("clean verdict note = %q, want empty", got)
	}
	planning := Verdict{Route: RoutePlanning, Field: "size", Reason: "size says: needs planning"}
	if got := planning.ShapeNote(); got != "SHAPE: needs planning" {
		t.Errorf("planning note = %q", got)
	}

	s := goodSpec()
	s.Description = "## Goal\nA thing."
	s.Acceptance = ""
	if got := Lint(s, defaultTemplate()).ShapeNote(); got != "SHAPE: ## Constraints: section missing; ## Out of scope: section missing; ## Gate: section missing; ## Size: section missing; acceptance: no acceptance criteria" {
		t.Errorf("refusal note = %q", got)
	}
	// A refused verdict with no refusals still renders a note.
	if got := (Verdict{Route: RouteRefuse}).ShapeNote(); got != "SHAPE: refused" {
		t.Errorf("empty refusal note = %q", got)
	}
}

// UnshapedReason is the dispatcher's hold reason for a refused bead: every
// failed field, in check order, after the "unshaped:" prefix. A clean or
// planning verdict has none — the planner's route is not an unshaped hold
// (gt-f8ppx).
func TestVerdictUnshapedReason(t *testing.T) {
	t.Parallel()
	if got := (Verdict{Route: RouteDispatch}).UnshapedReason(); got != "" {
		t.Errorf("clean verdict reason = %q, want empty", got)
	}
	if got := (Verdict{Route: RoutePlanning, Field: "size", Reason: "size says: needs planning"}).UnshapedReason(); got != "" {
		t.Errorf("planning verdict reason = %q, want empty", got)
	}

	s := goodSpec()
	s.Description = "## Goal\nA thing."
	s.Acceptance = ""
	if got := Lint(s, defaultTemplate()).UnshapedReason(); got != "unshaped: ## Constraints, ## Out of scope, ## Gate, ## Size, acceptance" {
		t.Errorf("refusal reason = %q", got)
	}

	oneField := goodSpec()
	oneField.Description = strings.Replace(oneField.Description, "## Gate\nmake gate", "", 1)
	if got := Lint(oneField, defaultTemplate()).UnshapedReason(); got != "unshaped: ## Gate" {
		t.Errorf("one-field reason = %q", got)
	}
}

func TestLintAcceptanceFallsBackToDescriptionSection(t *testing.T) {
	t.Parallel()
	s := goodSpec()
	s.Acceptance = ""
	s.Description += "\n\n## Acceptance criteria\n- [ ] a\n- [ ] b\n"
	if v := Lint(s, defaultTemplate()); !v.Clean() {
		t.Fatalf("acceptance section not read: %s", v.Line(s.ID))
	}
}

func TestCountAcceptance(t *testing.T) {
	t.Parallel()
	cases := map[string]int{
		"":                            0,
		"   ":                         0,
		"a single sentence":           1,
		"- [ ] a\n- [x] b\n* c\n1. d": 4,
		"- a\n  continuation\n- b":    2,
	}
	for in, want := range cases {
		if got := CountAcceptance(in); got != want {
			t.Errorf("CountAcceptance(%q) = %d, want %d", in, got, want)
		}
	}
}

// The real template's headings must parse to the built-in list, so the lint
// and /workorder read one shape.
func TestParseTemplateSectionsMatchesTemplateShape(t *testing.T) {
	t.Parallel()
	template := "# Spec template\n\n```bash\nbd create --description=\"## Goal\n<x>\n\n## Constraints\n<y>\n\n## Out of scope\n<z>\n\n## Gate\n<g>\n\n## Size\none worker, one MR\"\n```\n"
	got := ParseTemplateSections(template)
	if strings.Join(got, "|") != strings.Join(DefaultSections, "|") {
		t.Fatalf("sections = %v, want %v", got, DefaultSections)
	}
	// A heading that opens the quoted description string still counts.
	quoted := "bd create --description=\"## Goal\nx\n## Extra\ny\""
	if got := ParseTemplateSections(quoted); strings.Join(got, "|") != "Goal|Extra" {
		t.Fatalf("quoted heading sections = %v", got)
	}
}

func TestLoadTemplateFileWinsAndFallsBack(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "spec-template.md")
	if err := os.WriteFile(path, []byte("## Goal\n## Rollback\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tmpl := LoadTemplate(path)
	if tmpl.Source != path || strings.Join(tmpl.Sections, "|") != "Goal|Rollback" {
		t.Fatalf("LoadTemplate(file) = %+v", tmpl)
	}
	s := goodSpec()
	if v := Lint(s, tmpl); v.Field != "## Rollback" {
		t.Fatalf("template section not enforced: %+v", v)
	}
	fallback := LoadTemplate(filepath.Join(dir, "missing.md"))
	if fallback.Source != "built-in" || strings.Join(fallback.Sections, "|") != strings.Join(DefaultSections, "|") {
		t.Fatalf("fallback = %+v", fallback)
	}
}
