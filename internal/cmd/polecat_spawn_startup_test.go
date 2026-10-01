package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/dispatch"
)

// TestNoteStartOutcome_RecordsFailureAndClearsOnSuccess guards gt-wacl: the
// sling that fails at session start is what tells the convoy feeders to back
// off, and a later sling that starts its session lifts the backoff.
func TestNoteStartOutcome_RecordsFailureAndClearsOnSuccess(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"type":"town","version":1,"name":"t"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	s := &SpawnedPolecatInfo{HookBead: "gt-abc"}
	s.noteStartOutcomeIn(townRoot, errors.New("starting session: startup blocked: trust dialog"))
	if dispatch.StartupBackoff(townRoot, "gt-abc") == "" {
		t.Fatal("a failed session start should put the bead into backoff")
	}

	s.noteStartOutcomeIn(townRoot, nil)
	if got := dispatch.StartupBackoff(townRoot, "gt-abc"); got != "" {
		t.Errorf("a started session should clear the backoff, got %q", got)
	}

	// A spawn with no bead has nothing to record against.
	(&SpawnedPolecatInfo{}).noteStartOutcomeIn(townRoot, errors.New("boom"))
}
