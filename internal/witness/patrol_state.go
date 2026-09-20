package witness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
func NormalizePatrolCounter(townRoot, rig string) (bool, error) {
	path := WitnessStatePath(townRoot, rig)

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading %s: %w", path, err)
	}

	// Raw messages, not a struct: this file is the witness's own and carries
	// fields Go knows nothing about, which must survive the rewrite.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return false, fmt.Errorf("parsing %s: %w", path, err)
	}

	count, ok := fields[patrolCountKey]
	if !ok {
		// No counter to bound — nothing to do, and adding one would be a
		// field the witness never asked for.
		return false, nil
	}

	var current int
	if err := json.Unmarshal(count, &current); err != nil {
		return false, fmt.Errorf("parsing %s in %s: %w", patrolCountKey, path, err)
	}
	if current == 0 {
		return false, nil // already normalized; do not touch the file
	}

	fields[patrolCountKey] = json.RawMessage("0")
	out, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return false, fmt.Errorf("encoding %s: %w", path, err)
	}
	out = append(out, '\n')

	// Preserve the file mode the witness gave it.
	mode := os.FileMode(0600)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}

	// Write-then-rename: the witness may be writing this file concurrently, and
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
