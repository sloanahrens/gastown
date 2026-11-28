package land

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// approveAheadMerge stores a review approving the merge gateAheadBuild makes of
// f.work onto f.base; the key holds the patch-id, so any merge of it matches.
func approveAheadMerge(t *testing.T, f *landFixture, l *Lander) {
	t.Helper()
	store := TownAheadStore(f.town, "gastown")
	store.Now = l.Now
	l.Ahead, l.ReuseExpiry = store, time.Hour
	origin := f.git.Open(f.origin)
	dir, merged, cleanup, err := l.gateAheadMerge(origin, f.work, f.base)
	must(t, err)
	issue, err := l.Beads.Show(f.work.BeadID)
	must(t, err)
	in, _, _, err := l.reviewInputFor(issue, f.work, f.base, merged, dir, origin, true)
	must(t, err)
	key, _, err := l.reviewKey(context.Background(), origin, l.open(dir), f.work, in, f.base, merged)
	must(t, err)
	cleanup()
	must(t, store.Put(reuseAskRecord(key, f.base, l.now().Add(-time.Minute))))
}

// TestGateAheadRunPushesAndHoldsTheResult is train 3c: a built candidate is
// gated once and the slot hands back that result with the front commit as the
// car's base and no bead written.
func TestGateAheadRunPushesAndHoldsTheResult(t *testing.T) {
	t.Parallel()
	f, l, _ := keyFixture(t)
	approveAheadMerge(t, f, l)
	cand := &fakeCandidate{res: CandidateResult{State: CandidatePassed, Context: "ci / gate (push)"}}
	l.Candidate = cand
	var slot AheadSlot
	l.Ahead2 = &slot
	before := aheadBeadWrites(t, f.bd)
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		if !l.GateAhead(ctx, f.work, f.base) {
			t.Fatal("GateAhead = false, want true: a run should start")
		}
		gate, ok := slot.Take(ctx, f.work.BeadID, f.work.Head, f.base)
		if !ok {
			t.Fatal("Take = false, want the gated car")
		}
		if gate.Base != f.base {
			t.Errorf("gate.Base = %s, want the front commit %s", shortSHA(gate.Base), shortSHA(f.base))
		}
		if gate.Merged == "" {
			t.Fatal("gate.Merged is empty, want the merge the run reported")
		}
		if gate.Result.State != CandidatePassed || gate.Result.SHA != gate.Merged {
			t.Errorf("gate.Result = %v at %s, want passed at the gated %s", gate.Result.State, shortSHA(gate.Result.SHA), shortSHA(gate.Merged))
		}
		if len(cand.calls) != 1 {
			t.Fatalf("the candidate gate ran %d time(s), want once", len(cand.calls))
		}
		if want := filepath.Join(f.workRoot, landWorktreeName+"-1"); len(cand.dirs) != 1 || cand.dirs[0] != want {
			t.Errorf("the candidate gate dir = %v, want slot 1's %s", cand.dirs, want)
		}
		if wantBranch := "land/" + f.work.BeadID; cand.calls[0].CandidateBranch != wantBranch || cand.calls[0].CandidateHead != gate.Merged {
			t.Errorf("the candidate gate's work = (%q, %q), want (%q, %q)", cand.calls[0].CandidateBranch, cand.calls[0].CandidateHead, wantBranch, gate.Merged)
		}
	})
	if n := aheadBeadWrites(t, f.bd) - before; n != 0 {
		t.Errorf("GateAhead wrote %d bead records, want 0", n)
	}
}

// TestGateAheadRunBuildErrorStartsNoGate is train 3c: a failing build ends the
// run with its reason, logs the bead's not-started line once, runs no gate and
// writes no bead.
func TestGateAheadRunBuildErrorStartsNoGate(t *testing.T) {
	t.Parallel()
	f, l, _ := keyFixture(t)
	cand := &fakeCandidate{res: CandidateResult{State: CandidatePassed, Context: "ci / gate (push)"}}
	l.Candidate = cand
	var out bytes.Buffer
	l.Out = &out
	var slot AheadSlot
	l.Ahead2 = &slot
	before := aheadBeadWrites(t, f.bd)
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		if !l.GateAhead(ctx, f.work, f.base) {
			t.Fatal("GateAhead = false, want true: a run starts and then fails")
		}
		if _, ok := slot.Take(ctx, f.work.BeadID, f.work.Head, f.base); ok {
			t.Fatal("Take = true, want false for a failed run")
		}
		if len(cand.calls) != 0 {
			t.Errorf("the candidate gate ran %d time(s), want 0", len(cand.calls))
		}
		if n := strings.Count(out.String(), "gate-ahead: not started:"); n != 1 {
			t.Errorf("not-started lines = %d, want 1; log:\n%s", n, out.String())
		}
		if want := f.work.BeadID + ": gate-ahead: not started: "; !strings.Contains(out.String(), want) {
			t.Errorf("log = %q, want it to carry %q", out.String(), want)
		}
	})
	if n := aheadBeadWrites(t, f.bd) - before; n != 0 {
		t.Errorf("GateAhead wrote %d bead records, want 0", n)
	}
	assertNoWorkRootEntries(t, f)
}

// TestGateAheadRunOffStartsNothing is train 3c: a nil slot starts no run.
func TestGateAheadRunOffStartsNothing(t *testing.T) {
	t.Parallel()
	f, l, _ := keyFixture(t)
	cand := &fakeCandidate{res: CandidateResult{State: CandidatePassed, Context: "ci / gate (push)"}}
	l.Candidate = cand
	if l.GateAhead(context.Background(), f.work, f.base) {
		t.Fatal("GateAhead with a nil slot = true, want false")
	}
	if len(cand.calls) != 0 {
		t.Errorf("the candidate gate ran %d time(s), want 0", len(cand.calls))
	}
}
