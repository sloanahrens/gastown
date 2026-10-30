package landworker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}

const (
	noteHead = "1111111111111111111111111111111111111111"
	tipHead  = "2222222222222222222222222222222222222222"
	// seededRowAt is the updated_at a seeded bead's list row carries, the key
	// the worker's outcome memo holds it by (gt-r9pj9).
	seededRowAt = "2026-09-30T11:30:00Z"
)

type fakeRemote struct {
	tips      map[string]string
	contains  map[string]bool // commit -> on target
	tipErr    error
	tipCalls  int
	containsN int

	// The remote branch model the landing cleanup drives: full ref name to
	// tip. beforeDelete, when set, runs before a delete's lease check so a
	// test can move a branch after the listing that saw it.
	remoteRefs   map[string]string
	listErr      error
	deleteErr    map[string]error
	beforeDelete func(branch string)
	deleted      []string
	// listCalls counts ListRemoteRefs calls, which the sweep's interval is
	// read from.
	listCalls int

	// The leftover sweep's questions: which branches are provably on the
	// target, and when their tips were committed.
	landed    map[string]bool
	landedErr error
	times     map[string]time.Time
	timeErr   error

	// The rig repository's own branches, which the landing's local reap
	// drives (gt-kne58): the branches it holds and the tip of each, the ones
	// a worktree has checked out, and what it deleted.
	localBranches map[string]string
	checkedOut    map[string]bool
	localErr      map[string]error
	localTipErr   map[string]error
	// localBeforeDelete, when set, runs before a local delete's pin check so
	// a test can move a branch after the reap read it.
	localBeforeDelete func(branch string)
	localDeleted      []string
}

func (r *fakeRemote) BranchTip(branch string) (string, error) {
	r.tipCalls++
	return r.tips[branch], r.tipErr
}

func (r *fakeRemote) Contains(_, commit string) (bool, error) {
	r.containsN++
	return r.contains[commit], nil
}

func (r *fakeRemote) ListRemoteRefs(prefix string) ([]RemoteRef, error) {
	r.listCalls++
	if r.listErr != nil {
		return nil, r.listErr
	}
	var out []RemoteRef
	for name, hash := range r.remoteRefs {
		if strings.HasPrefix(name, prefix) {
			out = append(out, RemoteRef{Name: name, Hash: hash})
		}
	}
	slices.SortFunc(out, func(a, b RemoteRef) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (r *fakeRemote) BranchLandedOn(_, branch, _ string) (bool, error) {
	if r.landedErr != nil {
		return false, r.landedErr
	}
	return r.landed[branch], nil
}

func (r *fakeRemote) CommitTime(commit string) (time.Time, error) {
	if r.timeErr != nil {
		return time.Time{}, r.timeErr
	}
	at, ok := r.times[commit]
	if !ok {
		return time.Time{}, fmt.Errorf("no commit %s", commit)
	}
	return at, nil
}

func (r *fakeRemote) DeleteRemoteBranchIfAt(branch, expectedHash string) error {
	if err := r.deleteErr[branch]; err != nil {
		return err
	}
	if r.beforeDelete != nil {
		r.beforeDelete(branch)
	}
	name := "refs/heads/" + branch
	if r.remoteRefs[name] != expectedHash {
		return fmt.Errorf("%s moved since it was listed", branch)
	}
	delete(r.remoteRefs, name)
	r.deleted = append(r.deleted, branch)
	return nil
}

// DeleteLocalBranch deletes a branch of the rig's own repository, refusing
// what the worker must: an absent branch is a no-op, and a checked out one an
// error. The delete is pinned at expectedHash as git update-ref -d is, so a
// branch that is not at the commit the reap proved it to be stays (gt-xpmr7).
func (r *fakeRemote) DeleteLocalBranch(branch, expectedHash string) (bool, error) {
	if err := r.localErr[branch]; err != nil {
		return false, err
	}
	if _, ok := r.localBranches[branch]; !ok {
		return false, nil
	}
	if r.checkedOut[branch] {
		return false, fmt.Errorf("%s is checked out in a worktree", branch)
	}
	if r.localBeforeDelete != nil {
		r.localBeforeDelete(branch)
	}
	tip, ok := r.localBranches[branch]
	if !ok {
		return false, nil
	}
	if tip != expectedHash {
		return false, fmt.Errorf("%s is at %s, not at the proven %s", branch, short(tip), short(expectedHash))
	}
	delete(r.localBranches, branch)
	r.localDeleted = append(r.localDeleted, branch)
	return true, nil
}

// LocalBranchTip is the branch the rig's own repository holds under that
// name, empty when it holds none.
func (r *fakeRemote) LocalBranchTip(branch string) (string, error) {
	if err := r.localTipErr[branch]; err != nil {
		return "", err
	}
	return r.localBranches[branch], nil
}

type fakeLander struct {
	calls []land.Work
	fn    func(n int, w land.Work) (land.Result, error)
}

func (l *fakeLander) Land(_ context.Context, w land.Work) (land.Result, error) {
	l.calls = append(l.calls, w)
	if l.fn == nil {
		return land.Result{LandedCommit: "cccccccccc", PatchID: "pppppppppp"}, nil
	}
	return l.fn(len(l.calls), w)
}

type fakeLandings struct {
	recs []land.LandingRecord
	err  error
}

func (f *fakeLandings) LatestForBead(id string) (land.LandingRecord, bool, error) {
	var out land.LandingRecord
	ok := false
	for _, r := range f.recs {
		if r.BeadID == id {
			out, ok = r, true
		}
	}
	return out, ok, f.err
}

func (f *fakeLandings) Recent(n int) ([]land.LandingRecord, error) {
	if len(f.recs) > n {
		return f.recs[len(f.recs)-n:], f.err
	}
	return f.recs, f.err
}

// failingCommentStore is the fake whose comment write fails: reads and
// updates reach the bead, the comment never does.
type failingCommentStore struct {
	*beadsfake.Fake
	err error
}

func (s failingCommentStore) AddComment(string, string) error { return s.err }

type harness struct {
	w         *Worker
	bd        *beadsfake.Fake
	remote    *fakeRemote
	lander    *fakeLander
	files     *fakeLandings
	cleared   []string
	afterLand []string
	now       time.Time
}

const branch = "polecat/opal/gt-abc+x1"

// humanActor is the town's overseer identity the harness hands the worker:
// the name a submission this worker refuses carries.
const humanActor = "Sloan Ahrens"

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		bd:     beadsfake.New(beadsfake.WithPrefix("gt")),
		remote: &fakeRemote{tips: map[string]string{branch: tipHead}, contains: map[string]bool{}},
		lander: &fakeLander{},
		files:  &fakeLandings{},
		now:    time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC),
	}
	h.w = &Worker{
		Rig: "gastown", HumanActor: humanActor, Beads: h.bd, Remote: h.remote, Lander: h.lander, Landings: h.files,
		ClearIntent: func(w land.Work) error { h.cleared = append(h.cleared, w.BeadID+"@"+w.Worker); return nil },
		AfterLanded: func(w land.Work) { h.afterLand = append(h.afterLand, w.BeadID+"@"+w.Worker) },
		Now:         func() time.Time { return h.now },
		Logf:        t.Logf,
	}
	return h
}

