package formularefs

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestReferencesCatchesEveryForm(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"formula run":  "gt formula run mol-prd-review --set problem=x",
		"quoted sling": `gt sling "code-review" gastown`,
		"file":         "see design.formula.toml for the shape",
		"prose":        "the mol-plan-review convoy reviews the plan",
	}
	for name, text := range cases {
		if got := References(text); len(got) == 0 {
			t.Errorf("%s: References(%q) found nothing", name, text)
		}
	}
}

func TestReferencesSparesLookalikes(t *testing.T) {
	t.Parallel()

	// mol-polecat-code-review survives the retirement, and shiny's own step id
	// is "design": neither is a reference to a deleted formula. So too a longer
	// name that only ends in mol-idea-to-plan, or one that only starts with it.
	spared := []string{
		"gt sling mol-polecat-code-review --var focus=security",
		"see mol-polecat-code-review.formula.toml",
		"id = \"design\"\nneeds = [\"design\"]",
		"follow the design doc, then do a code-review pass",
		"gt sling mol-polecat-idea-to-plan --var idea=x",
		"see mol-idea-to-planning.formula.toml",
	}
	for _, text := range spared {
		if got := References(text); len(got) > 0 {
			t.Errorf("References(%q) = %v, want nothing", text, got)
		}
	}
}

func TestReferencesNamesEachIdeaToPlanShape(t *testing.T) {
	t.Parallel()

	cases := map[string]struct{ text, want string }{
		"invocation": {"gt formula run mol-idea-to-plan --set idea=x", "invokes removed formula mol-idea-to-plan"},
		"file":       {"the retired mol-idea-to-plan.formula.toml", "names removed formula file mol-idea-to-plan.formula.toml"},
		"standalone": {"mol-idea-to-plan drives idea intake", "names removed formula mol-idea-to-plan"},
	}
	for name, c := range cases {
		if got := References(c.text); len(got) != 1 || got[0] != c.want {
			t.Errorf("%s: References(%q) = %v, want [%s]", name, c.text, got, c.want)
		}
	}
}

func TestScanFSNamesTheFile(t *testing.T) {
	t.Parallel()

	fsys := fstest.MapFS{
		"roles/crew.md": &fstest.MapFile{Data: []byte("run gt formula run mol-prd-review")},
		"roles/ok.md":   &fstest.MapFile{Data: []byte("run gt sling mol-polecat-work")},
	}
	got, err := ScanFS(fsys, "roles")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.HasPrefix(got[0], "crew.md: ") {
		t.Fatalf("ScanFS() = %v, want one crew.md line", got)
	}
}
