package cmd

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/formula"
)

func formulaStepIDs(f *formula.Formula) []string {
	ids := make([]string, 0, len(f.Steps))
	for _, s := range f.Steps {
		ids = append(ids, s.ID)
	}
	return ids
}

// TestLoadResolvedFormula_ExtendingFormulas guards gt-ad1a1: gt formula show
// resolves a formula that carries only its delta, as prime does. Embedded
// formulas only (empty town root).
func TestLoadResolvedFormula_ExtendingFormulas(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		contains []string
		absent   []string
	}{
		// Inherits load-context, expands implement into audit, overrides commit-changes.
		{"mol-doc-audit", []string{"load-context", "audit", "commit-changes", "submit-and-exit"}, []string{"implement"}},
		// Declares no steps of its own; everything comes from its parent and tdd-cycle.
		{"mol-polecat-work-monorepo-tdd", []string{"load-context", "submit-and-exit"}, []string{"implement"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			raw, resolved, err := loadResolvedFormula(tt.name, "", "")
			if err != nil {
				t.Fatalf("loadResolvedFormula: %v", err)
			}
			if !formulaComposes(raw) {
				t.Fatalf("%s should extend or compose", tt.name)
			}
			ids := formulaStepIDs(resolved)
			if len(ids) <= len(raw.Steps) {
				t.Errorf("resolved has %d steps, raw %d; want inherited steps added", len(ids), len(raw.Steps))
			}
			for _, id := range tt.contains {
				if !slices.Contains(ids, id) {
					t.Errorf("resolved steps %v missing %q", ids, id)
				}
			}
			for _, id := range tt.absent {
				if slices.Contains(ids, id) {
					t.Errorf("resolved steps %v still hold expanded %q", ids, id)
				}
			}
		})
	}
}

// TestLoadResolvedFormula_PlainFormula: a formula without extends or compose
// comes back as written, so gt formula show hands it to bd unchanged.
func TestLoadResolvedFormula_PlainFormula(t *testing.T) {
	t.Parallel()
	raw, resolved, err := loadResolvedFormula("mol-polecat-work", "", "")
	if err != nil {
		t.Fatalf("loadResolvedFormula: %v", err)
	}
	if formulaComposes(raw) || raw != resolved {
		t.Fatalf("mol-polecat-work should not compose; raw and resolved should be one formula")
	}
}

func TestRenderResolvedFormula(t *testing.T) {
	t.Parallel()
	raw, resolved, err := loadResolvedFormula("mol-doc-audit", "", "")
	if err != nil {
		t.Fatalf("loadResolvedFormula: %v", err)
	}
	var sb strings.Builder
	renderResolvedFormula(&sb, raw, resolved)
	out := sb.String()
	for _, want := range []string{
		"mol-doc-audit (resolved)",
		"Extends: mol-polecat-work",
		"Expands: implement with doc-audit-slice",
		"gt formula show mol-doc-audit --raw",
		"{{slice_docs}}",
		"{{issue}}", // inherited var
		"── load-context: ",
		"── audit: ",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if got := strings.Count(out, "── "); got != len(resolved.Steps) {
		t.Errorf("rendered %d steps, want %d", got, len(resolved.Steps))
	}
}

func TestWriteResolvedFormulaJSON(t *testing.T) {
	t.Parallel()
	raw, resolved, err := loadResolvedFormula("mol-doc-audit", "", "")
	if err != nil {
		t.Fatalf("loadResolvedFormula: %v", err)
	}
	var sb strings.Builder
	if err := writeResolvedFormulaJSON(&sb, raw, resolved); err != nil {
		t.Fatalf("writeResolvedFormulaJSON: %v", err)
	}
	var got struct {
		Formula  string   `json:"formula"`
		Resolved bool     `json:"resolved"`
		Extends  []string `json:"extends"`
		Compose  struct {
			Expand []struct{ Target, With string } `json:"expand"`
		} `json:"compose"`
		Vars  map[string]json.RawMessage `json:"vars"`
		Steps []struct {
			ID    string   `json:"id"`
			Needs []string `json:"needs"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(sb.String()), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, sb.String())
	}
	if got.Formula != "mol-doc-audit" || !got.Resolved {
		t.Errorf("formula=%q resolved=%v", got.Formula, got.Resolved)
	}
	if !slices.Equal(got.Extends, []string{"mol-polecat-work"}) {
		t.Errorf("extends = %v", got.Extends)
	}
	if len(got.Compose.Expand) != 1 || got.Compose.Expand[0].Target != "implement" {
		t.Errorf("compose.expand = %+v", got.Compose.Expand)
	}
	if _, ok := got.Vars["issue"]; !ok {
		t.Errorf("inherited var issue missing from %v", got.Vars)
	}
	if len(got.Steps) != len(resolved.Steps) || got.Steps[0].ID != resolved.Steps[0].ID {
		t.Errorf("steps = %+v, want %v", got.Steps, formulaStepIDs(resolved))
	}
}

// TestParseFormulaFile_ResolvesExtends: gt formula run parses by path and must
// run the inherited steps too, matching the three-tier load.
func TestParseFormulaFile_ResolvesExtends(t *testing.T) {
	t.Parallel()
	f, err := parseFormulaFile(filepath.Join("..", "formula", "formulas", "mol-doc-audit.formula.toml"))
	if err != nil {
		t.Fatalf("parseFormulaFile: %v", err)
	}
	_, want, err := loadResolvedFormula("mol-doc-audit", "", "")
	if err != nil {
		t.Fatalf("loadResolvedFormula: %v", err)
	}
	if got := formulaStepIDs(f); !slices.Equal(got, formulaStepIDs(want)) {
		t.Errorf("steps = %v, want %v", got, formulaStepIDs(want))
	}
}
