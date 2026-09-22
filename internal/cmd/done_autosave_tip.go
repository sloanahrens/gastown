package cmd

import (
	"fmt"

	"github.com/steveyegge/gastown/internal/checkpoint"
)

// autoSaveTipFixCommand is the rewrite that folds a branch's leading run of
// machine-generated commits into the commit beneath it, or — when there is no
// commit to inherit a message from, because the run is the whole branch or the
// commit beneath is a merge — replaces it with a real message. The polecat runs
// it by hand because gt done may not rewrite a branch that origin already has.
func autoSaveTipFixCommand(tip checkpoint.AutoSaveTip) string {
	if tip.Trailing >= tip.Ahead || tip.BeneathIsMerge {
		return fmt.Sprintf(`git reset --soft HEAD~%d && git commit -m "<what the work does>"`, tip.Trailing)
	}
	return fmt.Sprintf("git reset --soft HEAD~%d && git commit --amend --no-edit", tip.Trailing)
}

// autoSaveTipGate refuses a branch whose tip is a machine-generated commit and
// that gt done cannot rewrite, because origin already has it. It returns nil
// when the branch may be submitted — either because the tip is real work, or
// because the squash step below folds it away first (gt-iki6).
func autoSaveTipGate(tip checkpoint.AutoSaveTip, branch, pushedReason string) error {
	if !tip.AutoSave || pushedReason == "" {
		return nil
	}
	return autoSaveTipRefusalError(tip, branch,
		fmt.Sprintf("origin already has this branch (%s), so squashing it here would need a force-push.", pushedReason))
}

// autoSaveTipRefusalError refuses a branch whose tip is a machine-generated
// commit, naming why the squash could not clear it and the command that does
// (gt-iki6). Quietly submitting such a branch is what put f0a00f6 — a
// "WIP: checkpoint (auto)" subject — at the tip of main.
func autoSaveTipRefusalError(tip checkpoint.AutoSaveTip, branch, why string) error {
	return fmt.Errorf("refusing to submit branch %s: its tip is a machine-generated commit %q\n"+
		"%s\n"+
		"Fold the tip's %d machine-generated commit(s) into the commit beneath them, "+
		"then re-run gt done:\n\n"+
		"  %s\n",
		branch, tip.Subject, why, tip.Trailing, autoSaveTipFixCommand(tip))
}

// autoSaveTipUninspectableError refuses a branch whose tip could not be read at
// all. The check must not fail open: this runs only when origin already has the
// branch, so an unread tip could be a machine-generated one that gt done then
// could not rewrite (gt-iki6).
func autoSaveTipUninspectableError(branch, pushedReason string, cause error) error {
	return fmt.Errorf("refusing to submit branch %s: could not read its tip commit (%v), and origin already has the branch (%s), so a machine-generated tip could not be rewritten.\n"+
		"Check the branch, fold any machine-generated tip into the commit beneath it, then re-run gt done:\n\n"+
		"  git log --oneline\n",
		branch, cause, pushedReason)
}

// autoSaveSquashResetError names the state a squash that failed partway leaves
// behind: the soft reset to the branch base already landed, so the branch holds
// its changes staged and uncommitted and only the commit is missing. Submitting
// that state would push a branch stripped of its commits (gt-iki6).
func autoSaveSquashResetError(branch, baseRef string, cause error) error {
	return fmt.Errorf("refusing to submit branch %s: squashing its machine-generated commits reset it to %s and re-committing failed: %v\n"+
		"The branch's changes are staged on top of %s and no commit holds them. Commit them, then re-run gt done:\n\n"+
		"  git commit -m \"<what the work does>\"\n",
		branch, baseRef, cause, baseRef)
}