func (h *harness) seedReady(t *testing.T, id string) {
	t.Helper()
	w := land.Work{Branch: branch, Head: noteHead, Target: "main", Worker: "opal"}
	h.bd.Seed(beads.Issue{ID: id, Title: "work", Status: "hooked", Type: "task", Assignee: "gastown/polecats/opal",
		Labels: []string{land.LabelReadyToLand}, Notes: land.FormatReadyNote(w), UpdatedAt: seededRowAt})
}

// seedReadyAt seeds a ready bead carrying priority and the submission time
// the landing worker orders by, so a test can build a queue with a known
// order.
func (h *harness) seedReadyAt(t *testing.T, id string, priority int, submitted string) {
	t.Helper()
	is := readyIssue(id, priority, submitted, submitted)
	is.Title, is.Status, is.Type, is.Assignee = "work", "hooked", "task", "gastown/polecats/opal"
	h.bd.Seed(*is)
}

func (h *harness) comments(t *testing.T, id string) []string {
	t.Helper()
	cs, err := h.bd.Comments(id)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, c := range cs {
		out = append(out, c.Text)
	}
	return out
}

func TestPassLandsTheBranchTipNotThePinnedHead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	rep := h.w.Pass(context.Background())
	if rep.Landed != 1 || len(h.lander.calls) != 1 {
		t.Fatalf("report %v, calls %d; want one landing", rep, len(h.lander.calls))
	}
	got := h.lander.calls[0]
	if got.Head != tipHead || got.Branch != branch || got.Target != "main" || got.BeadID != "gt-abc" || got.Rig != "gastown" {
		t.Fatalf("landed %+v; want the tip %s of %s", got, tipHead, branch)
	}
	if len(h.cleared) != 1 || h.cleared[0] != "gt-abc@opal" {
		t.Fatalf("intent cleared for %v; want gt-abc@opal", h.cleared)
	}
	if len(h.afterLand) != 1 || h.afterLand[0] != "gt-abc@opal" {
		t.Fatalf("after-landing hooks %v; want gt-abc@opal once", h.afterLand)
	}
}

// A rejection leaves the seat's work unlanded, so the after-landing hook — the
// daemon's sandbox-seat cleanup — must not run for it (gt-sefzt).
func TestPassRejectionSkipsTheAfterLandedHook(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.lander.fn = func(int, land.Work) (land.Result, error) {
		return land.Result{}, &land.Rejection{Kind: land.RejectGate, Reason: "gate failed on the merged tree", Rework: true}
	}
	if rep := h.w.Pass(context.Background()); rep.Rejected != 1 {
		t.Fatalf("report %v; want one rejection", rep)
	}
	if len(h.afterLand) != 0 {
		t.Fatalf("after-landing hooks %v; a rejection did not land anything", h.afterLand)
	}
}

// A landing whose record is left unfinished has not yet proved itself, so the
// after-landing hook waits. The repair pass that finishes the record runs it,
// once (gt-sefzt).
func TestPassRecordIncompleteRunsTheAfterLandedHookOnRepair(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.lander.fn = func(n int, w land.Work) (land.Result, error) {
		if n == 1 {
			res := land.Result{LandedCommit: "cccccccc"}
			return res, &land.RecordError{Result: res, Err: errors.New("close failed")}
		}
		return land.Result{LandedCommit: "cccccccc"}, nil
	}
	if rep := h.w.Pass(context.Background()); rep.Landed != 1 {
		t.Fatalf("first pass %v; want the landing counted", rep)
	}
	if len(h.afterLand) != 0 {
		t.Fatalf("after-landing hook ran before the record was complete: %v", h.afterLand)
	}
	if err := h.bd.Update("gt-abc", beads.UpdateOptions{RemoveLabels: []string{land.LabelReadyToLand}}); err != nil {
		t.Fatal(err)
	}
	h.remote.tips[branch] = "3333333333333333333333333333333333333333"
	if rep := h.w.Pass(context.Background()); rep.Repaired != 1 {
		t.Fatalf("second pass %v; want one repair", rep)
	}
	if len(h.afterLand) != 1 || h.afterLand[0] != "gt-abc@opal" {
		t.Fatalf("after-landing hooks %v; want gt-abc@opal once, after the repair", h.afterLand)
	}
	h.w.Pass(context.Background())
	if len(h.afterLand) != 1 {
		t.Fatalf("after-landing hooks %v; a finished repair ran the hook again", h.afterLand)
	}
}

func TestPassMissingBranchAnnotatesOnceAndKeepsLabel(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.remote.tips = map[string]string{}
	rep := h.w.Pass(context.Background())
	if len(h.lander.calls) != 0 || rep.Skipped != 1 {
		t.Fatalf("report %v, calls %d; want a skip and no landing", rep, len(h.lander.calls))
	}
	cs := h.comments(t, "gt-abc")
	if len(cs) != 1 || !strings.Contains(cs[0], "not on origin") {
		t.Fatalf("comments %q; want one saying the branch is missing", cs)
	}
	is, _ := h.bd.Show("gt-abc")
	if !beads.HasLabel(is, land.LabelReadyToLand) {
		t.Fatal("the ready label was removed; it must stay for a human")
	}
	// Inside the backoff: not even re-checked.
	h.w.Pass(context.Background())
	if h.remote.tipCalls != 1 {
		t.Fatalf("branch tip read %d times inside the backoff; want 1", h.remote.tipCalls)
	}
	// After it: re-checked, but not re-announced.
	h.now = h.now.Add(humanWaitBackoff + time.Second)
	h.w.Pass(context.Background())
	if h.remote.tipCalls != 2 || len(h.comments(t, "gt-abc")) != 1 {
		t.Fatalf("tip reads %d, comments %d; want a re-check with no second comment", h.remote.tipCalls, len(h.comments(t, "gt-abc")))
	}
}

