package cmd

import (
	"fmt"
	"io"
)

// staleBaseThreshold is how many commits origin/main may carry that the
// branch does not before gt done warns. Below it the rebase is cheap enough
// that the warning would be noise on every submission.
const staleBaseThreshold = 25

// staleBaseGit is git's surface for the stale-base check: the one read it
// makes. *git.Git in production, a recorder in a test.
type staleBaseGit interface {
	CountCommitsBehind(ref string) (int, error)
}

// staleBaseWarning returns the line gt done prints for a branch behind
// origin/main by behind commits, or "" when behind is at or under threshold.
// Pure so the boundary and the wording are tested without git.
func staleBaseWarning(behind, threshold int) string {
	if behind <= threshold {
		return ""
	}
	return fmt.Sprintf(
		"Warning: your branch base is %d commits behind origin/main; run `git rebase origin/main` before the review",
		behind,
	)
}

// warnStaleBase writes the stale-base warning for g to w. A count that fails
// prints nothing: the warning is advisory and never stops a submission.
func warnStaleBase(w io.Writer, g staleBaseGit) {
	behind, err := g.CountCommitsBehind("origin/main")
	if err != nil {
		return
	}
	if msg := staleBaseWarning(behind, staleBaseThreshold); msg != "" {
		fmt.Fprintln(w, msg)
	}
}

// submitWithStaleBaseWarning prints the stale-base warning, then returns
// submit's value unchanged: the warning cannot alter the exit status.
func submitWithStaleBaseWarning(w io.Writer, g staleBaseGit, submit func() error) error {
	warnStaleBase(w, g)
	return submit()
}
