package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// overrunAction is what the overrun guard does for one seat on one tick.
type overrunAction int

const (
	overrunNone overrunAction = iota
	overrunNudge
	overrunStop
)

// overrunInput is everything decideOverrun reads. The caller supplies Now so
// the decision never touches a clock.
type overrunInput struct {
	Now, SlungAt    time.Time
	Nudged          bool          // the nudge already went out for this sling
	NudgeAt, StopAt time.Duration // elapsed since SlungAt
}

// decideOverrun returns the action for one seat. A zero SlungAt or one after
// Now means the sling time is unknown or wrong, so it acts on nothing. Stop
// wins over nudge, and fires even when the seat was never nudged. A nudge
// fires once: Nudged suppresses it on later ticks.
func decideOverrun(in overrunInput) overrunAction {
	if in.SlungAt.IsZero() || in.SlungAt.After(in.Now) {
		return overrunNone
	}
	elapsed := in.Now.Sub(in.SlungAt)
	switch {
	case elapsed >= in.StopAt:
		return overrunStop
	case elapsed >= in.NudgeAt && !in.Nudged:
		return overrunNudge
	}
	return overrunNone
}

// overrunState is what the guard remembers about one bead between ticks.
//
// The stop's progress is kept here too, one stamp per step, because every
// step after the pause outlives the seat's visibility: once the session is
// gone the seat is no longer a working seat, so a step that fails is retried
// from this record (pendingOverrunStops), not from the seat list.
type overrunState struct {
	Bead     string    `json:"bead"`
	NudgedAt time.Time `json:"nudged_at,omitzero"`

	// Rig, Polecat and Branch name the seat the stop acts on; Elapsed is how
	// long it had held the bead when the stop began.
	Rig     string        `json:"rig,omitempty"`
	Polecat string        `json:"polecat,omitempty"`
	Branch  string        `json:"branch,omitempty"`
	Elapsed time.Duration `json:"elapsed,omitempty"`

	// StopBeganAt is when the pause marker went down, SessionStoppedAt when
	// the session was confirmed gone, CheckpointedAt when the worktree
	// checkpoint step finished. StoppedAt is set last, once the bead is handed
	// back; with it set the stop does not run again for this sling.
	StopBeganAt      time.Time `json:"stop_began_at,omitzero"`
	SessionStoppedAt time.Time `json:"session_stopped_at,omitzero"`
	CheckpointedAt   time.Time `json:"checkpointed_at,omitzero"`
	StoppedAt        time.Time `json:"stopped_at,omitzero"`
}

// overrunStatePath is <town>/.runtime/overrun-guard/<bead>.json. A bead id
// that could leave that directory is an error.
func overrunStatePath(townRoot, bead string) (string, error) {
	if bead == "" || bead != filepath.Base(bead) || strings.ContainsAny(bead, `/\`) {
		return "", fmt.Errorf("overrun guard: invalid bead id %q", bead)
	}
	return filepath.Join(townRoot, ".runtime", "overrun-guard", bead+".json"), nil
}

// readOverrunState loads the state for bead. A missing file is the zero state
// (nothing has happened yet), not an error.
func readOverrunState(townRoot, bead string) (overrunState, error) {
	path, err := overrunStatePath(townRoot, bead)
	if err != nil {
		return overrunState{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return overrunState{}, nil
	}
	if err != nil {
		return overrunState{}, fmt.Errorf("read overrun state: %w", err)
	}
	var s overrunState
	if err := json.Unmarshal(data, &s); err != nil {
		return overrunState{}, fmt.Errorf("parse overrun state %s: %w", path, err)
	}
	return s, nil
}

// writeOverrunState saves s atomically: it writes a temp file in the same
// directory and renames it over the target, so a reader never sees half a file.
func writeOverrunState(townRoot string, s overrunState) error {
	path, err := overrunStatePath(townRoot, s.Bead)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create overrun state dir: %w", err)
	}
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("encode overrun state: %w", err)
	}
	tmp, err := os.CreateTemp(dir, s.Bead+".*.tmp")
	if err != nil {
		return fmt.Errorf("create overrun state temp file: %w", err)
	}
	tmpName := tmp.Name()
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("write overrun state: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace overrun state: %w", err)
	}
	return nil
}

// pendingOverrunStops lists the beads whose stop began but did not finish: the
// pause marker is down and the bead is not yet handed back. A state file that
// cannot be read is skipped; its bead is judged again from the seat list.
func pendingOverrunStops(townRoot string) []overrunState {
	dir := filepath.Join(townRoot, ".runtime", "overrun-guard")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []overrunState
	for _, e := range entries {
		bead, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() {
			continue
		}
		st, err := readOverrunState(townRoot, bead)
		if err != nil || st.StopBeganAt.IsZero() || !st.StoppedAt.IsZero() {
			continue
		}
		st.Bead = bead
		out = append(out, st)
	}
	return out
}
