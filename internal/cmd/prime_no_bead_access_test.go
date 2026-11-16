package cmd

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/steveyegge/gastown/internal/formula"
)

// TestNoBeadAccessChecklistStillRendersFromTheShippedFormula pins W1b for F7
// (gt-fgnho3): after the connectivity probe and the mail step are dropped, the
// shipped formula still cooks into steps and the checklist production renders
// from them carries every remaining step title.
func TestNoBeadAccessChecklistStillRendersFromTheShippedFormula(t *testing.T) {
	t.Parallel()
	raw, err := formula.GetEmbeddedFormulaContent("mol-polecat-work")
	if err != nil {
		t.Fatalf("read the shipped mol-polecat-work: %v", err)
	}
	// bd prints the cooked tree as JSON. A generic decode of the shipped TOML
	// gives the same steps without a second copy of bd's step schema.
	var doc struct {
		Steps []map[string]any `toml:"steps"`
	}
	if _, err := toml.Decode(string(raw), &doc); err != nil {
		t.Fatalf("decode the shipped mol-polecat-work: %v", err)
	}
	tree, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encode the shipped steps as bd's tree: %v", err)
	}
	fake := &fakeCook{out: tree}
	f, err := (formulaCooker{open: fake.open}).cookForRender("mol-polecat-work", "", "", nil)
	if err != nil {
		t.Fatalf("cook the shipped mol-polecat-work: %v", err)
	}
	steps := f.checklist()
	if len(steps) == 0 {
		t.Fatal("the shipped mol-polecat-work cooked no steps")
	}
	out := renderFormulaChecklist("mol-polecat-work", steps, 1)
	header := fmt.Sprintf("**Formula Checklist** (%d steps from mol-polecat-work):", len(steps))
	if !strings.Contains(out, header) {
		t.Errorf("the checklist header does not name the %d rendered steps:\n%s", len(steps), out)
	}
	for i, s := range steps {
		if strings.TrimSpace(s.Title) == "" {
			t.Errorf("step %d (%s) has no title to render", i+1, s.ID)
			continue
		}
		if want := fmt.Sprintf("### Step %d: %s", i+1, s.Title); !strings.Contains(out, want) {
			t.Errorf("the checklist does not render step %d's title %q:\n%s", i+1, s.Title, out)
		}
	}
	for _, gone := range []string{"connectivity", "gt mail"} {
		if strings.Contains(strings.ToLower(out), gone) {
			t.Errorf("the rendered checklist still carries %q; a file-mode seat cannot run it", gone)
		}
	}
}
