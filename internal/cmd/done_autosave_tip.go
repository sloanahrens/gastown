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
