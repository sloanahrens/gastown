package cmd

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// A held rig's bead is skipped by its hold even when its edges also refuse it:
// the hold, not the blocker, is what stops the rig's beads, and a blocker line
// would hide it (gt-acn51, gt-vty1v).
func TestSpecDispatchNamesTheRigHoldBeforeABlockersRefusal(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-acn51", 2, "2026-09-29T10:00:00Z"))
	f.rigHold["gastown"] = "rig ESTOP"
	f.blockers["gt-acn51"] = []beads.IssueDep{{ID: "gt-frtad", Status: "deferred", DependencyType: "blocks"}}

	r := runSpecDispatchCycle(f.env())
	if len(f.slung) != 0 {
		t.Fatalf("slung %v under a rig hold", f.slung)
	}
	if len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0].Line, specdispatch.SkipRigHold) ||
		strings.Contains(r.Skipped[0].Line, "blocked by") {
		t.Fatalf("skipped = %+v, want the hold named and no blocker line", r.Skipped)
	}
}

// The admission wiring runs end to end (gt-acn51): the ready board carries no
// dependency rows, so the edges the tick refuses on are the ones
// specBoardDetails read from the rig's store, and the skip line is the tick's
// own. A tick handed Blockers directly, as the fake does, says nothing about
// that wiring.
func TestSpecDispatchAdmitsOnTheEdgesTheBoardDetailsReadFilled(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// blocker is the blocking bead's status; "" seeds no dependency, so the
		// store answers a bead with no edges.
		blocker string
		skip    string // the tick's skip line; "" dispatches
	}{
		{name: "deferred blocker", blocker: "deferred", skip: "gt-acn51: blocked by gt-frtad (deferred)"},
		{name: "in progress blocker", blocker: "in_progress", skip: "gt-acn51: blocked by gt-frtad (in_progress)"},
		{name: "closed blocker", blocker: "closed"},
		{name: "no dependency"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := beadsfake.New()
			db.Seed(beads.Issue{ID: "gt-acn51"})
			if tc.blocker != "" {
				db.Seed(beads.Issue{ID: "gt-frtad", Status: tc.blocker})
				if err := db.AddDependency("gt-acn51", "gt-frtad"); err != nil {
					t.Fatalf("seed the blocker: %v", err)
				}
			}
			store := newCountingStore(db)
			f := newFakeSpecTown(cleanSpec("gt-acn51", 2, "2026-09-29T10:00:00Z"))
			env := f.env()
			board := env.Candidates
			env.Candidates = func() specBoardRead {
				return specBoardDetails(newSpecTickReads(), func(string) beads.Client { return store }, board(), nil)
			}

			r := runSpecDispatchCycle(env)

			if len(store.multis) != 1 {
				t.Errorf("board detail reads = %v, want the one batch that answered the edges", store.multis)
			}
			if tc.skip == "" {
				if len(r.Dispatched) != 1 || r.Dispatched[0].Bead != "gt-acn51" {
					t.Fatalf("dispatched = %+v, want the bead the read answered unblocked", r.Dispatched)
				}
				if len(r.Skipped) != 0 {
					t.Errorf("skipped = %+v, want none", r.Skipped)
				}
				return
			}
			if len(f.slung) != 0 || len(r.Dispatched) != 0 {
				t.Fatalf("slung %v, want the blocked bead left ready", f.slung)
			}
			if len(r.Skipped) != 1 || r.Skipped[0].Line != tc.skip {
				t.Fatalf("skipped = %+v, want %q", r.Skipped, tc.skip)
			}
		})
	}
}

