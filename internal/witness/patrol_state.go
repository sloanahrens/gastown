package witness

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// PatrolStateFileName is the witness's per-session patrol state file. The
// rendered witness role prompt tells the agent to read patrol_count and
// extraordinary_action from it at the loop-or-exit step and hand off when the
// counter passes its ceiling.
const PatrolStateFileName = "state.json"

// WitnessDir returns the working directory of a rig's witness, preferring
// witness/rig/ for existing legacy clones.
func WitnessDir(rigPath string) string {
	witnessRigDir := filepath.Join(rigPath, "witness", "rig")
	if _, err := os.Stat(witnessRigDir); err == nil {
		return witnessRigDir
	}
	return filepath.Join(rigPath, "witness")
}

// PatrolStatePath returns the path to a witness's patrol state file.
func PatrolStatePath(witnessDir string) string {
	return filepath.Join(witnessDir, PatrolStateFileName)
}

// ResetPatrolState zeroes patrol_count and clears extraordinary_action in the
// witness state file in witnessDir, keeping every other field intact. It
// reports whether it rewrote the file.
//
// The counter is per-session: the loop-or-exit step hands off once
// patrol_count reaches its ceiling, and nothing else ever resets it, so a
// count inherited from a predecessor makes every fresh session hand off after
// one cycle and go idle at the prompt (gt-oabl). The same applies to
// extraordinary_action, which left set turns a crashed session into a handoff
// loop. Session start is the one moment where "patrols this session" is
// well defined, so that is where the reset belongs — determined in code rather
// than by the agent remembering.
//
// A missing file is a no-op, and an unparseable one is reported but left
// untouched: it is evidence, and the caller treats the failure as non-fatal.
func ResetPatrolState(witnessDir string) (bool, error) {
	path := PatrolStatePath(witnessDir)

	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read witness state %s: %w", path, err)
	}

	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &fields); err != nil {
		return false, fmt.Errorf("parse witness state %s: %w", path, err)
	}

	if !patrolStateNeedsReset(fields) {
		return false, nil
	}

	fields["patrol_count"] = json.RawMessage("0")
	fields["extraordinary_action"] = json.RawMessage("false")

	out, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return false, fmt.Errorf("encode witness state %s: %w", path, err)
	}
	out = append(out, '\n')

	mode := fs.FileMode(0o644)
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
	}
	if err := writeFileAtomic(path, out, mode); err != nil {
		return false, err
	}
	return true, nil
}

// patrolStateNeedsReset reports whether the parsed state carries a non-zero
// patrol counter or a set extraordinary-action flag. A missing or unusable key
// counts as needing a reset, so a file that has never carried the patrol keys
// ends up with both of them present.
func patrolStateNeedsReset(fields map[string]json.RawMessage) bool {
	var count float64
	if err := json.Unmarshal(fields["patrol_count"], &count); err != nil {
		return true
	}
	if count != 0 {
		return true
	}

	var extraordinary bool
	if err := json.Unmarshal(fields["extraordinary_action"], &extraordinary); err != nil {
		return true
	}
	return extraordinary
}

// writeFileAtomic replaces path with data in one rename, so a witness reading
// the file concurrently never sees a half-written state.
func writeFileAtomic(path string, data []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), PatrolStateFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	// A no-op once the rename below succeeds; the cleanup path otherwise.
	defer os.Remove(tmpName)

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp for %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp for %s: %w", path, err)
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return fmt.Errorf("chmod temp for %s: %w", path, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
