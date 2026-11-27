package cmd

import (
	"maps"
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

// TestTickEffQuietRecordsTwoRigs drives the board closure over two rigs read in
// one tick, their hashes from one hashes store: the quiet-rig memory holds each
// under its own name with its own hashes (gt-kgm2oa).
func TestTickEffQuietRecordsTwoRigs(t *testing.T) {
	t.Parallel()
	town := quietWiringTown(t)
	memory := loadSpecAnnotated(filepath.Join(t.TempDir(), "spec_annotated.json"), "daemon-1")
	store := &sqlChildrenClient{records: [][]string{{"be_head", "be_working", "gt_head", "gt_working"}, {"b1", "b2", "g1", "g2"}}}
	board := specTickBoard(specTickBoardDeps{
		townRoot:    town,
		maxPriority: 2,
		reads:       newSpecTickReads(),
		ready:       func(string) ([]*beads.Issue, error) { return nil, nil },
		rigStatus:   func(string, string) (bool, string) { return false, "" },
		heads:       specTickHeadsHold(town, []string{"beads", "gastown"}, slingStores{pinned: func(string) slingStore { return store }}, func(string, ...any) {}),
		memory:      memory,
		now:         func() time.Time { return time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC) },
		detailStore: func(string) beads.Client { return nil },
	})

	if got := board(nil, nil, nil); len(got.Errors) != 0 {
		t.Fatalf("board read: Errors = %v, want none", got.Errors)
	}

	memory.mu.Lock()
	defer memory.mu.Unlock()
	got := map[string]beadsql.RigHash{}
	for rig, read := range memory.rigs {
		got[rig] = beadsql.RigHash{Head: read.Head, Working: read.Working}
	}
	want := map[string]beadsql.RigHash{
		"beads":   {Head: "b1", Working: "b2"},
		"gastown": {Head: "g1", Working: "g2"},
	}
	if !maps.Equal(got, want) {
		t.Errorf("recorded hashes by rig = %v, want %v", got, want)
	}
}
