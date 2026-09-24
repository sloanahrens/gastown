package witness

import (
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/patrolstate"
)

// patrolStateFileName is the witness's agent-managed state file. Unlike the
// deacon's operational state files (internal/deacon/redispatch.go), this one is
// written by the witness itself, not by Go code — which is exactly why the
// counter in it can run away (gt-oabl: the om witness reached patrol_count 610).
const patrolStateFileName = "state.json"

// patrolCountKey is the cycle counter the witness keeps in its state file.
const patrolCountKey = "patrol_count"

// WitnessStateDir returns the directory holding a rig witness's state file.
// It prefers witness/rig/ for legacy clones, otherwise witness/ — the same
// resolution Manager.witnessDir applies when it starts the session, so the
// path read here is the path the running witness writes.
func WitnessStateDir(townRoot, rig string) string {
	rigDir := filepath.Join(townRoot, rig)
	if _, err := os.Stat(filepath.Join(rigDir, "witness", "rig")); err == nil {
		return filepath.Join(rigDir, "witness", "rig")
	}
	return filepath.Join(rigDir, "witness")
}

// WitnessStatePath returns the path to a rig witness's state.json.
func WitnessStatePath(townRoot, rig string) string {
	return filepath.Join(WitnessStateDir(townRoot, rig), patrolStateFileName)
}

// NormalizePatrolCounter zeroes patrol_count in a rig witness's state file so
// the counter cannot accumulate across cycles.
//
// The counter is hand-maintained by the witness, so a patrol rule that stops
// the loop at a threshold (the role template's old "hand off after 15 patrol
// loops") is enforced by reading a number no code ever bounded — the om witness
// sat at patrol_count 610 with a rule that fired at 15 (gt-oabl). Zeroing at
// every cycle end, from gt patrol report, disarms that rule even for a session
// still running an older rendered system prompt, which a template edit alone
// cannot reach (gt prime writes the role prompt for the *next* spawn).
//
// Every other field is preserved verbatim, so a field added to the file later
// survives. Returns whether the file was rewritten.
//
// A missing file is a no-op. An unreadable or unparseable file is an error and
// is left untouched — a state file we cannot parse is not one to overwrite.
//
// The read/zero/atomic-write mechanics are shared with the deacon's own
// counter reset (internal/deacon.ResetPatrolCount, gt-wdv9) via
// internal/patrolstate, so this function only owns the witness's path and key.
func NormalizePatrolCounter(townRoot, rig string) (bool, error) {
	return patrolstate.ResetCounter(WitnessStatePath(townRoot, rig), patrolCountKey)
}
