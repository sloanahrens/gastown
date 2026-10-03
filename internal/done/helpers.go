package done

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/style"
)

// requireRealCurrentBranch refuses a branch value of "HEAD" — what
// git.CurrentBranch returns in a detached worktree. gt done records its
// caller's CurrentBranch() verbatim as the branch it submits, so a detached
// capture would declare a branch that does not exist and lose the real work
// branch. command names the CLI invocation in the error so the operator knows
// which one to re-run after checking out a real branch.
func requireRealCurrentBranch(branch, command string) error {
	if strings.TrimSpace(branch) != "HEAD" {
		return nil
	}
	return fmt.Errorf("cannot determine current branch: worktree is in detached HEAD state — check out your work branch before running %s", command)
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

// resolveMRTarget refuses a self-targeted submission: a resolved target equal
// to the branch being submitted merges as a no-op and the post-merge cleanup
// deletes the only copy of the work (gt-a8i3).
//
// Falls back to defaultBranch when target == branch. Returns an error only
// if the fallback ALSO equals branch (defaultBranch itself is somehow the
// branch being submitted), since there is then no safe target to fall back
// to and the caller must be told explicitly rather than guess.
//
// Also refuses a resolved target that is a DIFFERENT polecat's branch
// (polecat/*) unless explicit is true. explicit is true only when the
// caller declared target itself, via --target — never when it came from
// formula_vars base_branch. That path is exactly how gt-a8i3's self-target
// leak happened (a stray base_branch formula var); a polecat/* target
// reaching here through it is the same class of leak, just pointed at
// someone else's in-flight branch instead of the submitter's own, so it gets
// the same refusal rather than silently merging into another polecat's
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
		return "", fmt.Errorf("cannot submit MR: resolved target %q is another polecat's branch; pass --target explicitly if merging into it is intentional", target)
	}
	return target, nil
}

// ExitCodeError is a gt done failure that ends the process with Code. Its
// errors carry the submission failure the polecat must fix. The command
// layer's exitCodeForError maps Code onto the process exit status, since this
// package cannot import internal/cmd to map it here.
type ExitCodeError struct {
	Code int
	Err  error
}

func (e *ExitCodeError) Error() string { return e.Err.Error() }

func (e *ExitCodeError) Unwrap() error { return e.Err }
