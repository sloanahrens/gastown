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

// executeSling is sling.Run on these collaborators. It is the one method every
// in-process engine dispatch goes through — gt sling's rig target, the spec
// dispatcher (slingSpec) and the scheduler's capacity dispatch (the package
// executeSling) — so the bond-lock give-back below reaches each of them without
// any caller remembering to ask for it.
func (d *slingDeps) executeSling(params SlingParams) (*SlingResult, error) {
	result, err := sling.Run(context.Background(), d.engineDeps(), params)
	if err != nil {
		giveBackBondLockRespawn(result, err)
	}
	return result, err
}

// giveBackBondLockRespawn gives back the respawn a spawn charged when the
// engine's formula step failed on a bond that never got the town lock
// (ErrBondLockTimeout). The lock never reached bd, so the failure is the town's
// load rather than a death of the bead: the spec dispatcher defers the bead and
// slings it again next tick, and a count per tick would spend the bead's
// respawn budget on the queue in front of it (gt-8d38p). Every other failure
// keeps its count. The engine returns the spawn beside the error, unless the
// spawn itself failed: then there is no count to give back.
//
// The engine cannot classify the error itself — internal/sling does not import
// cmd — which is why this lives here rather than on the engine's Deps.
func giveBackBondLockRespawn(result *SlingResult, err error) {
	if result == nil || result.SpawnInfo == nil {
		return
	}
	cmdSpawn(result.SpawnInfo).giveBackBondLockRespawn(err)
}
