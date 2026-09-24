package deacon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
func ResetPatrolCount(townRoot string) (bool, error) {
	path := DeaconStatePath(townRoot)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading %s: %w", path, err)
	}

	// Raw messages, not a struct: this file is the deacon's own and carries
	// fields Go knows nothing about, which must survive the rewrite.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return false, fmt.Errorf("parsing %s: %w", path, err)
	}

	count, ok := fields[patrolCountKey]
	if !ok {
		// No counter to reset — nothing to do, and adding one would be a
		// field the deacon never asked for.
		return false, nil
	}

	var current int
	if err := json.Unmarshal(count, &current); err != nil {
		return false, fmt.Errorf("parsing %s in %s: %w", patrolCountKey, path, err)
	}
	if current == 0 {
		return false, nil // already reset; do not touch the file
	}

	fields[patrolCountKey] = json.RawMessage("0")
	out, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return false, fmt.Errorf("encoding %s: %w", path, err)
	}
	out = append(out, '\n')

	// Preserve the file mode the deacon gave it.
	mode := os.FileMode(0600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}

	// Write-then-rename: the deacon may be writing this file concurrently, and
	// a reader must never see a half-written state file.
	tmp, err := os.CreateTemp(filepath.Dir(path), patrolStateFileName+".tmp-*")
	if err != nil {
		return false, fmt.Errorf("creating temp file for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed

	if _, err := tmp.Write(out); err != nil {
		_ = tmp.Close()
		return false, fmt.Errorf("writing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return false, fmt.Errorf("closing %s: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return false, fmt.Errorf("setting mode on %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return false, fmt.Errorf("replacing %s: %w", path, err)
	}

	return true, nil
}
