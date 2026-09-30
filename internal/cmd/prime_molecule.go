package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/formula"
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
func showMoleculeExecutionPrompt(workDir, moleculeID string) {
	// Call bd mol current with JSON output
	cmd := beads.CommandWithEnv(workDir, nil, "mol", "current", moleculeID, "--json")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Fall back to simple message if bd mol current fails
		fmt.Println(style.Bold.Render("→ PROPULSION PRINCIPLE: Work is on your hook. RUN IT."))
		fmt.Println("  Begin working on this molecule immediately.")
		fmt.Printf("  Check status with: bd mol current %s\n", moleculeID)
		return
	}
	// Handle bd exit 0 bug: empty stdout means not found
	if stdout.Len() == 0 {
		fmt.Println(style.Bold.Render("→ PROPULSION PRINCIPLE: Work is on your hook. RUN IT."))
		fmt.Println("  Begin working on this molecule immediately.")
		return
	}

	// Parse JSON output - it's an array with one element
	var outputs []MoleculeCurrentOutput
	if err := json.Unmarshal(stdout.Bytes(), &outputs); err != nil || len(outputs) == 0 {
		// Fall back to simple message
		fmt.Println(style.Bold.Render("→ PROPULSION PRINCIPLE: Work is on your hook. RUN IT."))
		fmt.Println("  Begin working on this molecule immediately.")
		return
	}
	output := outputs[0]

	// Show molecule progress
	fmt.Printf("**Progress:** %d/%d steps complete\n\n",
		output.Completed, output.Total)

	// Show current step if available
	if output.NextStep != nil {
		step := output.NextStep
		fmt.Printf("%s\n\n", style.Bold.Render("## 🎬 CURRENT STEP: "+step.Title))
		fmt.Printf("**Step ID:** %s\n", step.ID)
		fmt.Printf("**Status:** %s (ready to execute)\n\n", step.Status)

		// Show step description if available
		if step.Description != "" {
			fmt.Println("### Instructions")
			fmt.Println()
			// Indent the description for readability
			lines := strings.Split(step.Description, "\n")
			for _, line := range lines {
				fmt.Printf("%s\n", line)
			}
			fmt.Println()
		}

		// The propulsion directive
		fmt.Println(style.Bold.Render("→ EXECUTE THIS STEP NOW."))
		fmt.Println()
		fmt.Println("When complete:")
		fmt.Printf("  1. Close the step: bd close %s\n", step.ID)
		fmt.Printf("  2. Check for next step: bd mol current %s\n", moleculeID)
		fmt.Println("  3. Continue until molecule complete")
	} else {
		// No next step - molecule may be complete
		fmt.Println(style.Bold.Render("✓ MOLECULE COMPLETE"))
		fmt.Println()
		fmt.Println("All steps are done. You may:")
		fmt.Println("  - Report completion to supervisor")
		fmt.Println("  - Check for new work: bd mol current")
	}
}

// showFormulaSteps renders the formula steps inline in the prime output.
// Agents read these steps instead of materializing them as wisp rows.
// The label parameter customizes the section header (e.g., "Patrol Steps", "Work Steps").
// townRoot and rigName are used to load formula overlays (operator customizations).
// extraVars is an optional list of "key=value" overrides that are substituted into
// step descriptions before rendering, taking precedence over formula defaults.
func showFormulaSteps(formulaName, label, townRoot, rigName string, extraVars ...[]string) {
	f, varMap, err := resolveFormulaForRendering(formulaName, townRoot, rigName, firstFormulaVars(extraVars))
	if err != nil {
		style.PrintWarning("%v", err)
		return
	}

	if len(f.Steps) == 0 {
		return
	}

	fmt.Println()
	fmt.Printf("**%s** (%d steps from %s):\n", label, len(f.Steps), formulaName)
	for i, step := range f.Steps {
		desc := applyFormulaVars(step.Description, varMap)
		fmt.Printf("  %d. **%s** — %s\n", i+1, step.Title, truncateDescription(desc, 120))
	}
	fmt.Println()
}

// showFormulaStepsFull renders the bounded formula checklist (every title, the
// body of step 1, and how to fetch the rest). Used for polecat work formulas and
// patrol formulas. The full-body renderer renderFormulaStepsFull remains for the
// Ralph loop directive, whose /ralph-loop prompt must carry every step inline;
// Ralph-mode attachments therefore still exceed the hook budget (rare, known).
// townRoot and rigName are used to load formula overlays (operator customizations).
// extraVars is an optional list of "key=value" overrides substituted into step descriptions.
func showFormulaStepsFull(formulaName, townRoot, rigName string, extraVars ...[]string) {
	f, varMap, err := resolveFormulaForRendering(formulaName, townRoot, rigName, firstFormulaVars(extraVars))
	if err != nil {
		style.PrintWarning("%v", err)
		return
	}
	fmt.Print(renderFormulaChecklist(formulaName, f, varMap, 1))
}

func renderFormulaStepsFull(formulaName, townRoot, rigName string, extraVars ...[]string) (string, error) {
	f, varMap, err := resolveFormulaForRendering(formulaName, townRoot, rigName, firstFormulaVars(extraVars))
	if err != nil {
		return "", err
	}
	return renderFormulaStepsFullParsed(formulaName, f, varMap), nil
}