func TestPassReworkRejectionCommentsAndClearsIntent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.lander.fn = func(int, land.Work) (land.Result, error) {
		return land.Result{}, &land.Rejection{Kind: land.RejectGate, Reason: "gate failed on the merged tree", Rework: true}
	}
	rep := h.w.Pass(context.Background())
	if rep.Rejected != 1 {
		t.Fatalf("report %v; want one rejection", rep)
	}
	cs := h.comments(t, "gt-abc")
	// The rework comment is pinned whole: its wording is what the returning
	// polecat acts on, so a change to it is a deliberate one.
	want := "Landing rejected (gate): gate failed on the merged tree. Back to the polecat as rework: " + ReworkComment + "."
	if len(cs) != 1 || cs[0] != want {
		t.Fatalf("comments %q; want the rework instruction %q", cs, want)
	}
	if len(h.cleared) != 1 {
		t.Fatalf("intent cleared %v; want once", h.cleared)
	}
}

// TestPassHumanRejectionCommentsTheVerdict: a rejection left for a human
// leaves the same verdict, score, summary and findings a rework rejection
// does, and its first sentence says the loop is escalated instead of
// reworked (gt-7nidu).
func TestPassHumanRejectionCommentsTheVerdict(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.lander.fn = func(int, land.Work) (land.Result, error) {
		return land.Result{}, &land.Rejection{Kind: land.RejectReview, Rework: false, Reason: "om requested changes (score 0.41, 2 finding(s))",
			ReviewScore: 0.41, ReviewVerdict: land.VerdictRequestChanges, ReviewSummary: "Retry loop drops the last error.",
			Findings: []land.Finding{{Severity: "major", Path: "a.go", Line: 9, Title: "error dropped"}, {Severity: "minor", Path: "b.go", Title: "naming"}}}
	}
	h.w.Pass(context.Background())
	cs := h.comments(t, "gt-abc")
	if len(cs) != 1 {
		t.Fatalf("comments %q; want exactly one on the work bead", cs)
	}
	first := strings.SplitN(cs[0], "\n", 2)[0]
	if !strings.Contains(first, "escalated to a human") || !strings.Contains(first, "not reworked again") {
		t.Errorf("the first sentence does not say the loop is escalated:\n%s", cs[0])
	}
	for _, want := range []string{"om verdict: request_changes, score 0.41", "Retry loop drops the last error.", "[major] a.go:9 error dropped", "[minor] b.go naming"} {
		if !strings.Contains(cs[0], want) {
			t.Errorf("comment lacks %q:\n%s", want, cs[0])
		}
	}
	if strings.Contains(cs[0], ReworkComment) {
		t.Errorf("a human rejection tells the polecat to rework:\n%s", cs[0])
	}
}

// TestPassHumanRejectionCommentFailureIsLogged: a comment that does not reach
// the store is logged, and the escalation happens anyway (gt-7nidu).
func TestPassHumanRejectionCommentFailureIsLogged(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.w.Beads = failingCommentStore{Fake: h.bd, err: errors.New("bd down")}
	var mu sync.Mutex
	var logs []string
	h.w.Logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	var escalations atomic.Int32
	h.w.Escalate = func(string, string) { escalations.Add(1) }
	h.lander.fn = func(int, land.Work) (land.Result, error) {
		return land.Result{}, &land.Rejection{Kind: land.RejectPolicy, Reason: "no_merge work never lands", Rework: false}
	}
	h.w.Pass(context.Background())
	h.w.escWG.Wait()
	if n := escalations.Load(); n != 1 {
		t.Fatalf("escalated %d times; want 1", n)
	}
	mu.Lock()
	joined := strings.Join(logs, "\n")
	mu.Unlock()
	if !strings.Contains(joined, "adding the human-rejection comment: bd down") {
		t.Fatalf("logs %q; want the comment failure logged", joined)
	}
	if cs := h.comments(t, "gt-abc"); len(cs) != 0 {
		t.Fatalf("comments %q; the fake store refused the write", cs)
	}
}

// TestPassHumanRejectionEscalates: a rejection left for a human is raised to
// the operator once, naming the bead and the reason (gt-is0ep).
func TestPassHumanRejectionEscalates(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	got := make(chan string, 2)
	h.w.Escalate = func(beadID, message string) { got <- beadID + ": " + message }
	h.lander.fn = func(int, land.Work) (land.Result, error) {
		return land.Result{}, &land.Rejection{Kind: land.RejectReview, Reason: "om review returned no verdict", Rework: false}
	}
	h.w.Pass(context.Background())
	// Bounded by the test binary's timeout: a missing escalation hangs here.
	msg := <-got
	if !strings.HasPrefix(msg, "gt-abc: ") || !strings.Contains(msg, "no verdict") || !strings.Contains(msg, "review") {
		t.Fatalf("escalation %q; want the bead, the kind and the reason", msg)
	}
	if !strings.Contains(msg, "left for the overseer") {
		t.Fatalf("escalation %q; want it to say the landing is left for the overseer, as the log line does", msg)
	}
}

// TestPassReworkRejectionDoesNotEscalate: rework goes back to the polecat,
// so the operator is not alerted (gt-j8ade).
func TestPassReworkRejectionDoesNotEscalate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	var escalations atomic.Int32
	h.w.Escalate = func(string, string) { escalations.Add(1) }
	h.lander.fn = func(int, land.Work) (land.Result, error) {
		return land.Result{}, &land.Rejection{Kind: land.RejectGate, Reason: "gate failed", Rework: true}
	}
	if rep := h.w.Pass(context.Background()); rep.Rejected != 1 {
		t.Fatalf("report %v; want one rejection", rep)
	}
	h.w.escWG.Wait()
	if n := escalations.Load(); n != 0 {
		t.Fatalf("escalated %d times for a rework rejection", n)
	}
}

