package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

// writeDogFixture creates a town with a dog kennel whose .dog.json assigns
// the given work, plus a town-level formula file when formulaTOML is non-empty.
func writeDogFixture(t *testing.T, work, formulaName, formulaTOML string) string {
	t.Helper()
	townRoot := t.TempDir()

	kennel := filepath.Join(townRoot, "deacon", "dogs", "alpha")
	if err := os.MkdirAll(kennel, 0755); err != nil {
		t.Fatal(err)
	}
	state := `{"name":"alpha","state":"working","work":"` + work + `"}`
	if err := os.WriteFile(filepath.Join(kennel, ".dog.json"), []byte(state), 0644); err != nil {
		t.Fatal(err)
	}

	if formulaTOML != "" {
		formulasDir := filepath.Join(townRoot, ".beads", "formulas")
		if err := os.MkdirAll(formulasDir, 0755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(formulasDir, formulaName+".formula.toml")
		if err := os.WriteFile(path, []byte(formulaTOML), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return townRoot
}

const constrainedFormula = `
formula = "test-dog-work"
type = "workflow"
command_allowlist = ["gt reaper", "gt convoy check"]

[[steps]]
id = "step1"
title = "Step 1"
`

const unconstrainedFormula = `
formula = "test-dog-free"
type = "workflow"

[[steps]]
id = "step1"
title = "Step 1"
`

func TestResolveDogFormulaAllowlist(t *testing.T) {
	t.Run("constrained formula returns declared entries", func(t *testing.T) {
		townRoot := writeDogFixture(t, "test-dog-work", "test-dog-work", constrainedFormula)
		name, entries := resolveDogFormulaAllowlist("dog", "alpha", townRoot)
		if name != "test-dog-work" {
			t.Errorf("formula name = %q, want test-dog-work", name)
		}
		if len(entries) != 2 || entries[0] != "gt reaper" || entries[1] != "gt convoy check" {
			t.Errorf("entries = %v, want [gt reaper, gt convoy check]", entries)
		}
	})

	t.Run("non-dog role unconstrained", func(t *testing.T) {
		townRoot := writeDogFixture(t, "test-dog-work", "test-dog-work", constrainedFormula)
		if _, entries := resolveDogFormulaAllowlist("polecat", "alpha", townRoot); entries != nil {
			t.Errorf("expected no constraint for non-dog role, got %v", entries)
		}
	})

	t.Run("missing dog name unconstrained", func(t *testing.T) {
		townRoot := writeDogFixture(t, "test-dog-work", "test-dog-work", constrainedFormula)
		if _, entries := resolveDogFormulaAllowlist("dog", "", townRoot); entries != nil {
			t.Errorf("expected no constraint without dog name, got %v", entries)
		}
	})

	t.Run("missing dog state fails open", func(t *testing.T) {
		townRoot := t.TempDir()
		if _, entries := resolveDogFormulaAllowlist("dog", "alpha", townRoot); entries != nil {
			t.Errorf("expected no constraint without .dog.json, got %v", entries)
		}
	})

	t.Run("work that is not a formula fails open", func(t *testing.T) {
		townRoot := writeDogFixture(t, "hq-abc123", "", "")
		if _, entries := resolveDogFormulaAllowlist("dog", "alpha", townRoot); entries != nil {
			t.Errorf("expected no constraint for bead-ID work, got %v", entries)
		}
	})

	t.Run("formula without allowlist unconstrained", func(t *testing.T) {
		townRoot := writeDogFixture(t, "test-dog-free", "test-dog-free", unconstrainedFormula)
		if _, entries := resolveDogFormulaAllowlist("dog", "alpha", townRoot); entries != nil {
			t.Errorf("expected no constraint for formula without allowlist, got %v", entries)
		}
	})

	t.Run("idle dog with no work unconstrained", func(t *testing.T) {
		townRoot := writeDogFixture(t, "", "", "")
		if _, entries := resolveDogFormulaAllowlist("dog", "alpha", townRoot); entries != nil {
			t.Errorf("expected no constraint for idle dog, got %v", entries)
		}
	})
}

// TestDogAllowlistBaselineCoversLifecycle pins the baseline entries that the
// dog role templates depend on — removing one would strand a constrained dog.
func TestDogAllowlistBaselineCoversLifecycle(t *testing.T) {
	required := []string{"gt dog done", "gt escalate", "gt nudge", "gt hook", "gt prime"}
	have := make(map[string]bool, len(dogAllowlistBaseline))
	for _, e := range dogAllowlistBaseline {
		have[e] = true
	}
	for _, want := range required {
		if !have[want] {
			t.Errorf("dogAllowlistBaseline missing lifecycle entry %q", want)
		}
	}
}
