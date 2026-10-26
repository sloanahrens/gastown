package beadsfake

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

var _ beads.FormulaVerbs = (*Fake)(nil)

// fakeFormula is one formula the fake holds: the fields of bd's formula list
// JSON that a caller can observe without the engine.
type fakeFormula struct {
	name        string
	kind        string
	description string
}

// SeedFormula registers a formula FormulaList, FormulaShow and
// FormulaShowText answer for. kind is bd's formula type; empty means "task".
func (f *Fake) SeedFormula(name, kind, description string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if kind == "" {
		kind = "task"
	}
	f.formulas = append(f.formulas, fakeFormula{name: name, kind: kind, description: description})
}

// formula returns the registered formula called name. Callers hold f.mu.
func (f *Fake) formula(name string) (fakeFormula, bool) {
	for _, fm := range f.formulas {
		if fm.name == name {
			return fm, true
		}
	}
	return fakeFormula{}, false
}

// FormulaList returns the registered formulas as bd's formula list --json
// payload does, and "null" when there are none, as bd prints it.
func (f *Fake) FormulaList() ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("formula list"); err != nil {
		return nil, err
	}
	if len(f.formulas) == 0 {
		return []byte("null\n"), nil
	}
	type entry struct {
		Name        string `json:"name"`
		Type        string `json:"type"`
		Description string `json:"description,omitempty"`
		Source      string `json:"source"`
		Steps       int    `json:"steps"`
		Vars        int    `json:"vars"`
	}
	entries := make([]entry, 0, len(f.formulas))
	for _, fm := range f.formulas {
		entries = append(entries, entry{
			Name: fm.name, Type: fm.kind, Description: fm.description,
			Source: "fake", // a formula file bd would name; the fake has none
		})
	}
	out, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// FormulaListText returns the registered formulas as bd's formula list prose
// does for a person's terminal.
func (f *Fake) FormulaListText() ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("formula list"); err != nil {
		return nil, err
	}
	if len(f.formulas) == 0 {
		return []byte("No formulas found.\n"), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "📜 Formulas (%d found)\n", len(f.formulas))
	for _, fm := range f.formulas {
		fmt.Fprintf(&b, "\n  %s  %s\n", fm.name, fm.description)
	}
	return []byte(b.String()), nil
}

// FormulaShow returns name's formula as bd's formula show payload, which
// names the formula; a name no formula has is an error.
func (f *Fake) FormulaShow(formula string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("formula show"); err != nil {
		return nil, err
	}
	fm, ok := f.formula(formula)
	if !ok {
		return nil, fmt.Errorf("formula not found: %q", formula)
	}
	out, err := json.MarshalIndent(map[string]string{
		"formula": fm.name, "type": fm.kind, "description": fm.description,
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// FormulaShowText returns name's formula as bd's formula show prose does for
// a person's terminal; a name no formula has is an error.
func (f *Fake) FormulaShowText(formula string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("formula show"); err != nil {
		return nil, err
	}
	fm, ok := f.formula(formula)
	if !ok {
		return nil, fmt.Errorf("formula not found: %q", formula)
	}
	return []byte(fmt.Sprintf("📐 %s\n   Type: %s\n   Description: %s\n", fm.name, fm.kind, fm.description)), nil
}