// TestPassHumanRejectionWithRecordErrorEscalates: a rejection left for a
// human is escalated even when writing it to the bead failed; the operator
// hears at once, and the retry after rejectRecordBackoff re-raises it under
// the daemon's per-bead alert key (gt-j8ade).
func TestPassHumanRejectionWithRecordErrorEscalates(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	var escalations atomic.Int32
	h.w.Escalate = func(string, string) { escalations.Add(1) }
	h.lander.fn = func(int, land.Work) (land.Result, error) {
		return land.Result{}, &land.Rejection{Kind: land.RejectReview, Reason: "no verdict", Rework: false, RecordErr: errors.New("bd down")}
	}
	h.w.Pass(context.Background())
	h.w.escWG.Wait()
	if n := escalations.Load(); n != 1 {
		t.Fatalf("escalated %d times; want 1", n)
	}
	if len(h.cleared) != 0 {
		t.Fatalf("intent cleared %v while the rejection is unrecorded", h.cleared)
	}
}

// TestPassEscalatePanicIsRecoveredAndLogged: a broken alert path must not
// take the daemon down (gt-j8ade).
func TestPassEscalatePanicIsRecoveredAndLogged(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	var mu sync.Mutex
	var logs []string
	h.w.Logf = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	h.w.Escalate = func(string, string) { panic("alert path broke") }
	h.lander.fn = func(int, land.Work) (land.Result, error) {
		return land.Result{}, &land.Rejection{Kind: land.RejectPolicy, Reason: "no_merge work never lands", Rework: false}
	}
	h.w.Pass(context.Background())
	h.w.escWG.Wait()
	mu.Lock()
	defer mu.Unlock()
	if !slices.ContainsFunc(logs, func(l string) bool { return strings.Contains(l, "panicked") && strings.Contains(l, "alert path broke") }) {
		t.Fatalf("logs %q; want the recovered panic", logs)
	}
}

func TestPassRejectionWithRecordErrorBacksOff(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.lander.fn = func(int, land.Work) (land.Result, error) {
		return land.Result{}, &land.Rejection{Kind: land.RejectConflict, Reason: "conflict", Rework: true, RecordErr: errors.New("bd down")}
	}
	h.w.Pass(context.Background())
	h.w.Pass(context.Background())
	if len(h.lander.calls) != 1 {
		t.Fatalf("landed %d times; a half-written rejection must back off", len(h.lander.calls))
	}
	if len(h.cleared) != 0 {
		t.Fatalf("intent cleared %v while the bead may still be ready", h.cleared)
	}
}

func TestPassRecordErrorIsRepairedNextPassWithTheSameWork(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.lander.fn = func(n int, w land.Work) (land.Result, error) {
		if n == 1 {
			res := land.Result{LandedCommit: "cccccccc"}
			return res, &land.RecordError{Result: res, Err: errors.New("close failed")}
		}
		return land.Result{LandedCommit: "cccccccc"}, nil
	}
	rep := h.w.Pass(context.Background())
	if rep.Landed != 1 {
		t.Fatalf("first pass %v; want the landing counted", rep)
	}
	// The label is gone now (Land took it off before the close failed), so
	// only the pending repair can bring the bead back.
	if err := h.bd.Update("gt-abc", beads.UpdateOptions{RemoveLabels: []string{land.LabelReadyToLand}}); err != nil {
		t.Fatal(err)
	}
	h.remote.tips[branch] = "3333333333333333333333333333333333333333"
	rep = h.w.Pass(context.Background())
	if rep.Repaired != 1 || len(h.lander.calls) != 2 {
		t.Fatalf("second pass %v, calls %d; want one repair", rep, len(h.lander.calls))
	}
	if h.lander.calls[1].Head != tipHead {
		t.Fatalf("repair landed head %s; want the head that landed, %s", h.lander.calls[1].Head, tipHead)
	}
	h.w.Pass(context.Background())
	if len(h.lander.calls) != 2 {
		t.Fatal("a finished repair was attempted again")
	}
}

func TestPassRecordedLandingOnTargetIsRepairedNotRelanded(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.files.recs = []land.LandingRecord{{BeadID: "gt-abc", Rig: "gastown", Branch: branch, Head: "a932cb5a", Target: "main", LandedCommit: "6db29a35"}}
	h.remote.contains["6db29a35"] = true
	h.w.startupDone = true // exercise the per-bead check, not the startup sweep
	rep := h.w.Pass(context.Background())
	if len(h.lander.calls) != 1 || rep.Repaired != 1 {
		t.Fatalf("report %v, calls %d; want one repair", rep, len(h.lander.calls))
	}
	if got := h.lander.calls[0]; got.Head != "a932cb5a" {
		t.Fatalf("repair passed head %s; want the recorded head so Land finds its record", got.Head)
	}
	if h.remote.tipCalls != 0 {
		t.Fatal("the branch tip was read for a bead that already landed")
	}
}

func TestPassStaleRecordOffTargetLandsNormally(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.files.recs = []land.LandingRecord{{BeadID: "gt-abc", Branch: branch, Head: "old", Target: "main", LandedCommit: "rewound"}}
	h.w.Pass(context.Background())
	if len(h.lander.calls) != 1 || h.lander.calls[0].Head != tipHead {
		t.Fatalf("calls %+v; want a normal landing of the tip", h.lander.calls)
	}
}

func TestPassStartupRepairsOpenBeadsFromTheLandingsFile(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.bd.Seed(beads.Issue{ID: "gt-old", Title: "done", Status: "in_progress", Type: "task", Assignee: "gastown/polecats/ruby"})
	h.bd.Seed(beads.Issue{ID: "gt-closed", Title: "done", Status: "closed", Type: "task"})
	h.files.recs = []land.LandingRecord{
		{BeadID: "gt-closed", Branch: "b1", Head: "h1", Target: "main", LandedCommit: "l1"},
		{BeadID: "gt-old", Rig: "gastown", Branch: "b2", Head: "h2", Target: "main", LandedCommit: "l2"},
	}
	h.remote.contains["l1"], h.remote.contains["l2"] = true, true
	rep := h.w.Pass(context.Background())
	if rep.Repaired != 1 || len(h.lander.calls) != 1 || h.lander.calls[0].BeadID != "gt-old" || h.lander.calls[0].Worker != "ruby" {
		t.Fatalf("report %v calls %+v; want gt-old repaired for ruby", rep, h.lander.calls)
	}
}

