package cmd

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/style"
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
