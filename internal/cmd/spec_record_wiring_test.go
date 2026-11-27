package cmd

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beadsql"
)

// TestTickEffQuietRecordsOneRig drives the board closure the tick builds over one
// rig that is read and one that is parked: afterwards the quiet-rig memory holds
// the read rig under its own name, with the hash the tick held, the time of the
// read and the number of ready beads, and holds nothing for the parked rig
// (gt-eu5n6o).
func TestTickEffQuietRecordsOneRig(t *testing.T) {
	t.Parallel()
	town := quietWiringTown(t)
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	memory := loadSpecAnnotated(filepath.Join(t.TempDir(), "spec_annotated.json"), "daemon-1")
	board := specTickBoard(specTickBoardDeps{
		townRoot:    town,
		maxPriority: 2,
		reads:       newSpecTickReads(),
		// A ready bead below the priority cut: counted by the read, no candidate.
		ready: func(string) ([]*beads.Issue, error) {
			return []*beads.Issue{{ID: "gt-low", Priority: 4}}, nil
		},
		rigStatus: func(_, rig string) (bool, string) { return rig == "beads", "parked" },
		heads: func() map[string]beadsql.RigHash {
			return map[string]beadsql.RigHash{"gastown": {Head: "g1", Working: "g2"}, "beads": {Head: "b1", Working: "b2"}}
		},
		memory:      memory,
		now:         func() time.Time { return now },
		detailStore: func(string) beads.Client { return nil },
	})

	if got := board(nil, nil, nil); len(got.Errors) != 0 {
		t.Fatalf("board read: Errors = %v, want none", got.Errors)
	}

	memory.mu.Lock()
	defer memory.mu.Unlock()
	want := specRigRead{Head: "g1", Working: "g2", At: now, Candidates: 1}
	if got := memory.rigs["gastown"]; got != want {
		t.Errorf("recorded %+v, want %+v", got, want)
	}
	if len(memory.rigs) != 1 {
		t.Errorf("memory holds %v, want only the rig that was read", memory.rigs)
	}
}