func renderFormulaRootAndStepsFull(formulaName, townRoot, rigName string, extraVars ...[]string) (string, error) {
	f, varMap, err := resolveFormulaForRendering(formulaName, townRoot, rigName, firstFormulaVars(extraVars))
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	if desc := strings.TrimSpace(applyFormulaVars(f.Description, varMap)); desc != "" {
		sb.WriteString(desc)
		sb.WriteString("\n\n")
	}
	sb.WriteString(strings.TrimLeft(renderFormulaStepsFullParsed(formulaName, f, varMap), "\n"))
	return strings.TrimSpace(sb.String()), nil
}

func resolveFormulaForRendering(formulaName, townRoot, rigName string, vars []string) (*formula.Formula, map[string]string, error) {
	_, f, err := loadResolvedFormula(formulaName, townRoot, rigName)
	if err != nil {
		return nil, nil, err
	}
	applyFormulaOverlays(f, formulaName, townRoot, rigName)
	return f, buildFormulaVarMap(f, vars), nil
}

// loadResolvedFormula loads formulaName through the three tiers (rig > town >
// embedded) and returns it as written (raw) and resolved. A formula that
// extends another or expands a step carries only its delta; resolved lists
// every step the agent must run. Without extends or compose the two are the
// same formula.
func loadResolvedFormula(formulaName, townRoot, rigName string) (raw, resolved *formula.Formula, err error) {
	content, err := formula.ResolveFormulaContent(formulaName, townRoot, rigName)
	if err != nil {
		return nil, nil, fmt.Errorf("could not load formula %s: %w", formulaName, err)
	}

	raw, err = formula.Parse(content)
	if err != nil {
		return nil, nil, fmt.Errorf("could not parse formula %s: %w", formulaName, err)
	}
	if !formulaComposes(raw) {
		return raw, raw, nil
	}
	resolved, err = formula.Resolve(raw, formulaSearchPaths(townRoot, rigName))
	if err != nil {
		return nil, nil, fmt.Errorf("could not resolve formula %s: %w", formulaName, err)
	}
	return raw, resolved, nil
}

// formulaComposes reports whether f extends another formula or expands a
// step, i.e. whether f as written is only a delta.
func formulaComposes(f *formula.Formula) bool {
	return len(f.Extends) > 0 || f.Compose != nil
}

func firstFormulaVars(extraVars [][]string) []string {
	if len(extraVars) == 0 {
		return nil
	}
	return extraVars[0]
}

func renderFormulaStepsFullParsed(formulaName string, f *formula.Formula, varMap map[string]string) string {
	if len(f.Steps) == 0 {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "**Formula Checklist** (%d steps from %s):\n\n", len(f.Steps), formulaName)
	for i, step := range f.Steps {
		title := applyFormulaVars(step.Title, varMap)
		fmt.Fprintf(&sb, "### Step %d: %s\n\n", i+1, title)
		if step.Description != "" {
			sb.WriteString(applyFormulaVars(step.Description, varMap))
			sb.WriteString("\n\n")
		}
	}
	return sb.String()
}

// buildFormulaVarMap builds a map of variable name → value for substitution.
// Formula defaults are applied first; extraVars (key=value strings) override them.
func buildFormulaVarMap(f *formula.Formula, extraVars []string) map[string]string {
	m := make(map[string]string, len(f.Vars))
	for k, v := range f.Vars {
		if v.Default != "" || !v.Required {
			m[k] = v.Default
		}
	}
	for _, kv := range extraVars {
		if idx := strings.IndexByte(kv, '='); idx > 0 {
			m[kv[:idx]] = kv[idx+1:]
		}
	}
	return m
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

// applyFormulaVars replaces {{key}} placeholders in text with values from varMap.
func applyFormulaVars(text string, varMap map[string]string) string {
	for k, v := range varMap {
		text = strings.ReplaceAll(text, "{{"+k+"}}", v)
	}
	return text
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

// truncateDescription truncates a multi-line description to a single line summary.
func truncateDescription(desc string, maxLen int) string {
	// Take just the first line
	if idx := strings.IndexByte(desc, '\n'); idx >= 0 {
		desc = desc[:idx]
	}
	desc = strings.TrimSpace(desc)
	if len(desc) > maxLen {
		desc = desc[:maxLen-3] + "..."
	}
	if desc == "" {
		desc = "(no description)"
	}
	return desc
}

// applyFormulaOverlays loads and applies overlays to a parsed formula.
// It emits warnings for stale step IDs and, in --explain mode, shows which overlays are active.
func applyFormulaOverlays(f *formula.Formula, formulaName, townRoot, rigName string) {
	if townRoot == "" {
		return
	}

	overlay, err := formula.LoadFormulaOverlay(formulaName, townRoot, rigName)
	if err != nil {
		style.PrintWarning("could not load overlay for %s: %v", formulaName, err)
		return
	}
	if overlay == nil {
		explain(true, fmt.Sprintf("Formula overlay: no overlay found for %s", formulaName))
		return
	}

	explain(true, fmt.Sprintf("Formula overlay: applying %d override(s) for %s (rig=%s)", len(overlay.StepOverrides), formulaName, rigName))
	for _, so := range overlay.StepOverrides {
		explain(true, fmt.Sprintf("  overlay: step_id=%s mode=%s", so.StepID, so.Mode))
	}

	warnings := formula.ApplyOverlays(f, overlay)
	for _, w := range warnings {
		style.PrintWarning("formula overlay: %s", w)
	}
}
