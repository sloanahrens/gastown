package cmd

import (
	"maps"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beadsql"
)

// blockedWiringStore answers the candidates' full-bead re-read with the beads the
// test's ready reads listed, under any rig.
type blockedWiringStore struct {
	beads.Client
	issues map[string]*beads.Issue
}

func (s blockedWiringStore) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	out := make(map[string]*beads.Issue, len(ids))
	for _, id := range ids {
		if issue, ok := s.issues[id]; ok {
			out[id] = issue
		}
	}
	return out, nil
}

// TestTickBlockedReuseWiring: two ticks share one quiet-rig memory. gastown's
// hashes hold across both, so its blocked list is read once; beads' head moves
// between them, so its list is read on both. Each tick orders the candidates by
// the inherited priority: be-cand inherits P0 from gt-hi in gastown, so it leads
// gt-cand, which has the same own priority P2 (gt-bbtsi4).
func TestTickBlockedReuseWiring(t *testing.T) {
	t.Parallel()
	town := quietWiringTown(t)
	now := time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC)
	memory := loadSpecAnnotated(filepath.Join(t.TempDir(), "spec_annotated.json"), "daemon-1")
	heads := map[string]beadsql.RigHash{
		"gastown": {Head: "g1", Working: "g2"},
		"beads":   {Head: "b1", Working: "b2"},
	}
	candidates := map[string][]*beads.Issue{
		"gastown": {{ID: "gt-cand", Type: "task", Status: "open", Priority: 2}},
		"beads":   {{ID: "be-cand", Type: "task", Status: "open", Priority: 2}},
	}
	blockedLists := map[string][]*beads.Issue{
		"gastown": {{ID: "gt-hi", Priority: 0, BlockedBy: []string{"be-cand"}}},
	}
	blockedReads := map[string]int{}
	store := blockedWiringStore{issues: map[string]*beads.Issue{
		"gt-cand": candidates["gastown"][0],
		"be-cand": candidates["beads"][0],
	}}
	tick := func() specBoardRead {
		board := specTickBoard(specTickBoardDeps{
			townRoot:    town,
			maxPriority: 2,
			reads:       newSpecTickReads(),
			ready: func(rigPath string) ([]*beads.Issue, error) {
				return candidates[filepath.Base(rigPath)], nil
			},
			rigStatus: func(string, string) (bool, string) { return false, "" },
			heads:     func() map[string]beadsql.RigHash { return maps.Clone(heads) },
			memory:    memory,
			now:       func() time.Time { return now },
			detailStore: func(string) beads.Client {
				return store
			},
			rigs: []string{"gastown", "beads"},
			blocked: func(beadsDir string) ([]*beads.Issue, error) {
				rig := filepath.Base(filepath.Dir(beadsDir))
				blockedReads[rig]++
				return blockedLists[rig], nil
			},
		})
		return board(nil, nil, nil)
	}

	for i, wantOrder := range [][]string{{"be-cand", "gt-cand"}, {"be-cand", "gt-cand"}} {
		if i == 1 {
			heads["beads"] = beadsql.RigHash{Head: "b3", Working: "b2"}
		}
		got := tick()
		if len(got.Errors) != 0 {
			t.Fatalf("tick %d: Errors = %v, want none", i+1, got.Errors)
		}
		var order []string
		for _, c := range got.Candidates {
			order = append(order, c.Spec.ID)
		}
		if !slices.Equal(order, wantOrder) {
			t.Errorf("tick %d: candidate order = %v, want %v by inherited priority", i+1, order, wantOrder)
		}
		if eff := got.Inherited["be-cand"]; eff.Priority != 0 {
			t.Errorf("tick %d: be-cand inherited priority = %d, want 0 from gt-hi", i+1, eff.Priority)
		}
	}

	if blockedReads["gastown"] != 1 {
		t.Errorf("gastown's blocked list read %d time(s) over two ticks, want 1: its hashes held", blockedReads["gastown"])
	}
	if blockedReads["beads"] != 2 {
		t.Errorf("beads' blocked list read %d time(s) over two ticks, want 2: its head moved", blockedReads["beads"])
	}
}
