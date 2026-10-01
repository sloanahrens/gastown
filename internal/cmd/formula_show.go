package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/steveyegge/gastown/internal/formula"
	"github.com/steveyegge/gastown/internal/workspace"
)

// formulaShowScope returns the town and rig the cwd sits in, so gt formula
// show finds the same rig > town > embedded copy gt prime renders. Either may
// be empty; an empty town leaves only the embedded set.
func formulaShowScope() (townRoot, rigName string) {
	townRoot, err := workspace.FindFromCwd()
	if err != nil || townRoot == "" {
		return "", ""
	}
	rigName, _ = inferRigFromCwd(townRoot)
	return townRoot, rigName
}

// renderResolvedFormula writes the resolved formula in bd formula show's
// layout, naming what it was resolved from so the reader knows the steps are
// not all in the file.
func renderResolvedFormula(w io.Writer, raw, resolved *formula.Formula) {
	fmt.Fprintf(w, "%s (resolved)\n", resolved.Name)
	fmt.Fprintf(w, "   Type: %s\n", resolved.Type)
	if desc := strings.TrimSpace(resolved.Description); desc != "" {
		fmt.Fprintf(w, "   Description: %s\n", desc)
	}
	if len(raw.Extends) > 0 {
		fmt.Fprintf(w, "   Extends: %s\n", strings.Join(raw.Extends, ", "))
	}
	if raw.Compose != nil {
		for _, e := range raw.Compose.Expand {
			fmt.Fprintf(w, "   Expands: %s with %s\n", e.Target, e.With)
		}
	}
	fmt.Fprintf(w, "   Raw view: gt formula show %s --raw\n", resolved.Name)

	if len(resolved.Vars) > 0 {
		fmt.Fprintf(w, "\nVariables:\n")
		for _, name := range slices.Sorted(maps.Keys(resolved.Vars)) {
			v := resolved.Vars[name]
			attr := fmt.Sprintf("[default=%q]", v.Default)
			if v.Required {
				attr = "[required]"
			}
			fmt.Fprintf(w, "   {{%s}}: %s %s\n", name, v.Description, attr)
		}
	}

	fmt.Fprintf(w, "\nSteps (%d):\n", len(resolved.Steps))
	for i, s := range resolved.Steps {
		branch := "├──"
		if i == len(resolved.Steps)-1 {
			branch = "└──"
		}
		line := fmt.Sprintf("   %s %s: %s", branch, s.ID, s.Title)
		if len(s.Needs) > 0 {
			line += fmt.Sprintf(" [needs: %s]", strings.Join(s.Needs, ", "))
		}
		fmt.Fprintln(w, line)
	}
}

// resolvedFormulaJSON mirrors the keys of bd formula show --json, plus
// resolved, so a consumer can tell the steps include inherited ones.
type resolvedFormulaJSON struct {
	Formula     string                    `json:"formula"`
	Type        formula.FormulaType       `json:"type"`
	Description string                    `json:"description"`
	Version     int                       `json:"version"`
	Resolved    bool                      `json:"resolved"`
	Extends     []string                  `json:"extends,omitempty"`
	Compose     *resolvedComposeJSON      `json:"compose,omitempty"`
	Vars        map[string]formulaVarJSON `json:"vars,omitempty"`
	Steps       []formulaStepJSON         `json:"steps"`
}

// resolvedComposeJSON records the expansions that were applied; the steps
// already include them.
type resolvedComposeJSON struct {
	Expand []resolvedExpandJSON `json:"expand"`
}

type resolvedExpandJSON struct {
	Target string `json:"target"`
	With   string `json:"with"`
}

type formulaVarJSON struct {
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Default     string `json:"default"`
}

type formulaStepJSON struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Needs       []string `json:"needs,omitempty"`
}

// writeResolvedFormulaJSON writes the resolved formula as JSON.
func writeResolvedFormulaJSON(w io.Writer, raw, resolved *formula.Formula) error {
	out := resolvedFormulaJSON{
		Formula:     resolved.Name,
		Type:        resolved.Type,
		Description: resolved.Description,
		Version:     resolved.Version,
		Resolved:    true,
		Extends:     raw.Extends,
		Steps:       make([]formulaStepJSON, 0, len(resolved.Steps)),
	}
	if raw.Compose != nil {
		out.Compose = &resolvedComposeJSON{}
		for _, e := range raw.Compose.Expand {
			out.Compose.Expand = append(out.Compose.Expand, resolvedExpandJSON{Target: e.Target, With: e.With})
		}
	}
	if len(resolved.Vars) > 0 {
		out.Vars = make(map[string]formulaVarJSON, len(resolved.Vars))
		for name, v := range resolved.Vars {
			out.Vars[name] = formulaVarJSON{Description: v.Description, Required: v.Required, Default: v.Default}
		}
	}
	for _, s := range resolved.Steps {
		out.Steps = append(out.Steps, formulaStepJSON{ID: s.ID, Title: s.Title, Description: s.Description, Needs: s.Needs})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
