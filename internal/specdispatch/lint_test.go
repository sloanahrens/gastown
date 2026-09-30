package specdispatch

import (
	"os"
	"path/filepath"
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

func goodSpec() Spec {
	return Spec{
		ID:          "gt-good",
		Title:       "Add a thing",
		Type:        "feature",
		Status:      "open",
		Labels:      []string{"spec"},
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
		{"type task", func(s *Spec) { s.Type = "task" }, "type", RouteRefuse},
		{"type empty", func(s *Spec) { s.Type = "" }, "type", RouteRefuse},
		{"no spec label", func(s *Spec) { s.Labels = nil }, "label spec", RouteRefuse},
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
