package cmd

import (
	"time"

	"github.com/steveyegge/gastown/internal/crewfresh"
)

// primeFreshTimeout bounds the one network call prime's crew reminder makes,
// so an unreachable remote delays a session's start by no more than this: a
// fetch that outlives it is reported as failed.
const primeFreshTimeout = 3 * time.Second

// primeFreshRunner asks one crew clone's git the freshness questions. It is a
// parameter of the freshness functions so a test drives them without git.
type primeFreshRunner func(root string) (crewfresh.Report, error)

// primeFreshRun is the runner prime passes: the real one, against the clone
// that holds the session.
var primeFreshRun primeFreshRunner = func(root string) (crewfresh.Report, error) {
	return crewfresh.Run(crewfresh.NewGit(root), primeFreshTimeout)
}

// primeFreshnessAtStart returns the freshness line a session started by prime
// prints, and "" when there is nothing to say.
//
// A compact resume is not a fresh start: that fast path must stay off the
// network and inside prime's sub-second budget, so it never runs the check
// (gt-5dh3y).
func primeFreshnessAtStart(ctx RoleContext, compactResume bool, run primeFreshRunner) string {
	if compactResume {
		return ""
	}
	return primeFreshnessLine(ctx, run)
}

// primeFreshnessLine runs the crew freshness check for the clone that holds
// the session and returns its one line when that clone is behind origin/main.
//
// Only a crew session is reminded: any other role, a directory crewCloneRoot
// refuses, and a check that cannot be run all give "". A check that fails is
// not a reason for prime to fail, so the runner's error stops at this
// function: returning it would turn an unreachable remote into a session that
// cannot start (gt-5dh3y).
func primeFreshnessLine(ctx RoleContext, run primeFreshRunner) string {
	if ctx.Role != RoleCrew {
		return ""
	}
	root, err := crewCloneRoot(ctx.WorkDir, ctx.TownRoot)
	if err != nil {
		return ""
	}
	report, err := run(root)
	if err != nil || report.Behind <= 0 {
		return ""
	}
	return report.Message
}
