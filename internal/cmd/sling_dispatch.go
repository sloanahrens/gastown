package cmd

import (
	"context"

	"github.com/steveyegge/gastown/internal/sling"
)

// SlingParams and SlingResult are the dispatch engine's request and outcome.
// They are aliases, not copies: the engine lives in internal/sling so the
// daemon's scheduled dispatch and the spec dispatcher reach the same code, and
// a second definition here would be a boundary no compiler checks.
type (
	SlingParams = sling.Options
	SlingResult = sling.Result
)

// executeSling performs the unified per-bead polecat/rig dispatch with the
// running gt's collaborators. Batch dispatch and queue dispatch call this
// function.
//
// Caller responsibilities (NOT handled here):
//   - Cross-rig guard: callers must call checkCrossRigGuard() before
//     executeSling to verify the bead's prefix matches the target rig. Batch
//     dispatch does this pre-loop; queue dispatch skips the guard because the
//     bead prefix was validated at enqueue time and is immutable.
//   - wakeRigAgents: callers must call wakeRigAgents() after the dispatch loop
//     when NoBoot is false. Batch dispatch calls it post-loop; queue dispatch
//     sets NoBoot=true to avoid lock contention in the daemon.
//
// sling.Run documents the steps it takes.
func executeSling(params SlingParams) (*SlingResult, error) {
	return realSlingDeps().executeSling(params)
}

// executeSling is sling.Run on these collaborators.
func (d *slingDeps) executeSling(params SlingParams) (*SlingResult, error) {
	return sling.Run(context.Background(), d.engineDeps(), params)
}
