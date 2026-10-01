package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/style"
)

// MoleculeCurrentOutput represents the JSON output of bd mol current.
type MoleculeCurrentOutput struct {
	MoleculeID    string `json:"molecule_id"`
	MoleculeTitle string `json:"molecule_title"`
	NextStep      *struct {
		ID          string `json:"id"`
		Title       string `json:"title"`
		Description string `json:"description"`
		Status      string `json:"status"`
	} `json:"next_step"`
	Completed int `json:"completed"`
	Total     int `json:"total"`
}

// showMoleculeExecutionPrompt calls bd mol current and shows the current step
// with execution instructions. This is the core of the Propulsion Principle.
func showMoleculeExecutionPrompt(w io.Writer, workDir, moleculeID string) {
	// Call bd mol current with JSON output
	cmd := beads.CommandWithEnv(workDir, nil, "mol", "current", moleculeID, "--json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Fall back to simple message if bd mol current fails
		fmt.Fprintln(w, style.Bold.Render("→ PROPULSION PRINCIPLE: Work is on your hook. RUN IT."))
		fmt.Fprintln(w, "  Begin working on this molecule immediately.")
		fmt.Fprintf(w, "  Check status with: bd mol current %s\n", moleculeID)
		return
	}
	// Handle bd exit 0 bug: empty stdout means not found
	if stdout.Len() == 0 {
		fmt.Fprintln(w, style.Bold.Render("→ PROPULSION PRINCIPLE: Work is on your hook. RUN IT."))
		fmt.Fprintln(w, "  Begin working on this molecule immediately.")
		return
	}

	// Parse JSON output - it's an array with one element
	var outputs []MoleculeCurrentOutput
	if err := json.Unmarshal(stdout.Bytes(), &outputs); err != nil || len(outputs) == 0 {
		// Fall back to simple message
		fmt.Fprintln(w, style.Bold.Render("→ PROPULSION PRINCIPLE: Work is on your hook. RUN IT."))
		fmt.Fprintln(w, "  Begin working on this molecule immediately.")
		return
	}
	output := outputs[0]

	// Show molecule progress
	fmt.Fprintf(w, "**Progress:** %d/%d steps complete\n\n",
		output.Completed, output.Total)

	// Show current step if available
	if output.NextStep != nil {
		step := output.NextStep
		fmt.Fprintf(w, "%s\n\n", style.Bold.Render("## 🎬 CURRENT STEP: "+step.Title))
		fmt.Fprintf(w, "**Step ID:** %s\n", step.ID)
		fmt.Fprintf(w, "**Status:** %s (ready to execute)\n\n", step.Status)

		// Show step description if available
		if step.Description != "" {
			fmt.Fprintln(w, "### Instructions")
			fmt.Fprintln(w)
			// Indent the description for readability
			lines := strings.Split(step.Description, "\n")
			for _, line := range lines {
				fmt.Fprintf(w, "%s\n", line)
			}
			fmt.Fprintln(w)
		}

		// The propulsion directive
		fmt.Fprintln(w, style.Bold.Render("→ EXECUTE THIS STEP NOW."))
		fmt.Fprintln(w)
		fmt.Fprintln(w, "When complete:")
		fmt.Fprintf(w, "  1. Close the step: bd close %s\n", step.ID)
		fmt.Fprintf(w, "  2. Check for next step: bd mol current %s\n", moleculeID)
		fmt.Fprintln(w, "  3. Continue until molecule complete")
	} else {
		// No next step - molecule may be complete
		fmt.Fprintln(w, style.Bold.Render("✓ MOLECULE COMPLETE"))
		fmt.Fprintln(w)
		fmt.Fprintln(w, "All steps are done. You may:")
		fmt.Fprintln(w, "  - Report completion to supervisor")
		fmt.Fprintln(w, "  - Check for new work: bd mol current")
	}
}

// showStepsFull renders the bounded formula checklist (every title, the
// body of step 1, and how to fetch the rest). Used for polecat work formulas and
// patrol formulas. The full-body renderer renderStepsFull remains for the
// Ralph loop directive, whose /ralph-loop prompt must carry every step inline;
// Ralph-mode attachments therefore still exceed the hook budget (rare, known).
// The steps are what bd cooks (formulaCooker.cookForRender); a cook failure is
// one warning line and no checklist. vars ("key=value") are passed to the cook.
func (c formulaCooker) showStepsFull(w io.Writer, formulaName, townRoot, rigName string, vars []string) {
	f, err := c.cookForRender(formulaName, townRoot, rigName, vars)
	if err != nil {
		style.PrintWarning("%v", err)
		return
	}
	_, _ = fmt.Fprint(w, renderFormulaChecklist(formulaName, f, 1))
}

func (c formulaCooker) renderStepsFull(formulaName, townRoot, rigName string, vars []string) (string, error) {
	f, err := c.cookForRender(formulaName, townRoot, rigName, vars)
	if err != nil {
		return "", err
	}
	return renderFormulaStepsFullCooked(formulaName, f), nil
}

func renderFormulaStepsFullCooked(formulaName string, f *cookedFormula) string {
	steps := f.checklist()
	if len(steps) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "**Formula Checklist** (%d steps from %s):\n\n", len(steps), formulaName)
	for i, step := range steps {
		fmt.Fprintf(&sb, "### Step %d: %s\n\n", i+1, step.Title)
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

// extractFormulaVar extracts a specific key's value from a newline-separated
// key=value string (as stored in AttachmentFields.FormulaVars).
// Returns "" if the key is not found.
func extractFormulaVar(formulaVars, key string) string {
	for _, line := range strings.Split(formulaVars, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && k == key {
			return v
		}
	}
	return ""
}

// resolveMRTarget refuses a self-targeted MR: a resolved target equal to the
// branch being submitted merges as a no-op and the post-merge cleanup
// deletes the only copy of the work (gt-a8i3). Used by both `gt done` and
// `gt mq submit`, which otherwise independently duplicate the same target
// resolution priority chain (--target/--epic > formula_vars base_branch >
// integration auto-detect > rig default).
//
// Falls back to defaultBranch when target == branch. Returns an error only
// if the fallback ALSO equals branch (defaultBranch itself is somehow the
// branch being submitted), since there is then no safe target to fall back
// to and the caller must be told explicitly rather than guess.
//
// Also refuses a resolved target that is a DIFFERENT polecat's branch
// (polecat/*) unless explicit is true. explicit is true only when the
// caller declared target itself, via --target (gt done) or --epic (gt mq
// submit) — never when it came from formula_vars base_branch or integration
// auto-detect. Those two paths are exactly how gt-a8i3's self-target leak
// happened (a stray base_branch formula var); a polecat/* target reaching
// here through them is the same class of leak, just pointed at someone
// else's in-flight branch instead of the submitter's own, so it gets the
// same refusal rather than silently merging into another polecat's
// unfinished work.
func resolveMRTarget(target, branch, defaultBranch string, explicit bool) (string, error) {
	if target == branch {
		style.PrintWarning("MR target %q equals the source branch; refusing self-target, falling back to rig default %q", target, defaultBranch)
		if defaultBranch == branch {
			return "", fmt.Errorf("cannot submit MR: resolved target %q equals the source branch and the rig default branch also equals the source branch; specify the target explicitly", target)
		}
		target = defaultBranch
	}
	if !explicit && strings.HasPrefix(target, "polecat/") {
		return "", fmt.Errorf("cannot submit MR: resolved target %q is another polecat's branch; pass --target (or --epic) explicitly if merging into it is intentional", target)
	}
	return target, nil
}
