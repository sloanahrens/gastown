package deacon

import (
	"path/filepath"

	"github.com/steveyegge/gastown/internal/patrolstate"
)

// patrolStateFileName is the deacon's agent-managed state file. Like the
// witness's state.json before gt-oabl, this one is written by the deacon
// itself, not by Go code, which is exactly why its patrol_count can run away
// across sessions.
const patrolStateFileName = "state.json"

// patrolCountKey is the cycle counter the deacon keeps in its state file.
const patrolCountKey = "patrol_count"

// DeaconStateDir returns the directory holding the deacon's state file.
// Unlike the witness, the deacon is not rig-scoped: its state lives directly
// under <townRoot>/deacon, alongside heartbeat.json and the other
// deacon-owned state files (internal/deacon/redispatch.go and friends).
func DeaconStateDir(townRoot string) string {
	return filepath.Join(townRoot, "deacon")
}

// DeaconStatePath returns the path to the deacon's state.json.
func DeaconStatePath(townRoot string) string {
	return filepath.Join(DeaconStateDir(townRoot), patrolStateFileName)
}

// ResetPatrolCount zeroes patrol_count in the deacon's state file so a fresh
// session does not inherit its predecessor's count.
//
// The deacon's role template hands off at the loop-or-exit step once
// patrol_count reaches its ceiling (20) — by design, unlike the witness's
// now-informational counter (gt-oabl). Nothing ever reset it between
// sessions, so a fresh session that inherited 20+ read the ceiling on its
// very first cycle and handed off immediately, before ever reaching `gt
// patrol report`: eight deacon handoff+session_start pairs landed in 26
// minutes on 2026-09-24 with no incident behind any of them (gt-wdv9). That
// rules out bounding the counter at patrol-report time the way
// witness.NormalizePatrolCounter does — the storm never gets there — so this
// must run at session start instead, wired into `gt prime`'s fresh-session
// path.
//
// Every other field is preserved verbatim, so a field added to the file
// later survives. Returns whether the file was rewritten.
//
// A missing file is a no-op. An unreadable or unparseable file is an error
// and is left untouched — a state file we cannot parse is not one to
// overwrite.
//
// The read/zero/atomic-write mechanics are shared with the witness's own
// counter reset (internal/witness.NormalizePatrolCounter, gt-oabl) via
// internal/patrolstate, so this function only owns the deacon's path and key.
func ResetPatrolCount(townRoot string) (bool, error) {
	return patrolstate.ResetCounter(DeaconStatePath(townRoot), patrolCountKey)
}
