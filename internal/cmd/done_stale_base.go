package cmd

import (
	"fmt"
	"io"
	"time"

	"github.com/steveyegge/gastown/internal/git"
)

// staleBaseThreshold is how many commits origin/main may carry that the
// branch does not before gt done warns. Below it the rebase is cheap enough
// that the warning would be noise on every submission.
const staleBaseThreshold = 25

// staleBaseGit is git's surface for the stale-base check: the bounded fetch
// that refreshes origin/main and the read it makes. *git.Git in production, a
// recorder in a test.
type staleBaseGit interface {
	FetchRefspecWithTimeout(remote, refspec string, timeout time.Duration) error
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
//
// origin/main is refreshed first, bounded like every read-only remote query:
// counted against the last fetch, the warning would understate a stale base —
// or miss one entirely — because done.Run fetches only after this runs. A
// fetch that fails leaves the existing ref in place, which is the best
// question the branch can still be asked.
func warnStaleBase(w io.Writer, g staleBaseGit) {
	_ = g.FetchRefspecWithTimeout("origin", "+refs/heads/main:refs/remotes/origin/main", git.RemoteQueryTimeout)
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