func TestPassInfraFailureBacksOffAndAnnouncesOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	cs := newCountingStore()
	h.bd = cs.Fake
	h.w.Beads = cs
	h.seedReady(t, "gt-abc")
	h.lander.fn = func(int, land.Work) (land.Result, error) {
		return land.Result{}, &land.InfraError{Stage: "gate", Err: errors.New("make: not found")}
	}
	for i := 0; i < infraAnnounceAfter+2; i++ {
		h.w.Pass(context.Background())
		cs.reset()
		h.w.Pass(context.Background()) // inside the backoff: not even read
		if n := cs.countOf("Show", "Children", "Comments", "AddComment"); n != 0 {
			t.Fatalf("the pass inside the backoff made %d reads of the bead; want it held on the row it left", n)
		}
		h.now = h.now.Add(infraBackoffMax)
	}
	if len(h.lander.calls) != infraAnnounceAfter+2 {
		t.Fatalf("landed %d times; want one attempt per backoff window", len(h.lander.calls))
	}
	comments := h.comments(t, "gt-abc")
	if len(comments) != 1 || !strings.Contains(comments[0], "infrastructure") {
		t.Fatalf("comments %q; want one infrastructure notice", comments)
	}
	if len(h.cleared) != 0 {
		t.Fatal("intent cleared on an infrastructure failure")
	}
}

func TestPassRaceRetriesNextPassWithoutBackoff(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.lander.fn = func(n int, _ land.Work) (land.Result, error) {
		if n == 1 {
			return land.Result{}, &land.RaceError{Target: "main", Expected: "a", Actual: "b"}
		}
		return land.Result{LandedCommit: "c"}, nil
	}
	h.w.Pass(context.Background())
	rep := h.w.Pass(context.Background())
	if rep.Landed != 1 || len(h.lander.calls) != 2 {
		t.Fatalf("report %v, calls %d; want the second pass to land", rep, len(h.lander.calls))
	}
}

// TestPassRebuildRaceIsNotAFailure: on a cut-over rig the outdated-branch
// guard refusing the merge is the normal rebuild, so it is retried without
// being counted as a failed landing.
func TestPassRebuildRaceIsNotAFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.lander.fn = func(n int, _ land.Work) (land.Result, error) {
		if n == 1 {
			return land.Result{}, &land.RaceError{Target: "main", Rebuild: true}
		}
		return land.Result{LandedCommit: "c"}, nil
	}
	first := h.w.Pass(context.Background())
	if first.Failed != 0 {
		t.Fatalf("report %v; want the rebuild not counted as a failure", first)
	}
	rep := h.w.Pass(context.Background())
	if rep.Landed != 1 || len(h.lander.calls) != 2 {
		t.Fatalf("report %v, calls %d; want the next pass to land", rep, len(h.lander.calls))
	}
}

func TestPassReadBackBacksOffAndAnnounces(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.lander.fn = func(int, land.Work) (land.Result, error) {
		return land.Result{}, &land.InfraError{Stage: "read-back", Err: fmt.Errorf("%w: tip is x", land.ErrReadBack)}
	}
	h.w.Pass(context.Background())
	h.now = h.now.Add(infraBackoffMax - time.Minute)
	h.w.Pass(context.Background())
	if len(h.lander.calls) != 1 {
		t.Fatalf("landed %d times inside the read-back backoff", len(h.lander.calls))
	}
	if cs := h.comments(t, "gt-abc"); len(cs) != 1 || !strings.Contains(cs[0], "read-back") {
		t.Fatalf("comments %q; want one read-back notice", cs)
	}
}

func TestPassFallsBackToTheSubmissionComment(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.bd.Seed(beads.Issue{ID: "gt-abc", Title: "work", Status: "hooked", Type: "task", Assignee: "gastown/polecats/opal", Labels: []string{land.LabelReadyToLand}})
	if err := h.bd.AddComment("gt-abc", "Submitted for landing: polecat/opal/old @ 0123abcd onto main (attempt 1)"); err != nil {
		t.Fatal(err)
	}
	if err := h.bd.AddComment("gt-abc", "Submitted for landing: "+branch+" @ 1111aaaa onto main (attempt 2)"); err != nil {
		t.Fatal(err)
	}
	h.w.Pass(context.Background())
	if len(h.lander.calls) != 1 || h.lander.calls[0].Branch != branch || h.lander.calls[0].Worker != "opal" {
		t.Fatalf("calls %+v; want the latest comment's branch", h.lander.calls)
	}
}

func TestPassWithNoLandingRequestWaitsForHuman(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.bd.Seed(beads.Issue{ID: "gt-abc", Title: "work", Status: "hooked", Type: "task", Labels: []string{land.LabelReadyToLand}})
	rep := h.w.Pass(context.Background())
	if rep.Skipped != 1 || len(h.lander.calls) != 0 || len(h.comments(t, "gt-abc")) != 1 {
		t.Fatalf("report %v, calls %d; want a skip with one annotation", rep, len(h.lander.calls))
	}
}

func TestPassSkipsClosedAndUnlabeled(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.bd.Seed(beads.Issue{ID: "gt-x", Title: "x", Status: "open", Type: "task"})
	h.seedReady(t, "gt-abc")
	if err := h.bd.ForceCloseWithReason("done", "gt-abc"); err != nil {
		t.Fatal(err)
	}
	h.w.Pass(context.Background())
	if len(h.lander.calls) != 0 {
		t.Fatalf("landed %+v; nothing is ready", h.lander.calls)
	}
}

// TestPassLeavesAParkedReadyBead covers the operator's hold on a submission:
// deferring a ready bead parks it, and the worker must not land work the
// health count already reports as 0 pending (gt-y7n1u).
func TestPassLeavesAParkedReadyBead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	deferred := string(beads.StatusDeferred)
	if err := h.bd.Update("gt-abc", beads.UpdateOptions{Status: &deferred}); err != nil {
		t.Fatal(err)
	}
	rep := h.w.Pass(context.Background())
	if len(h.lander.calls) != 0 {
		t.Fatalf("landed a parked bead: %+v", h.lander.calls)
	}
	if len(h.comments(t, "gt-abc")) != 0 {
		t.Errorf("comments %q; a parked bead is left alone, not annotated", h.comments(t, "gt-abc"))
	}
	if rep.Skipped != 0 {
		t.Errorf("report %v; a parked bead is out of the queue, not skipped", rep)
	}
}

func TestPassStopsWhenContextIsDone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.w.Pass(ctx)
	if len(h.lander.calls) != 0 {
		t.Fatal("landed after the context was cancelled")
	}
}