// TestSpecDispatchSkipsACandidateItsBlockersRefuse is gt-ndad8: the ready board
// is not the admission check. bd's ready query reads a deferred blocker as
// resolved, so a bead the board lists can still be blocked, and the tick asks
// sling.Admit before it spends a read or a seat on the candidate. The refusal
// is a skip, not a failure: the bead stays ready and the next tick asks again
// once the blocker closes.
func TestSpecDispatchSkipsACandidateItsBlockersRefuse(t *testing.T) {
	t.Parallel()
	dep := func(id, status, kind string) beads.IssueDep {
		return beads.IssueDep{ID: id, Status: status, DependencyType: kind}
	}
	cases := []struct {
		name string
		deps []beads.IssueDep
		skip string // the reason the skip line names; "" dispatches
	}{
		{name: "no dependencies"},
		{name: "open blocker", deps: []beads.IssueDep{dep("gt-frtad", "open", "blocks")}, skip: "blocked by gt-frtad (open)"},
		{name: "deferred blocker", deps: []beads.IssueDep{dep("gt-frtad", "deferred", "blocks")}, skip: "blocked by gt-frtad (deferred)"},
		{name: "in progress blocker", deps: []beads.IssueDep{dep("gt-frtad", "in_progress", "blocks")}, skip: "blocked by gt-frtad (in_progress)"},
		{name: "closed blocker"},
		{name: "open edge that does not block", deps: []beads.IssueDep{dep("gt-frtad", "open", "related")}},
		{
			name: "one of two open",
			deps: []beads.IssueDep{dep("gt-done1", "closed", "blocks"), dep("gt-frtad", "deferred", "blocks")},
			skip: "blocked by gt-frtad (deferred)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeSpecTown(cleanSpec("gt-ndad8", 2, "2026-09-29T10:00:00Z"))
			f.blockers["gt-ndad8"] = tc.deps
			r := runSpecDispatchCycle(f.env())

			if tc.skip == "" {
				if len(f.slung) != 1 || len(r.Dispatched) != 1 {
					t.Fatalf("slung %v with %d dispatched, want the unblocked bead dispatched", f.slung, len(r.Dispatched))
				}
				if len(r.Skipped) != 0 {
					t.Errorf("skipped = %+v, want none", r.Skipped)
				}
				return
			}
			if len(f.slung) != 0 || len(r.Dispatched) != 0 {
				t.Fatalf("slung %v, want the blocked bead left ready", f.slung)
			}
			if len(r.Skipped) != 1 || r.Skipped[0].Bead != "gt-ndad8" {
				t.Fatalf("skipped = %+v, want the candidate's one skip entry", r.Skipped)
			}
			if want := "gt-ndad8: " + tc.skip; r.Skipped[0].Line != want {
				t.Errorf("skip line = %q, want %q", r.Skipped[0].Line, want)
			}
		})
	}
}

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

// The wrapper checks the read a dispatch starts from and no other. executeSling
// reads the bead again after the spawn to catch a hold written meanwhile
// (gt-0k7kb); a blocker that opens between the two reads is not that read's
// question, and refusing there would call the bead's fields unreadable.
func TestSpecSlingChecksBlockersOnlyOnTheFirstRead(t *testing.T) {
	t.Parallel()
	h := newSlingHarness(t)
	h.addBead(slingBead, beadInfo{})
	read := h.run.beadInfoInTown
	reads := 0
	h.run.beadInfoInTown = refuseOpenBlockers(func(town, id string) (*beadInfo, error) {
		info, err := read(town, id)
		reads++
		if reads > 1 && info != nil {
			info.Dependencies = []beads.IssueDep{{ID: "gt-frtad", Status: "open", DependencyType: "blocks"}}
		}
		return info, err
	})

	if _, err := h.run.executeSling(executeParams()); err != nil {
		t.Fatalf("executeSling: %v", err)
	}
	if reads < 2 {
		t.Fatalf("executeSling read the bead %d time(s), want the re-read after the spawn too", reads)
	}
	h.wantCalls("spawn", "spawn gastown")
}

// Existing skip reasons and dispatch order for a bead that is not blocked are
// byte-identical with and without the wrapper (gt-ewrtm): the same calls in the
// same order, the same output, and a failed read keeps its own error.
func TestSpecSlingWrapperLeavesAnUnblockedDispatchUnchanged(t *testing.T) {
	t.Parallel()
	closed := []beads.IssueDep{{ID: "gt-done1", Status: "closed", DependencyType: "blocks"}}
	cases := []struct {
		name string
		bead *beadInfo // nil leaves the bead out, so the read fails
	}{
		{name: "no dependencies", bead: &beadInfo{}},
		{name: "closed blocker", bead: &beadInfo{Dependencies: closed}},
		{name: "unreadable bead"},
	}
	run := func(t *testing.T, bead *beadInfo, wrap bool) (log []string, out string, err error) {
		h := newSlingHarness(t)
		if bead != nil {
			h.addBead(slingBead, *bead)
		}
		if wrap {
			h.run.beadInfoInTown = refuseOpenBlockers(h.run.beadInfoInTown)
		}
		_, err = h.run.executeSling(executeParams())
		return h.log(), h.out.String(), err
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			wantLog, wantOut, wantErr := run(t, tc.bead, false)
			gotLog, gotOut, gotErr := run(t, tc.bead, true)
			if strings.Join(gotLog, "\n") != strings.Join(wantLog, "\n") {
				t.Errorf("calls with the wrapper:\n  %s\nwithout:\n  %s", strings.Join(gotLog, "\n  "), strings.Join(wantLog, "\n  "))
			}
			if gotOut != wantOut {
				t.Errorf("output with the wrapper %q, without %q", gotOut, wantOut)
			}
			if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
				t.Errorf("error with the wrapper %v, without %v", gotErr, wantErr)
			}
		})
	}
}
