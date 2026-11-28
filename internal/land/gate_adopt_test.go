package land

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// gatePassed is a candidate gate run that passed.
var gatePassed = CandidateResult{State: CandidatePassed, Context: "ci / gate (push)"}

// seqCandidate answers the n-th gate run with results[n], and the last result
// for any run past the list.
type seqCandidate struct {
	*fakeCandidate
	results []CandidateResult
	// ran, when set, is closed by the first gate run, which starts once the
	// build has made its last write to the work root.
	ran chan struct{}
}

func (c *seqCandidate) Run(ctx context.Context, wt Repo, dir string, w Work, head string) CandidateResult {
	if c.ran != nil {
		close(c.ran)
		c.ran = nil
	}
	c.fakeCandidate.res = c.results[min(len(c.calls), len(c.results)-1)]
	return c.fakeCandidate.Run(ctx, wt, dir, w, head)
}

// gatedAheadCar holds a car gated ahead on f.base, its gate runs answering
// results in turn, after the review approval its build needs is stored. It
// returns the candidate and the lander's log.
func gatedAheadCar(t *testing.T, f *landFixture, l *Lander, results ...CandidateResult) (*seqCandidate, *strings.Builder) {
	t.Helper()
	cand := &seqCandidate{fakeCandidate: &fakeCandidate{}, results: results}
	var out strings.Builder
	l.Candidate, l.Out, l.Ahead2 = cand, &out, &AheadSlot{}
	approveAheadMerge(t, f, l)
	if !l.GateAhead(context.Background(), f.work, f.base) {
		t.Fatal("GateAhead = false, want a car held")
	}
	return cand, &out
}

// assertDiscarded checks a car that was let go: its log line, its candidate
// branch discarded once, its worktree removed, and the landing's own gate run
// after the car's.
func assertDiscarded(t *testing.T, f *landFixture, cand *seqCandidate, out *strings.Builder) {
	t.Helper()
	if want := f.work.BeadID + ": gate-ahead: discarded"; !strings.Contains(out.String(), want) {
		t.Errorf("log = %q, want it to carry %q", out.String(), want)
	}
	if want := []string{"land/" + f.work.BeadID}; !slices.Equal(cand.discarded, want) {
		t.Errorf("discarded %v, want %v", cand.discarded, want)
	}
	if len(cand.calls) != 2 {
		t.Errorf("the candidate gate ran %d time(s), want 2: the gate-ahead run and the landing's own", len(cand.calls))
	}
	assertSlotOneGone(t, f)
}

// assertSlotOneGone checks the slot 1 worktree is absent from the work root.
func assertSlotOneGone(t *testing.T, f *landFixture) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(f.workRoot, landWorktreeName+"-1")); !os.IsNotExist(err) {
		t.Errorf("slot 1 worktree: stat err = %v, want it removed", err)
	}
}

// buildFailureLog is the lander's log: it closes failed the first time the
// gate-ahead build logs that it did not start.
type buildFailureLog struct {
	failed chan struct{}
}

func (b buildFailureLog) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "gate-ahead: not started") {
		select {
		case <-b.failed:
		default:
			close(b.failed)
		}
	}
	return len(p), nil
}

// probeApprover answers the version probe a review key reads from keyFixture's
// reviewer, and approves every review it is asked for.
type probeApprover struct {
	*keyReviewer
}

func (probeApprover) Review(context.Context, string, string, string) (Verdict, error) {
	return Verdict{Verdict: VerdictApprove, Score: 0.9}, nil
}

// patchIDFails reads no patch-id, as an adopted car's read would fail.
type patchIDFails struct {
	Repo
}

func (patchIDFails) PatchID(string, string) (string, error) {
	return "", errors.New("patch-id read failed")
}

// TestGateAdoptLandsTheGatedCommit is train 4b-2: a car gated ahead on the
// target tip is adopted, so the landing merges and gates nothing and lands the
// commit the one gate run tested.
func TestGateAdoptLandsTheGatedCommit(t *testing.T) {
	t.Parallel()
	f, l, _ := keyFixture(t)
	cand, out := gatedAheadCar(t, f, l, gatePassed)
	res, err := l.Land(context.Background(), f.work)
	must(t, err)
	if len(cand.calls) != 1 {
		t.Fatalf("the candidate gate ran %d time(s), want once: the gate-ahead run only", len(cand.calls))
	}
	merged := cand.calls[0].CandidateHead
	if res.LandedCommit != merged || f.originMain() != merged {
		t.Errorf("landed %s, origin main %s; want the gated commit %s", shortSHA(res.LandedCommit), shortSHA(f.originMain()), shortSHA(merged))
	}
	if want := f.work.BeadID + ": gate-ahead: adopted " + shortSHA(merged) + " on " + shortSHA(f.base); !strings.Contains(out.String(), want) {
		t.Errorf("log = %q, want it to carry %q", out.String(), want)
	}
	assertSlotOneGone(t, f)
}

// TestGateAdoptRedStoredResultRejects is train 4b-2: an adopted car whose stored
// gate run is red is refused as a red gate is, with no second gate run.
func TestGateAdoptRedStoredResultRejects(t *testing.T) {
	t.Parallel()
	f, l, _ := keyFixture(t)
	cand, _ := gatedAheadCar(t, f, l, CandidateResult{State: CandidateFailed, Context: "ci / gate (push)"})
	_, err := l.Land(context.Background(), f.work)
	f.assertRejected(t, err, RejectGate, LabelRework)
	if len(cand.calls) != 1 {
		t.Errorf("the candidate gate ran %d time(s), want once: the gate-ahead run only", len(cand.calls))
	}
	assertSlotOneGone(t, f)
}