func TestClassify(t *testing.T) {
	t.Parallel()
	bg := context.Background()
	cancelled, cancel := context.WithCancel(bg)
	cancel()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  error
		want outcome
	}{
		{"nil", bg, nil, outLanded},
		{"record", bg, &land.RecordError{Err: errors.New("x")}, outRecordIncomplete},
		{"rework", bg, &land.Rejection{Rework: true}, outRejectedRework},
		{"human", bg, &land.Rejection{Rework: false}, outRejectedHuman},
		{"race", bg, &land.RaceError{}, outRace},
		{"not ready", bg, fmt.Errorf("%w: gone", land.ErrNotReady), outNotReady},
		{"read-back", bg, &land.InfraError{Stage: "read-back", Err: fmt.Errorf("%w: x", land.ErrReadBack)}, outReadBack},
		{"cancelled", cancelled, &land.InfraError{Stage: "gate", Err: context.Canceled}, outCanceled},
		{"infra", bg, &land.InfraError{Stage: "gate", Err: errors.New("boom")}, outInfra},
		{"timeout is infra", bg, &land.InfraError{Stage: "gate", Err: context.DeadlineExceeded}, outInfra},
	} {
		if got := classify(tc.ctx, tc.err); got != tc.want {
			t.Errorf("%s: classify = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// readyIssue is a ready-to-land bead submitted at submitted (RFC 3339; "" for
// a bead submitted before the note carried a time) and last updated at
// updated. A comment moves updated, never submitted.
func readyIssue(id string, priority int, submitted, updated string) *beads.Issue {
	var at time.Time
	if submitted != "" {
		var err error
		if at, err = time.Parse(time.RFC3339, submitted); err != nil {
			panic(err)
		}
	}
	w := land.Work{Branch: branch, Head: noteHead, Target: "main", Worker: "opal", Submitted: at}
	return &beads.Issue{ID: id, Priority: priority, UpdatedAt: updated, Labels: []string{land.LabelReadyToLand}, Notes: land.FormatReadyNote(w)}
}

func order(is []*beads.Issue) string {
	var got []string
	for _, i := range is {
		got = append(got, i.ID)
	}
	return strings.Join(got, ",")
}

// A P1 submitted later lands before a P2 submitted earlier: priority leads,
// so a throughput fix does not wait behind queued P2 work (gt-t2jhf).
func TestSortByLandingOrderPutsPriorityFirst(t *testing.T) {
	t.Parallel()
	is := []*beads.Issue{
		readyIssue("gt-p2", 2, "2026-09-30T10:00:00Z", "2026-09-30T10:00:00Z"),
		readyIssue("gt-p1", 1, "2026-09-30T11:00:00Z", "2026-09-30T11:00:00Z"),
	}
	sortByLandingOrder(is)
	if got := order(is); got != "gt-p1,gt-p2" {
		t.Fatalf("order %s; want the P1 first", got)
	}
}

// A comment on a submitted bead moves its last update, never its place in the
// queue: ordering by last update pushed reviewed work behind work submitted
// after it (gt-t2jhf).
func TestSortByLandingOrderIgnoresComments(t *testing.T) {
	t.Parallel()
	is := []*beads.Issue{
		readyIssue("gt-reviewed", 2, "2026-09-30T10:00:00Z", "2026-09-30T13:00:00Z"),
		readyIssue("gt-unreviewed", 2, "2026-09-30T11:00:00Z", "2026-09-30T11:00:00Z"),
	}
	sortByLandingOrder(is)
	if got := order(is); got != "gt-reviewed,gt-unreviewed" {
		t.Fatalf("order %s; want the comment not to have reordered the queue", got)
	}
}

// A bead submitted before the note carried a time is ordered by its last
// update, the only order it ever had.
func TestSortByLandingOrderFallsBackToTheLastUpdate(t *testing.T) {
	t.Parallel()
	is := []*beads.Issue{
		readyIssue("gt-newer", 2, "", "2026-09-30T12:00:00Z"),
		readyIssue("gt-older", 2, "", "2026-09-30T10:00:00Z"),
	}
	sortByLandingOrder(is)
	if got := order(is); got != "gt-older,gt-newer" {
		t.Fatalf("order %s; want the older bead first", got)
	}
}

func TestSortByLandingOrderBreaksTiesByID(t *testing.T) {
	t.Parallel()
	is := []*beads.Issue{
		readyIssue("gt-c", 2, "2026-09-30T10:00:00Z", "2026-09-30T10:00:00Z"),
		readyIssue("gt-b", 2, "2026-09-30T10:00:00Z", "2026-09-30T10:00:00Z"),
		readyIssue("gt-a", 2, "2026-09-30T10:00:00Z", "2026-09-30T10:00:00Z"),
	}
	sortByLandingOrder(is)
	if got := order(is); got != "gt-a,gt-b,gt-c" {
		t.Fatalf("order %s; want ID order", got)
	}
}

func TestPolecatFromAssignee(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"gastown/polecats/opal": "opal", "gastown/crew/sloan": "", "": "", "opal": ""} {
		if got := polecatFromAssignee(in); got != want {
			t.Errorf("polecatFromAssignee(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPassResubmissionAfterALandingLandsNormally(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	rec := land.LandingRecord{BeadID: "gt-abc", Rig: "gastown", Branch: branch, Head: "oldhead", Target: "main", LandedCommit: "l1", Route: "daemon"}
	notes := rec.NoteBlock() + "\n" + land.FormatReadyNote(land.Work{Branch: branch, Head: noteHead, Target: "main", Worker: "opal"})
	h.bd.Seed(beads.Issue{ID: "gt-abc", Title: "work", Status: "hooked", Type: "task", Assignee: "gastown/polecats/opal",
		Labels: []string{land.LabelReadyToLand}, Notes: notes})
	h.files.recs = []land.LandingRecord{rec}
	h.remote.contains["l1"] = true
	rep := h.w.Pass(context.Background())
	if rep.Landed != 1 || len(h.lander.calls) != 1 || h.lander.calls[0].Head != tipHead {
		t.Fatalf("report %v calls %+v; the new submission must land from its tip, not repair the old landing", rep, h.lander.calls)
	}
}

func TestPassStartupLeavesAReopenedLandedBead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	rec := land.LandingRecord{BeadID: "gt-old", Rig: "gastown", Branch: "b", Head: "h", Target: "main", LandedCommit: "l1", Route: "daemon"}
	// Recorded and label removed, then a human reopened it.
	h.bd.Seed(beads.Issue{ID: "gt-old", Title: "done", Status: "open", Type: "task", Notes: rec.NoteBlock()})
	h.files.recs = []land.LandingRecord{rec, {BeadID: "gt-manual", Branch: "b", Head: "h", Target: "main", LandedCommit: "l2", Route: "overseer-manual"}}
	h.bd.Seed(beads.Issue{ID: "gt-manual", Title: "m", Status: "open", Type: "task"})
	h.remote.contains["l1"], h.remote.contains["l2"] = true, true
	h.w.Pass(context.Background())
	if len(h.lander.calls) != 0 {
		t.Fatalf("startup repair touched %+v; a reopened bead and another route's record are not the worker's", h.lander.calls)
	}
}

func TestPassOMRejectionCommentCarriesTheVerdict(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.lander.fn = func(int, land.Work) (land.Result, error) {
		return land.Result{}, &land.Rejection{Kind: land.RejectReview, Rework: true, Reason: "om requested changes (score 0.41, 2 finding(s))",
			ReviewScore: 0.41, ReviewVerdict: land.VerdictRequestChanges, ReviewSummary: "Retry loop drops the last error.",
			Findings: []land.Finding{{Severity: "major", Path: "a.go", Line: 9, Title: "error dropped"}, {Severity: "minor", Path: "b.go", Title: "naming"}}}
	}
	h.w.Pass(context.Background())
	cs := h.comments(t, "gt-abc")
	if len(cs) != 1 {
		t.Fatalf("comments %q; want exactly one on the work bead", cs)
	}
	for _, want := range []string{ReworkComment, "om verdict: request_changes, score 0.41", "Retry loop drops the last error.", "[major] a.go:9 error dropped", "[minor] b.go naming"} {
		if !strings.Contains(cs[0], want) {
			t.Errorf("comment lacks %q:\n%s", want, cs[0])
		}
	}
}

// TestPassFindingRefusalCommentsTheApproveItRefused: a landing om approved and
// the finding rules refused names the approve, so the author is not told om
// asked for changes it never asked for (gt-oi4je).
func TestPassFindingRefusalCommentsTheApproveItRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.lander.fn = func(int, land.Work) (land.Result, error) {
		return land.Result{}, &land.Rejection{Kind: land.RejectReview, Rework: true,
			Reason:        "a major finding blocks the landing at any score",
			ReviewScore:   0.90,
			ReviewVerdict: land.VerdictApprove,
			ReviewSummary: "Clear change with one real problem.",
			Findings:      []land.Finding{{Severity: "major", Path: "a.go", Line: 9, Title: "error dropped"}}}
	}
	h.w.Pass(context.Background())
	cs := h.comments(t, "gt-abc")
	if len(cs) != 1 {
		t.Fatalf("comments %q; want exactly one on the work bead", cs)
	}
	for _, want := range []string{"a major finding blocks the landing at any score", "om verdict: approve, score 0.90", "Clear change with one real problem.", "[major] a.go:9 error dropped"} {
		if !strings.Contains(cs[0], want) {
			t.Errorf("comment lacks %q:\n%s", want, cs[0])
		}
	}
	if strings.Contains(cs[0], "request_changes") {
		t.Errorf("the comment claims om asked for changes on an approve:\n%s", cs[0])
	}
}

// gt-nxvpe: while the daemon drains for an upgrade restart, a pass claims
// nothing new.
func TestPassDrainingStartsNoLanding(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-a")
	h.w.Draining = func() bool { return true }
	h.w.Pass(context.Background())
	if len(h.lander.calls) != 0 {
		t.Fatalf("landed while draining: %v", h.lander.calls)
	}
	if h.remote.tipCalls != 0 {
		t.Fatalf("watched the target while draining: %d tip reads", h.remote.tipCalls)
	}
}

// The landing in flight finishes; the next ready bead is not claimed.
func TestPassDrainFinishesCurrentLandingOnly(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-a")
	h.seedReady(t, "gt-b")
	draining := false
	h.w.Draining = func() bool { return draining }
	h.lander.fn = func(int, land.Work) (land.Result, error) {
		draining = true // the restart marker appears mid-landing
		return land.Result{LandedCommit: "cccccccccc", PatchID: "pppppppppp"}, nil
	}
	rep := h.w.Pass(context.Background())
	if len(h.lander.calls) != 1 || rep.Landed != 1 {
		t.Fatalf("calls=%d landed=%d, want exactly the in-flight landing", len(h.lander.calls), rep.Landed)
	}
	// After the restart the next pass resumes.
	draining = false
	h.w.Pass(context.Background())
	if len(h.lander.calls) != 2 {
		t.Fatalf("calls=%d, want the second bead landed once draining ends", len(h.lander.calls))
	}
}

// No restart pending: passes land back to back.
func TestPassWithoutDrainLandsEverything(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-a")
	h.seedReady(t, "gt-b")
	h.w.Draining = func() bool { return false }
	h.w.Pass(context.Background())
	if len(h.lander.calls) != 2 {
		t.Fatalf("calls=%d, want 2", len(h.lander.calls))
	}
}

// The queue the worker walks is priority first: the P1 submitted later is
// landed before the earlier P2, so the drain that follows pays for the fix
// that matters first (gt-t2jhf).
func TestPassLandsHigherPriorityFirst(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReadyAt(t, "gt-p2", 2, "2026-09-30T10:00:00Z")
	h.seedReadyAt(t, "gt-p1", 1, "2026-09-30T11:00:00Z")
	rep := h.w.Pass(context.Background())
	if rep.Landed != 2 {
		t.Fatalf("report %v; want both landed", rep)
	}
	var landed []string
	for _, c := range h.lander.calls {
		landed = append(landed, c.BeadID)
	}
	if got := strings.Join(landed, ","); got != "gt-p1,gt-p2" {
		t.Fatalf("landed %s; want the P1 first", got)
	}
}

func TestPassReportsActiveBead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-a")
	var seen []string
	h.w.Active = func(id string) { seen = append(seen, id) }
	h.w.Pass(context.Background())
	if strings.Join(seen, ",") != "gt-a," {
		t.Fatalf("Active calls = %q, want gt-a then cleared", seen)
	}
}

func ciSilenceErr() error {
	return &land.InfraError{Stage: "ci", Err: fmt.Errorf(
		"%w: ci / gate (push) reported nothing on the candidate candidate (land/gt-abc) within its wait window", land.ErrCISilence)}
}

// TestClassifyCISilenceIsInfrastructure: the candidate gate reporting no
// verdict is an infrastructure outcome, never the rework a red CI verdict is.
func TestClassifyCISilenceIsInfrastructure(t *testing.T) {
	t.Parallel()
	if got := classify(context.Background(), ciSilenceErr()); got != outInfra {
		t.Fatalf("classify(CI silence) = %v, want outInfra", got)
	}
	red := &land.Rejection{Kind: land.RejectGate, Rework: true, Reason: "the candidate gate ci / gate (push) failed on the merged tree"}
	if got := classify(context.Background(), red); got != outRejectedRework {
		t.Fatalf("classify(CI red) = %v, want outRejectedRework", got)
	}
}

// TestPassRepeatedCISilencesEscalateOnce: DefaultCISilenceEscalateAfter
// candidate-gate silences in a row raise one escalation naming the bead, and
// further silences stay quiet (gt-fn9e6.5).
func TestPassRepeatedCISilencesEscalateOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	var mu sync.Mutex
	var got []string
	h.w.Escalate = func(beadID, message string) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, beadID+": "+message)
	}
	h.lander.fn = func(int, land.Work) (land.Result, error) { return land.Result{}, ciSilenceErr() }
	attempts := DefaultCISilenceEscalateAfter + 3
	for i := 0; i < attempts; i++ {
		h.w.Pass(context.Background())
		h.w.escWG.Wait()
		mu.Lock()
		n := len(got)
		mu.Unlock()
		want := 0
		if i+1 >= DefaultCISilenceEscalateAfter {
			want = 1
		}
		if n != want {
			t.Fatalf("after %d silences: %d escalations, want %d", i+1, n, want)
		}
		h.now = h.now.Add(infraBackoffMax)
	}
	if len(h.lander.calls) != attempts {
		t.Fatalf("landed %d times; want %d", len(h.lander.calls), attempts)
	}
	if !strings.HasPrefix(got[0], "gt-abc: ") || !strings.Contains(got[0], "candidate gate") {
		t.Fatalf("escalation %q; want the bead and the candidate gate", got[0])
	}
}

