package cmd

import (
	"fmt"
	"time"

	"github.com/steveyegge/gastown/internal/crewfresh"
)

// primeFreshTimeout bounds the one network call prime's crew reminder makes,
// so an unreachable remote delays a session's start by no more than this: a
// fetch that outlives it is reported as failed (gt-5dh3y).
const primeFreshTimeout = 3 * time.Second

// primeFreshFailedLine is what prime prints in place of the reminder when the
// check could not run. A failed check must not read as an up-to-date clone:
// that silence would trust a reading that was never taken (gt-5dh3y).
const primeFreshFailedLine = "gt: could not check whether this crew clone is behind origin/main; run `gt crew fresh`"

// primeFreshCheck is what the read-only freshness check found for one clone.
type primeFreshCheck struct {
	// Behind counts the commits origin/main has that the clone's HEAD lacks,
	// as of the fetch that precedes it.
	Behind int
}

// primeFreshRunner asks one crew clone's git the freshness questions. It is a
// parameter of the freshness functions so a test drives them without git.
type primeFreshRunner func(root string) (primeFreshCheck, error)

// primeFreshRun is the runner prime passes: the real check against the clone
// that holds the session.
func primeFreshRun(root string) (primeFreshCheck, error) {
	return primeFreshCheckWith(crewfresh.NewGit(root))
}

// primeFreshCheckWith asks g the two questions a reminder needs — how far
// origin/main leads HEAD, and nothing more — and returns the failed check as
// an error for the caller to turn into a notice.
//
// It never calls FastForwardMain: prime runs at a session's start, and moving
// the clone's main there would land a ref change the session did not ask for.
// The fetch updates origin/main, which every freshness check must do; no ref
// the clone's own work tracks moves (gt-5dh3y).
func primeFreshCheckWith(g crewfresh.Git) (primeFreshCheck, error) {
	if err := g.Fetch(primeFreshTimeout); err != nil {
		return primeFreshCheck{}, fmt.Errorf("fetch origin: %w", err)
	}
	behind, err := g.Behind()
	if err != nil {
		return primeFreshCheck{}, fmt.Errorf("count the commits behind origin/main: %w", err)
	}
	return primeFreshCheck{Behind: behind}, nil
}

// primeFreshnessAtStart returns the line a session started by prime prints and
// whether the check could not be completed.
//
// A compact resume is not a fresh start: that fast path must stay off the
// network and inside prime's sub-second budget, so it never runs the check.
// --dry-run reports what a prime would do without doing it, so it never
// fetches either (gt-5dh3y).
func primeFreshnessAtStart(ctx RoleContext, compactResume, dryRun bool, run primeFreshRunner) (line string, failed bool) {
	if compactResume || dryRun {
		return "", false
	}
	return primeFreshnessLine(ctx, run)
}

// primeFreshnessLine returns the reminder line for the clone that holds the
// session, and "" when the clone is current or the reminder does not apply.
// The second return is true only when the check could not run, because an
// empty line alone would read as an up-to-date clone; a failed check is not a
// reason for prime to fail, so the runner's error stops here (gt-5dh3y).
func primeFreshnessLine(ctx RoleContext, run primeFreshRunner) (line string, failed bool) {
	if ctx.Role != RoleCrew {
		return "", false
	}
	root, err := crewCloneRoot(ctx.WorkDir, ctx.TownRoot)
	if err != nil {
		return "", false
	}
	check, err := run(root)
	if err != nil {
		return "", true
	}
	if check.Behind <= 0 {
		return "", false
	}
	return primeFreshBehindLine(check.Behind), false
}

// primeFreshBehindLine is the reminder for a clone that lags origin/main.
func primeFreshBehindLine(behind int) string {
	commits := "commits"
	if behind == 1 {
		commits = "commit"
	}
	return fmt.Sprintf("gt: this crew clone is %d %s behind origin/main; run `gt crew fresh` to catch up", behind, commits)
}
