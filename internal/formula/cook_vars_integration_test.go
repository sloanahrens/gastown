//go:build integration

package formula

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// cookedVar is one var of bd's cooked tree.
type cookedVar struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
}

// cookedVars is the part of bd's cooked tree these tests read.
type cookedVars struct {
	Vars           []cookedVar `json:"vars"`
	UnresolvedVars []string    `json:"unresolved_vars"`
	Steps          []struct {
		ID string `json:"id"`
	} `json:"steps"`
}

// templateKeywords are {{else}}-style block words a formula body may hold;
// bd reports them as unresolved, but they are not inputs.
var templateKeywords = []string{"else", "end", "this"}

// TestIntegrationFormulaCook_WispInputs cooks the dog and polecat formulas
// through the real bd with their inputs (GitHub #1133: a template variable
// with no [vars] entry fails bd mol wisp): every placeholder left is a
// declared var and the inputs are declared required.
func TestIntegrationFormulaCook_WispInputs(t *testing.T) {
	town := t.TempDir()
	if _, err := ProvisionFormulas(town); err != nil {
		t.Fatalf("provision formulas: %v", err)
	}
	env := append(beads.StripEnvKey(beads.StripEnvKey(os.Environ(), "GT_ROOT"), "BD_FORMULA_OVERLAY_DIR"), "GT_ROOT="+town)
	bd := beads.NewPinned(filepath.Join(town, ".beads"), beads.WithWorkDir(town), beads.WithEnv(env))

	for _, tc := range []struct {
		name   string
		inputs []string
	}{
		{"mol-dep-propagate", nil},
		{"mol-orphan-scan", nil},
		{"mol-session-gc", nil},
		{"mol-polecat-code-review", []string{"scope", "issue"}},
		{"mol-polecat-review-pr", []string{"pr_url", "issue"}},
	} {
		var vars []string
		for _, in := range tc.inputs {
			vars = append(vars, in+"=x")
		}
		out, err := bd.Cook(tc.name, vars)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		var f cookedVars
		if err := json.Unmarshal(out, &f); err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		for _, name := range f.UnresolvedVars {
			declared := slices.ContainsFunc(f.Vars, func(v cookedVar) bool { return v.Name == name })
			if !declared && !slices.Contains(templateKeywords, name) {
				t.Errorf("%s: {{%s}} has no [vars] entry and would fail wisp creation", tc.name, name)
			}
		}
		for _, in := range tc.inputs {
			if !slices.Contains(f.Vars, cookedVar{Name: in, Required: true}) {
				t.Errorf("%s: input %q is not a required var", tc.name, in)
			}
		}
	}
}
