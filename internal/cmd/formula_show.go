package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/steveyegge/gastown/internal/workspace"
)

// formulaShowScope returns the town and rig the cwd sits in, so gt formula
// show cooks the formula where gt prime does. Either may be empty.
func formulaShowScope() (townRoot, rigName string) {
	townRoot, err := workspace.FindFromCwd()
	if err != nil || townRoot == "" {
		return "", ""
	}
	rigName, _ = inferRigFromCwd(townRoot)
	return townRoot, rigName
}

// renderCookedFormula writes the cooked formula in bd formula show's layout,
// saying the steps are cooked so the reader knows they are not all in the file.
func renderCookedFormula(w io.Writer, f *cookedFormula) {
	fmt.Fprintf(w, "%s (cooked)\n", f.Formula)
	fmt.Fprintf(w, "   Type: %s\n", f.Type)
	if desc := strings.TrimSpace(f.Description); desc != "" {
		fmt.Fprintf(w, "   Description: %s\n", desc)
	}
	fmt.Fprintf(w, "   Raw view: gt formula show %s --raw\n", f.Formula)

	if len(f.Vars) > 0 {
		fmt.Fprintf(w, "\nVariables:\n")
		for _, v := range f.Vars {
			attr := "[no default]"
			switch {
			case v.Required:
				attr = "[required]"
			case v.Default != nil:
				attr = fmt.Sprintf("[default=%q]", *v.Default)
			}
			fmt.Fprintf(w, "   {{%s}}: %s %s\n", v.Name, v.Description, attr)
		}
	}

	fmt.Fprintf(w, "\nSteps (%d):\n", len(f.checklist()))
	renderCookedSteps(w, f.Steps, "   ")
}

func renderCookedSteps(w io.Writer, steps []cookedStep, indent string) {
	for i, s := range steps {
		branch, childIndent := "├──", indent+"│   "
		if i == len(steps)-1 {
			branch, childIndent = "└──", indent+"    "
		}
		line := fmt.Sprintf("%s%s %s: %s", indent, branch, s.ID, s.Title)
		if len(s.Needs) > 0 {
			line += fmt.Sprintf(" [needs: %s]", strings.Join(s.Needs, ", "))
		}
		fmt.Fprintln(w, line)
		renderCookedSteps(w, s.Children, childIndent)
	}
}

// writeCookedFormulaJSON writes bd's cooked tree as bd printed it, indented.
func writeCookedFormulaJSON(w io.Writer, f *cookedFormula) error {
	var out bytes.Buffer
	if err := json.Indent(&out, f.raw, "", "  "); err != nil {
		return fmt.Errorf("formula %s: %w", f.Formula, err)
	}
	out.WriteByte('\n')
	_, err := w.Write(out.Bytes())
	return err
}
