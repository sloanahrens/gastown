package cmd

import (
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/lintlock"
)

// This file is gt done's side of golangci-lint's module-wide lock: what a failed
// lint tells the polecat. The policy it rests on — when to wait for the lock,
// how long, and how to tell a contended lint from one that found something —
// lives in internal/lintlock, shared with the refinery's gates so the two
// callers cannot describe the same failure differently (gt-ijqw).
//
// A contended lint is both real contention and a lie: the other lint is doing
// useful work and will release the lock, but nothing was analyzed. With a
// five-polecat cap plus the refinery's own batch lint, two lints overlapping is
// routine rather than exotic (gt-xsty).

// lintFailureDetail is what a failed lint attempt tells the polecat. Only a lint
// that ran to completion and reported findings may ask for findings to be
// fixed; the other outcomes linted nothing, and sending a polecat after output
// that does not exist is the misattribution this file exists to prevent
// (gt-xsty).
//
// budgetExpired is the case gt-taoz opened: a lint killed at the outer budget
// returns a signal error carrying no marker, so the default detail would claim
// findings for output that never existed. A lint still going at the deadline is
// one that took the lock and outran its reserve, or a lint command that waits
// on the lock internally and says nothing while it does (gt-kqwu).
func lintFailureDetail(outcome lintlock.Outcome, budgetExpired bool, budget time.Duration) string {
	switch {
	case outcome.Contended:
		return fmt.Sprintf("another golangci-lint held the lock %s (a concurrent gt done, or the refinery's batch lint) — nothing was linted and no finding is reported; re-run gt done once the other lint finishes", outcome.LockWait())
	case outcome.Unfinished:
		return "golangci-lint stopped without reporting findings — nothing was linted and no finding is reported; a concurrent golangci-lint holding the module lock is the likeliest reason it never finished, so re-run gt done once other lints have"
	case budgetExpired:
		return fmt.Sprintf("the lint was killed at its %s budget without finishing — nothing was linted and no finding is reported; a concurrent golangci-lint holding the module lock is the likeliest reason it never finished, so re-run gt done once other lints have", humanDuration(budget))
	default:
		return "fix the lint findings before resubmitting"
	}
}
