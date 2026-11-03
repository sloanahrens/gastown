package cmd

import (
	"errors"
	"fmt"
	"slices"
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

// blockerGraph is blockerCreates' read over a seeded graph: descriptions holds
// each bead's description, blockers its open blocker ids. An id absent from
// descriptions is a bead the read fails on.
func blockerGraph(descriptions map[string]string, blockers map[string][]string) func(string) (specdispatch.Spec, []string, error) {
	return func(id string) (specdispatch.Spec, []string, error) {
		description, ok := descriptions[id]
		if !ok {
			return specdispatch.Spec{}, nil, fmt.Errorf("bead %s not found", id)
		}
		return specdispatch.Spec{ID: id, Description: description}, blockers[id], nil
	}
}

// A bead two edges above one that creates a path gets that path: the walk is
// the whole chain, not the direct blockers alone (gt-hgt9j.12).
func TestBlockerCreatesWalksTransitive(t *testing.T) {
	t.Parallel()
	read := blockerGraph(
		map[string]string{
			"gt-a": "## Files\nmodify: x.go\n",
			"gt-b": "## Files\nread: x_test.go\n",
			"gt-c": "## Files\ncreate: x.go\n",
		},
		map[string][]string{"gt-a": {"gt-b"}, "gt-b": {"gt-c"}},
	)
	got, err := blockerCreates(read, "gt-a")
	if err != nil {
		t.Fatalf("blockerCreates: %v", err)
	}
	if want := []string{"x.go"}; !slices.Equal(got, want) {
		t.Errorf("blockerCreates = %v, want %v", got, want)
	}
}

// A blocker with no Files section declares no path, and skipping it is not an
// error: most blockers in a plan predate the Files section (gt-hgt9j.12).
func TestBlockerCreatesSkipsBeadsWithoutFiles(t *testing.T) {
	t.Parallel()
	read := blockerGraph(
		map[string]string{
			"gt-a": "## Files\nmodify: x.go\n",
			"gt-b": "## Goal\nno files declared.\n",
		},
		map[string][]string{"gt-a": {"gt-b"}},
	)
	got, err := blockerCreates(read, "gt-a")
	if err != nil {
		t.Fatalf("blockerCreates: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("blockerCreates = %v, want no paths from a blocker with no Files section", got)
	}
}

// A cycle between two beads terminates, each bead is read once, and the path
// both of them create is returned once (gt-hgt9j.12).
func TestBlockerCreatesCycleSafe(t *testing.T) {
	t.Parallel()
	read := blockerGraph(
		map[string]string{
			"gt-a": "## Files\nmodify: x.go\n",
			"gt-b": "## Files\ncreate: x.go\n",
			"gt-c": "## Files\ncreate: x.go\n",
		},
		map[string][]string{"gt-a": {"gt-b"}, "gt-b": {"gt-c"}, "gt-c": {"gt-b"}},
	)
	reads := map[string]int{}
	counted := func(id string) (specdispatch.Spec, []string, error) {
		reads[id]++
		return read(id)
	}
	got, err := blockerCreates(counted, "gt-a")
	if err != nil {
		t.Fatalf("blockerCreates: %v", err)
	}
	if want := []string{"x.go"}; !slices.Equal(got, want) {
		t.Errorf("blockerCreates = %v, want %v once", got, want)
	}
	for _, id := range []string{"gt-a", "gt-b", "gt-c"} {
		if reads[id] != 1 {
			t.Errorf("read %s %d time(s), want once: the visited set ends the cycle", id, reads[id])
		}
	}
}

// The walk stops at depth 8: a chain of ten beads answers the eight blockers a
// path check can resolve and no more (gt-hgt9j.12).
func TestBlockerCreatesDepthCap(t *testing.T) {
	t.Parallel()
	descriptions := map[string]string{"gt-b0": "## Files\nmodify: f0.go\n"}
	blockers := map[string][]string{}
	for i := 0; i < 9; i++ {
		id, next := fmt.Sprintf("gt-b%d", i), fmt.Sprintf("gt-b%d", i+1)
		blockers[id] = []string{next}
		descriptions[next] = fmt.Sprintf("## Files\ncreate: f%d.go\n", i+1)
	}
	got, err := blockerCreates(blockerGraph(descriptions, blockers), "gt-b0")
	if err != nil {
		t.Fatalf("blockerCreates: %v", err)
	}
	var want []string
	for i := 1; i <= 8; i++ {
		want = append(want, fmt.Sprintf("f%d.go", i))
	}
	if !slices.Equal(got, want) {
		t.Errorf("blockerCreates = %v, want the first eight blockers' paths %v", got, want)
	}
	if slices.Contains(got, "f9.go") {
		t.Errorf("blockerCreates = %v, want the depth-9 blocker's path left out", got)
	}
}

// A blocker that cannot be read is an error naming it, never an empty set: the
// caller refuses the bead rather than passing a path the graph may have held
// (gt-hgt9j.12).
func TestBlockerCreatesReadFailureFailsClosed(t *testing.T) {
	t.Parallel()
	read := blockerGraph(
		map[string]string{"gt-a": "## Files\nmodify: x.go\n"},
		map[string][]string{"gt-a": {"gt-unreadable"}},
	)
	got, err := blockerCreates(read, "gt-a")
	if err == nil {
		t.Fatalf("blockerCreates = %v with no error, want the unreadable blocker named", got)
	}
	if !strings.Contains(err.Error(), "gt-unreadable") {
		t.Errorf("error %q does not name the blocker that could not be read", err)
	}
	if got != nil {
		t.Errorf("blockerCreates = %v beside the error, want no set", got)
	}
}

// A blocker whose Files section does not parse is not read as one declaring no
// paths: the error names the blocker and the bead is refused (gt-hgt9j.12).
func TestBlockerCreatesUnparsableFilesFailsClosed(t *testing.T) {
	t.Parallel()
	read := blockerGraph(
		map[string]string{
			"gt-a": "## Files\nmodify: x.go\n",
			"gt-b": "## Files\nrename: x.go\n",
		},
		map[string][]string{"gt-a": {"gt-b"}},
	)
	got, err := blockerCreates(read, "gt-a")
	if err == nil || !strings.Contains(err.Error(), "gt-b") {
		t.Fatalf("blockerCreates = %v, %v; want an error naming gt-b", got, err)
	}
}

// A bead with no Files section asks nothing of the graph: the path check never
// reads the set for it, so no blocker is read either (gt-hgt9j.12).
func TestBlockerCreatesRootWithoutFilesSkipsTheWalk(t *testing.T) {
	t.Parallel()
	reads := 0
	read := func(id string) (specdispatch.Spec, []string, error) {
		reads++
		if id != "gt-a" {
			return specdispatch.Spec{}, nil, fmt.Errorf("unexpected blocker read of %s", id)
		}
		return specdispatch.Spec{ID: id, Description: "## Goal\nno files declared.\n"}, []string{"gt-b"}, nil
	}
	got, err := blockerCreates(read, "gt-a")
	if err != nil {
		t.Fatalf("blockerCreates: %v", err)
	}
	if got != nil {
		t.Errorf("blockerCreates = %v, want no set", got)
	}
	if reads != 1 {
		t.Errorf("reads = %d, want the bead alone and no blocker read", reads)
	}
}

// specFilesBead is a dispatchable bead whose Files section names path with verb,
// so the lint asks the blocker graph whether an open blocker creates it
// (gt-hgt9j.12).
func specFilesBead(id, verb, path string) specdispatch.Spec {
	s := cleanSpec(id, 2, "2026-09-29T12:00:00Z")
	s.Description = specTestDescription + "\n\n## Files\n" + verb + ": " + path
	return s
}

// The tick refuses a bead whose blocker graph cannot be read, with the read's
// error on the bead's line and no sling: an unreadable graph is never an empty
// one (gt-hgt9j.12).
func TestBlockerCreatesTickRefusesAnUnreadableGraph(t *testing.T) {
	t.Parallel()
	readErr := errors.New("bd show: connection refused")
	f := newFakeSpecTown(specFilesBead("gt-files", "modify", "internal/cmd/spec.go"))
	f.targets = map[string]*specdispatch.TargetTree{
		"gastown": {Branch: "main", Files: map[string]struct{}{"internal/cmd/spec.go": {}}},
	}
	env := f.env()
	env.BlockerRead = func(string) (specdispatch.Spec, []string, error) {
		return specdispatch.Spec{}, nil, readErr
	}

	r := runSpecDispatchCycle(env)

	if len(f.slung) != 0 {
		t.Fatalf("slung %v, want the bead held", f.slung)
	}
	if len(r.Skipped) != 1 || !strings.Contains(r.Skipped[0].Line, readErr.Error()) {
		t.Fatalf("skipped = %+v, want the read error on the bead's line", r.Skipped)
	}
	if len(r.Refused) != 0 {
		t.Errorf("refused = %+v, want no shape refusal recorded for a failed read", r.Refused)
	}
}

// A Files modify path absent from the target branch passes when an open
// blocker creates it, and the bead dispatches (gt-hgt9j.12).
func TestBlockerCreatesTickPassesAPathABlockerAdds(t *testing.T) {
	t.Parallel()
	bead := specFilesBead("gt-files", "modify", "internal/cmd/new.go")
	f := newFakeSpecTown(bead)
	f.targets = map[string]*specdispatch.TargetTree{
		"gastown": {Branch: "main", Files: map[string]struct{}{"internal/cmd/spec.go": {}}},
	}
	env := f.env()
	env.BlockerRead = blockerGraph(
		map[string]string{
			"gt-files":   bead.Description,
			"gt-blocker": "## Files\ncreate: internal/cmd/new.go\n",
		},
		map[string][]string{"gt-files": {"gt-blocker"}},
	)

	r := runSpecDispatchCycle(env)

	if len(f.slung) != 1 || f.slung[0] != "gt-files" {
		t.Fatalf("slung %v, want gt-files; refused %+v skipped %+v", f.slung, r.Refused, r.Skipped)
	}
}

// A blocker read that fails is returned as its own error, never read as a bead
// with no blockers: the walk that calls this refuses the bead on it
// (gt-hgt9j.12).
func TestBlockerCreatesReadHelperReturnsTheReadError(t *testing.T) {
	t.Parallel()
	readErr := errors.New("bd show: connection refused")
	read := specBlockerRead(nil, func(string) (*beads.Issue, error) { return nil, readErr })
	if _, _, err := read("gt-b"); !errors.Is(err, readErr) {
		t.Fatalf("specBlockerRead error = %v, want the read's own error", err)
	}
}
