package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// TestSpecSlingRefusesABeadBlockedAfterTheBoardRead is gt-ewrtm: the ready
// board is read at the start of a tick that can run for minutes, so a blocker
// added after it is only visible in the bead the sling reads.
func TestSpecSlingRefusesABeadBlockedAfterTheBoardRead(t *testing.T) {
	t.Parallel()
	dep := func(id, status, kind string) beads.IssueDep {
		return beads.IssueDep{ID: id, Status: status, DependencyType: kind}
	}
	cases := []struct {
		name    string
		deps    []beads.IssueDep
		refused string // the text the refusal names; "" dispatches
	}{
		{name: "no dependencies"},
		{name: "open blocker", deps: []beads.IssueDep{dep("gt-frtad", "open", "blocks")}, refused: "blocked by gt-frtad"},
		{name: "in progress blocker", deps: []beads.IssueDep{dep("gt-frtad", "in_progress", "blocks")}, refused: "blocked by gt-frtad"},
		{name: "closed blocker", deps: []beads.IssueDep{dep("gt-frtad", "closed", "blocks")}},
		{name: "open edge that does not block", deps: []beads.IssueDep{dep("gt-frtad", "open", "related")}},
		{
			name:    "one of two open",
			deps:    []beads.IssueDep{dep("gt-done1", "closed", "blocks"), dep("gt-frtad", "open", "blocks")},
			refused: "blocked by gt-frtad",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newSlingHarness(t)
			h.addBead(slingBead, beadInfo{Dependencies: tc.deps})
			h.run.beadInfoInTown = refuseOpenBlockers(h.run.beadInfoInTown)

			_, err := h.run.executeSling(executeParams())
			if tc.refused == "" {
				if err != nil {
					t.Fatalf("executeSling: %v", err)
				}
				h.wantCalls("spawn", "spawn gastown")
				return
			}
			wantSlingErr(t, err, tc.refused)
			if !isSpecSlingRefusal(err) {
				t.Errorf("%q is not read as a deferral: the tick would label the bead dispatch-failed", err)
			}
			if isSpecOverlapRefusal(err) {
				t.Errorf("%q is read as a content overlap", err)
			}
			h.wantNo("spawn")
			h.wantNo("hook")
		})
	}
}

// A degraded duplicate pool only warns: the bead's own show answered and shows
// no open blocker, so it still dispatches (gt-ewrtm).
func TestSpecSlingDispatchesWhenOnlyThePoolFetchFailed(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.addBead(slingBead, beadInfo{Dependencies: []beads.IssueDep{{ID: "gt-done1", Status: "closed", DependencyType: "blocks"}}})
	h.run.beadInfoInTown = refuseOpenBlockers(h.run.beadInfoInTown)
	h.run.checkDuplicates = func(string, string, *beadInfo) (*duplicateCandidate, []duplicateMatch, error) {
		return nil, nil, errors.New("duplicate check skipped: fetching design/notes for 40 bead(s): bd show failed")
	}

	if _, err := h.run.executeSling(executeParams()); err != nil {
		t.Fatalf("executeSling: %v", err)
	}
	h.wantCalls("spawn", "spawn gastown")
}

// A bead whose own read fails is not slung, and says why, not that it is blocked.
func TestSpecSlingReadFailureStaysTheReadsError(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.run.beadInfoInTown = refuseOpenBlockers(h.run.beadInfoInTown)

	_, err := h.run.executeSling(executeParams())
	wantSlingErr(t, err, "could not get bead info")
	if strings.Contains(err.Error(), "blocked by") {
		t.Errorf("a failed read is reported as a blocker: %v", err)
	}
	h.wantNo("spawn")
}
