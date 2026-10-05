package landings

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
)

// BackoffRecord is one bead a rig's landing worker is backing off from: a
// landing that keeps failing, and the facts a reader needs to see it. It
// mirrors the worker's own record; a test pins the keys to a line the writer
// produces.
type BackoffRecord struct {
	Bead     string    `json:"bead"`
	Stage    string    `json:"stage"`
	Failures int       `json:"failures"`
	NextTry  time.Time `json:"next_try"`
	Error    string    `json:"error"`
}

// BackoffState is one rig's landing backoff as the landing worker last wrote
// it: every bead whose landing failed and is waiting for a retry. An empty
// Beads is a healthy rig.
type BackoffState struct {
	Rig   string          `json:"rig"`
	At    time.Time       `json:"at"`
	Beads []BackoffRecord `json:"beads"`
}

// BackoffPath is <town>/.runtime/landings/<rig>.backoff.json, the file the
// landing worker rewrites after each pass. A rig name that could leave the
// directory is refused.
func BackoffPath(townRoot, rig string) (string, error) {
	if rig == "" || strings.ContainsAny(rig, `/\`) || rig == "." || rig == ".." {
		return "", fmt.Errorf("invalid rig name %q for a backoff file", rig)
	}
	return filepath.Join(constants.TownRuntimePath(townRoot), "landings", rig+".backoff.json"), nil
}

// ReadBackoff reads one rig's backoff file. A missing file has no landing
// worker writing for that rig yet: it reads as a healthy empty state, not as
// an error, so a rig the worker does not serve stays green.
func ReadBackoff(path string) (BackoffState, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return BackoffState{}, nil
	}
	if err != nil {
		return BackoffState{}, fmt.Errorf("reading backoff file: %w", err)
	}
	var st BackoffState
	if err := json.Unmarshal(b, &st); err != nil {
		return BackoffState{}, fmt.Errorf("reading backoff file %s: %w", path, err)
	}
	return st, nil
}
