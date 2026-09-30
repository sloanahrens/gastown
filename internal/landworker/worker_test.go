package landworker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
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
)

type fakeRemote struct {
	tips      map[string]string
	contains  map[string]bool // commit -> on target
	tipErr    error
	tipCalls  int
	containsN int
}

func (r *fakeRemote) BranchTip(branch string) (string, error) {
	r.tipCalls++
	return r.tips[branch], r.tipErr
}

func (r *fakeRemote) Contains(_, commit string) (bool, error) {
	r.containsN++
	return r.contains[commit], nil
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

type harness struct {
	w       *Worker
	bd      *beadsfake.Fake
	remote  *fakeRemote
	lander  *fakeLander
	files   *fakeLandings
	cleared []string
	now     time.Time
}

const branch = "polecat/opal/gt-abc+x1"

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
		Rig: "gastown", Beads: h.bd, Remote: h.remote, Lander: h.lander, Landings: h.files,
		ClearIntent: func(w land.Work) error { h.cleared = append(h.cleared, w.BeadID+"@"+w.Worker); return nil },
		Now:         func() time.Time { return h.now },
		Logf:        t.Logf,
	}
	return h
}

func (h *harness) seedReady(t *testing.T, id string) {
	t.Helper()
	w := land.Work{Branch: branch, Head: noteHead, Target: "main", Worker: "opal"}
	h.bd.Seed(beads.Issue{ID: id, Title: "work", Status: "hooked", Type: "task", Assignee: "gastown/polecats/opal",
		Labels: []string{land.LabelReadyToLand}, Notes: land.FormatReadyNote(w)})
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
}

func TestPassMissingBranchAnnotatesOnceAndKeepsLabel(t *testing.T) {
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
	if len(cs) != 1 || !strings.Contains(cs[0], ReworkComment) || !strings.Contains(cs[0], "gate") {
		t.Fatalf("comments %q; want the rework instruction", cs)
	}
	if len(h.cleared) != 1 {
		t.Fatalf("intent cleared %v; want once", h.cleared)
	}
}

func TestPassHumanRejectionLeavesNoReworkComment(t *testing.T) {
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.lander.fn = func(int, land.Work) (land.Result, error) {
		return land.Result{}, &land.Rejection{Kind: land.RejectPolicy, Reason: "no_merge work never lands", Rework: false}
	}
	rep := h.w.Pass(context.Background())
	if rep.Rejected != 1 || len(h.comments(t, "gt-abc")) != 0 {
		t.Fatalf("report %v, comments %q; want a rejection with no rework comment", rep, h.comments(t, "gt-abc"))
	}
}

func TestPassRejectionWithRecordErrorBacksOff(t *testing.T) {
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
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.files.recs = []land.LandingRecord{{BeadID: "gt-abc", Branch: branch, Head: "old", Target: "main", LandedCommit: "rewound"}}
	h.w.Pass(context.Background())
	if len(h.lander.calls) != 1 || h.lander.calls[0].Head != tipHead {
		t.Fatalf("calls %+v; want a normal landing of the tip", h.lander.calls)
	}
}

func TestPassStartupRepairsOpenBeadsFromTheLandingsFile(t *testing.T) {
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
	h := newHarness(t)
	h.seedReady(t, "gt-abc")
	h.lander.fn = func(int, land.Work) (land.Result, error) {
		return land.Result{}, &land.InfraError{Stage: "gate", Err: errors.New("make: not found")}
	}
	for i := 0; i < infraAnnounceAfter+2; i++ {
		h.w.Pass(context.Background())
		h.w.Pass(context.Background()) // inside the backoff: skipped
		h.now = h.now.Add(infraBackoffMax)
	}
	if len(h.lander.calls) != infraAnnounceAfter+2 {
		t.Fatalf("landed %d times; want one attempt per backoff window", len(h.lander.calls))
	}
	cs := h.comments(t, "gt-abc")
	if len(cs) != 1 || !strings.Contains(cs[0], "infrastructure") {
		t.Fatalf("comments %q; want one infrastructure notice", cs)
	}
	if len(h.cleared) != 0 {
		t.Fatal("intent cleared on an infrastructure failure")
	}
}

func TestPassRaceRetriesNextPassWithoutBackoff(t *testing.T) {
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

func TestPassReadBackBacksOffAndAnnounces(t *testing.T) {
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
	h := newHarness(t)
	h.bd.Seed(beads.Issue{ID: "gt-abc", Title: "work", Status: "hooked", Type: "task", Labels: []string{land.LabelReadyToLand}})
	rep := h.w.Pass(context.Background())
	if rep.Skipped != 1 || len(h.lander.calls) != 0 || len(h.comments(t, "gt-abc")) != 1 {
		t.Fatalf("report %v, calls %d; want a skip with one annotation", rep, len(h.lander.calls))
	}
}

func TestPassSkipsClosedAndUnlabeled(t *testing.T) {
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

func TestPassStopsWhenContextIsDone(t *testing.T) {
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

func TestSortOldestFirst(t *testing.T) {
	is := []*beads.Issue{
		{ID: "gt-c", UpdatedAt: "2026-09-30T12:00:00Z"},
		{ID: "gt-b", UpdatedAt: "2026-09-30T10:00:00Z"},
		{ID: "gt-a", UpdatedAt: "2026-09-30T12:00:00Z"},
	}
	sortOldestFirst(is)
	var got []string
	for _, i := range is {
		got = append(got, i.ID)
	}
	if strings.Join(got, ",") != "gt-b,gt-a,gt-c" {
		t.Fatalf("order %v", got)
	}
}

func TestPolecatFromAssignee(t *testing.T) {
	for in, want := range map[string]string{"gastown/polecats/opal": "opal", "gastown/crew/sloan": "", "": "", "opal": ""} {
		if got := polecatFromAssignee(in); got != want {
			t.Errorf("polecatFromAssignee(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPassResubmissionAfterALandingLandsNormally(t *testing.T) {
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