// TestPassCISilenceRunIsBrokenByAnotherOutcome: the count is of consecutive
// silences, so a pass that fails another way restarts it.
func TestPassCISilenceRunIsBrokenByAnotherOutcome(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	var escalations atomic.Int32
	h.w.Escalate = func(string, string) { escalations.Add(1) }
	h.w.CISilenceEscalateAfter = 2
	h.lander.fn = func(n int, _ land.Work) (land.Result, error) {
		if n == 2 {
			return land.Result{}, &land.InfraError{Stage: "ci", Err: errors.New("connection refused")}
		}
		return land.Result{}, ciSilenceErr()
	}
	// silence, other failure, silence: never two in a row.
	for i := 0; i < 3; i++ {
		h.w.Pass(context.Background())
		h.now = h.now.Add(infraBackoffMax)
	}
	h.w.escWG.Wait()
	if n := escalations.Load(); n != 0 {
		t.Fatalf("escalated %d times without %d silences in a row", n, h.w.CISilenceEscalateAfter)
	}
}

// TestPassRecordsTheAgentItsSeatRanOn: the landing worker reads the agent the
// polecat's seat was spawned on from its agent bead, which outlives the
// session, and carries it into the landing request so the record names the
// seat the work really cost (gt-mukgk.6).
func TestPassRecordsTheAgentItsSeatRanOn(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.bd.Seed(beads.Issue{ID: "gt-gastown-polecat-opal", Title: "opal", Status: "done", Type: "agent",
		Description: "role_type: polecat\nrig: gastown\nagent_state: done\nhook_bead: null\nagent: claude-haiku"})
	h.w.AgentBeadID = func(polecat string) string { return beads.PolecatBeadID("gastown", polecat) }
	if rep := h.w.Pass(context.Background()); rep.Landed != 1 {
		t.Fatalf("report %v; want one landing", rep)
	}
	if got := h.lander.calls[0].Agent; got != "claude-haiku" {
		t.Errorf("landing agent = %q, want claude-haiku; the seat's agent bead names it", got)
	}
}