// TestGateAdoptMovedTargetDiscards is train 4b-2: a car gated on a target tip
// that has since moved is discarded, and the landing merges and gates on the
// new tip as before.
func TestGateAdoptMovedTargetDiscards(t *testing.T) {
	t.Parallel()
	f, l, _ := keyFixture(t)
	cand, out := gatedAheadCar(t, f, l, gatePassed)
	// f.base follows the target, as the merger's race check reads it.
	f.base = f.git.Commit(t, f.origin, "main", "main: move the target", map[string]string{"c.txt": "moved\n"})
	res, err := l.Land(context.Background(), f.work)
	must(t, err)
	assertDiscarded(t, f, cand, out)
	if got := f.originMain(); got != res.LandedCommit {
		t.Errorf("origin main = %s, want the landed %s", shortSHA(got), shortSHA(res.LandedCommit))
	}
}

// TestGateAdoptChangedHeadDiscards is train 4b-2: a car gated for one head is
// discarded when the landing is for another head.
func TestGateAdoptChangedHeadDiscards(t *testing.T) {
	t.Parallel()
	f, l, _ := keyFixture(t)
	// The new head has no stored review, so its om review runs and approves.
	l.Reviewer = probeApprover{&keyReviewer{t: t, version: stubOMVersion}}
	cand, out := gatedAheadCar(t, f, l, gatePassed)
	w := f.work
	w.Head = f.git.Commit(t, f.origin, fixtureBranch, "feat: add b again", map[string]string{"b.txt": "work again\n"})
	_, err := l.Land(context.Background(), w)
	must(t, err)
	assertDiscarded(t, f, cand, out)
}

// TestGateAdoptStoredErrorDiscards is train 4b-2: a car whose stored gate run
// carries an error has its candidate discarded, and the landing gates its own.
func TestGateAdoptStoredErrorDiscards(t *testing.T) {
	t.Parallel()
	f, l, _ := keyFixture(t)
	failed := gatePassed
	failed.Err = errors.New("push: 502 from forgejo")
	cand, out := gatedAheadCar(t, f, l, failed, gatePassed)
	res, err := l.Land(context.Background(), f.work)
	must(t, err)
	assertDiscarded(t, f, cand, out)
	if got := f.originMain(); got != res.LandedCommit {
		t.Errorf("origin main = %s, want the landed %s", shortSHA(got), shortSHA(res.LandedCommit))
	}
}

// TestGateAdoptPatchIDFailureRemovesWorktree is train 4b-2: a patch-id read
// that fails on an adopted car removes its worktree, returns the error and
// leaves the target where it was.
func TestGateAdoptPatchIDFailureRemovesWorktree(t *testing.T) {
	t.Parallel()
	f, l, _ := keyFixture(t)
	open := l.openRepo
	l.openRepo = func(dir string) Repo {
		if dir == filepath.Join(f.workRoot, landWorktreeName+"-1") {
			return patchIDFails{Repo: open(dir)}
		}
		return open(dir)
	}
	cand, _ := gatedAheadCar(t, f, l, gatePassed)
	before := f.originMain()
	_, err := l.Land(context.Background(), f.work)
	var infra *InfraError
	if !errors.As(err, &infra) || infra.Stage != "patch-id" {
		t.Fatalf("Land error = %T %v, want *InfraError at patch-id", err, err)
	}
	assertSlotOneGone(t, f)
	if got := f.originMain(); got != before {
		t.Errorf("origin main = %s, want it unchanged at %s", shortSHA(got), shortSHA(before))
	}
	if len(cand.calls) != 1 {
		t.Errorf("the candidate gate ran %d time(s), want once: the gate-ahead run only", len(cand.calls))
	}
}

// TestGateAdoptIdentityFailureReturnsTheWorktreeError is train 4b-2: a car that
// cannot take a landing identity directory returns the error as a worktree
// failure, the work root being unwritable.
func TestGateAdoptIdentityFailureReturnsTheWorktreeError(t *testing.T) {
	t.Parallel()
	f, l, _ := keyFixture(t)
	ran := make(chan struct{})
	cand := &seqCandidate{fakeCandidate: &fakeCandidate{}, results: []CandidateResult{gatePassed}, ran: ran}
	failed := make(chan struct{})
	l.Candidate, l.Out, l.Ahead2 = cand, buildFailureLog{failed}, &AheadSlot{}
	approveAheadMerge(t, f, l)
	if !l.GateAhead(context.Background(), f.work, f.base) {
		t.Fatal("GateAhead = false, want a car held")
	}
	// The build's writes to the work root are done once its gate run starts;
	// a build that fails first never starts one, and its log line ends the wait.
	select {
	case <-ran:
	case <-failed:
		t.Fatal("the build failed before its gate run, so the work root was never written past it")
	}
	// The work root takes no new entry: read-only, the identity directory cannot
	// be made, and the cleanup restores write so the fixture can be removed.
	//testpolicy:allow no-exec-files — the target is the fixture directory: a read-only dir needs its search bit
	must(t, os.Chmod(f.workRoot, 0o500))
	t.Cleanup(func() {
		//testpolicy:allow no-exec-files — the target is the fixture directory: traversal needs its search bit back
		_ = os.Chmod(f.workRoot, 0o700)
	})
	_, err := l.Land(context.Background(), f.work)
	var infra *InfraError
	if !errors.As(err, &infra) || infra.Stage != "worktree" {
		t.Fatalf("Land error = %T %v, want *InfraError at worktree", err, err)
	}
	if len(cand.calls) != 1 {
		t.Errorf("the candidate gate ran %d time(s), want once: the gate-ahead run only", len(cand.calls))
	}
}
