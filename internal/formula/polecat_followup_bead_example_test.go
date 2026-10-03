package formula

import (
	"regexp"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

// The Discovered work block's example is what a polecat pastes when it finds
// work outside its bead (gt-vpeaj). These pull the literal description and
// acceptance out of the embedded formula text, so the lint below checks the
// text a reader copies, not a copy kept here to drift.
var (
	followUpDescRe   = regexp.MustCompile(`(?s)desc="\$\(cat <<'BEAD'\n(.*?)\nBEAD\n\)"`)
	followUpCreateRe = regexp.MustCompile(`(?ms)^gt bead create .*?-q$`)
	followUpAcceptRe = regexp.MustCompile(`--acceptance "([^"]+)"`)
)

// TestPolecatFollowUpBeadExampleLintsClean guards the Discovered work example
// against the dispatcher's own lint: filing a follow-up in this shape costs the
// filer nothing, and any other shape holds the bead off the dispatch path until
// someone reshapes it (gt-vpeaj).
func TestPolecatFollowUpBeadExampleLintsClean(t *testing.T) {
	t.Parallel()
	raw, err := GetEmbeddedFormulaContent("mol-polecat-work")
	if err != nil {
		t.Fatalf("GetEmbeddedFormulaContent(mol-polecat-work): %v", err)
	}
	text := string(raw)

	desc := followUpDescRe.FindStringSubmatch(text)
	if desc == nil {
		t.Fatal("the Discovered work example carries no desc=(cat <<'BEAD') heredoc")
	}
	create := followUpCreateRe.FindString(text)
	if create == "" {
		t.Fatal("the Discovered work example carries no gt bead create ... -q line")
	}
	acc := followUpAcceptRe.FindStringSubmatch(create)
	if acc == nil {
		t.Fatal("the Discovered work example's gt bead create line carries no --acceptance \"...\"")
	}
	if !strings.Contains(create, "--type task") {
		t.Errorf("the example does not file --type task, and bd's bug type demands a Steps to Reproduce section: %s", create)
	}
	if !strings.Contains(acc[1], "- [ ]") {
		t.Errorf("acceptance %q carries no '- [ ]' item", acc[1])
	}

	spec := specdispatch.Spec{
		ID:          "gt-example",
		Title:       "Found: <short description>",
		Type:        "task",
		Status:      "open",
		Description: desc[1],
		Acceptance:  acc[1],
	}
	if v := specdispatch.Lint(spec, specdispatch.LoadTemplate("")); !v.Clean() {
		t.Fatalf("the Discovered work example does not lint clean against the built-in template: %s", v.Line(spec.ID))
	}
}
