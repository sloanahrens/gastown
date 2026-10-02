package cmd

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
)

// renderFormulaStepsFull renders every step with its full body, for the
// Ralph loop directive, whose /ralph-loop prompt must carry every step inline;
// Ralph-mode attachments therefore still exceed the hook budget (rare, known).
func renderFormulaStepsFull(formulaName string, steps []checklistStep) string {
	if len(steps) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "**Formula Checklist** (%d steps from %s):\n\n", len(steps), formulaName)
	for i, step := range steps {
		fmt.Fprintf(&sb, "### Step %d: %s%s\n\n", i+1, step.Title, stepStatusSuffix(step))
		if step.Description != "" {
			sb.WriteString(step.Description)
			sb.WriteString("\n\n")
		}
	}
	return sb.String()
}

func attachmentFormulaVars(attachment *beads.AttachmentFields) []string {
	if attachment == nil {
		return nil
	}
	indexes := make(map[string]int)
	vars := make([]string, 0, len(attachment.AttachedVars))
	add := func(variable string) {
		variable = strings.TrimSpace(variable)
		if variable == "" {
			return
		}
		idx := strings.IndexByte(variable, '=')
		if idx <= 0 {
			return
		}
		key := strings.TrimSpace(variable[:idx])
		if idx, ok := indexes[key]; ok {
			vars[idx] = variable
			return
		}
		indexes[key] = len(vars)
		vars = append(vars, variable)
	}
	for _, variable := range strings.Split(attachment.FormulaVars, "\n") {
		add(variable)
	}
	for _, variable := range attachment.AttachedVars {
		add(variable)
	}
	return vars
}
