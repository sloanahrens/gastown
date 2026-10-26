package beadsfake

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// The sample formula RunFormulaContract's success cases answer for. A caller
// seeding the real seam writes SampleFormulaTOML to
// <workspace>/.beads/formulas/<SampleFormulaName>.formula.toml; the fake seeds
// the same name, type and description with SeedFormula.
const (
	SampleFormulaName        = "mol-contract-sample"
	SampleFormulaType        = "workflow"
	SampleFormulaDescription = "A sample formula for the formula seam contract."
)

// SampleFormulaTOML is the formula file the real seam answers for: a workflow
// with one step, because bd's formula list prose groups and names workflows.
func SampleFormulaTOML() string {
	return fmt.Sprintf(`formula = %q
version = 1
type = %q
description = %q

[[steps]]
id = "sample-step"
title = "The one step"
description = "Does nothing; it makes the sample a workflow bd lists."
`, SampleFormulaName, SampleFormulaType, SampleFormulaDescription)
}

// SeedSampleFormula registers the sample formula on f, the fake's half of what
// SampleFormulaTOML gives the real seam.
func SeedSampleFormula(f *Fake) {
	f.SeedFormula(SampleFormulaName, SampleFormulaType, SampleFormulaDescription)
}

// RunFormulaContract checks the behavior every beads.FormulaVerbs must share:
// the fake in the unit tier, *beads.Beads against a real bd in the integration
// tier. newVerbs returns a seam that answers for the sample formula and for no
// name the contract invents, so the fake and bd are held to one description of
// the verb surface (gt-p85pw.11).
func RunFormulaContract(t *testing.T, newVerbs func(t *testing.T) beads.FormulaVerbs) {
	t.Helper()

	t.Run("list is the JSON payload", func(t *testing.T) {
		out, err := newVerbs(t).FormulaList()
		if err != nil {
			t.Fatalf("FormulaList: %v", err)
		}
		var entries []struct {
			Name        string `json:"name"`
			Type        string `json:"type"`
			Description string `json:"description"`
			Source      string `json:"source"`
		}
		if err := json.Unmarshal(out, &entries); err != nil {
			t.Fatalf("FormulaList = %q, want bd's JSON payload: %v", out, err)
		}
		found := false
		for _, e := range entries {
			if e.Name == SampleFormulaName {
				found = true
				if e.Type != SampleFormulaType || e.Description != SampleFormulaDescription || e.Source == "" {
					t.Errorf("list entry = %+v, want the sample's type, description and source", e)
				}
			}
		}
		if !found {
			t.Errorf("FormulaList = %q, want the sample formula in it", out)
		}
	})

	t.Run("list text is prose naming the sample", func(t *testing.T) {
		out, err := newVerbs(t).FormulaListText()
		if err != nil {
			t.Fatalf("FormulaListText: %v", err)
		}
		if !strings.Contains(string(out), SampleFormulaName) {
			t.Errorf("FormulaListText = %q, want the sample formula named", out)
		}
		if strings.Contains(string(out), `"schema_version"`) {
			t.Errorf("FormulaListText = %q, want prose, not the machine envelope", out)
		}
	})

	t.Run("show answers the sample", func(t *testing.T) {
		v := newVerbs(t)
		out, err := v.FormulaShow(SampleFormulaName)
		if err != nil {
			t.Fatalf("FormulaShow(%q): %v", SampleFormulaName, err)
		}
		var payload struct {
			Formula     string `json:"formula"`
			Type        string `json:"type"`
			Description string `json:"description"`
		}
		if err := json.Unmarshal(out, &payload); err != nil {
			t.Fatalf("FormulaShow = %q, want bd's JSON payload: %v", out, err)
		}
		if payload.Formula != SampleFormulaName || payload.Type != SampleFormulaType || payload.Description != SampleFormulaDescription {
			t.Errorf("FormulaShow = %+v, want the sample's name, type and description", payload)
		}

		text, err := v.FormulaShowText(SampleFormulaName)
		if err != nil {
			t.Fatalf("FormulaShowText(%q): %v", SampleFormulaName, err)
		}
		if !strings.Contains(string(text), SampleFormulaName) {
			t.Errorf("FormulaShowText = %q, want the sample formula named", text)
		}
	})

	t.Run("an unknown formula errors", func(t *testing.T) {
		const missing = "gt-no-such-formula"
		v := newVerbs(t)
		if out, err := v.FormulaShow(missing); err == nil {
			t.Errorf("FormulaShow(%q) = %q, want an error", missing, out)
		}
		if out, err := v.FormulaShowText(missing); err == nil {
			t.Errorf("FormulaShowText(%q) = %q, want an error", missing, out)
		}
	})
}