// TestPassAsksForTheAgentOnlyWhenItLands: a record repair carries the agent
// its record holds, and a bead whose branch is gone lands nothing, so
// neither reads the seat's agent bead (gt-4atwn).
func TestPassAsksForTheAgentOnlyWhenItLands(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(h *harness)
	}{
		{"repair of a landing on the target", func(h *harness) {
			h.files.recs = []land.LandingRecord{{BeadID: "gt-abc", Rig: "gastown", Branch: branch, Head: "a932cb5a", Target: "main", LandedCommit: "6db29a35"}}
			h.remote.contains["6db29a35"] = true
			h.w.startupDone = true
		}},
		{"branch missing from origin", func(h *harness) {
			h.remote.tips = map[string]string{}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.seedReady(t, "gt-abc")
			tc.setup(h)
			h.w.AgentBeadID = func(polecat string) string {
				t.Errorf("asked for the agent of %q though nothing lands", polecat)
				return ""
			}
			h.w.Pass(context.Background())
		})
	}
}

// TestPassCrewLandingAsksNoSeatForAnAgent:a crew landing has no seat, so the
// worker records no agent and never asks an agent bead for one — the crew's
// own bead is not a polecat seat (gt-mukgk.6).
func TestPassCrewLandingAsksNoSeatForAnAgent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	w := land.Work{Branch: branch, Head: noteHead, Target: "main"}
	h.bd.Seed(beads.Issue{ID: "gt-crew", Title: "work", Status: "hooked", Type: "task", Assignee: "gastown/crew/sloan",
		Labels: []string{land.LabelReadyToLand}, Notes: land.FormatReadyNote(w)})
	// A crew session's gt done records the submission as a comment under the
	// identity the session runs as; the note it writes names no worker.
	if err := h.bd.AddCommentAs("gt-crew", "gastown/crew/sloan", "Submitted for landing: "+branch+" @ "+noteHead+" onto main"); err != nil {
		t.Fatal(err)
	}
	h.w.AgentBeadID = func(polecat string) string {
		t.Errorf("asked for the agent of a crew landing's worker %q", polecat)
		return ""
	}
	if rep := h.w.Pass(context.Background()); rep.Landed != 1 {
		t.Fatalf("report %v; want one landing", rep)
	}
	if got := h.lander.calls[0].Agent; got != "" {
		t.Errorf("crew landing recorded agent %q, want none", got)
	}
}
