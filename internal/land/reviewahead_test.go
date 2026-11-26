package land

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/testutil"
)

// aheadReadyIssue is the bead a landing of w finds ready: actionable, labelled
// for landing, with no policy refusal and no overseer waiver.
func aheadReadyIssue(w Work) beads.Issue {
	return beads.Issue{ID: w.BeadID, Title: "add b", Status: "hooked", Type: "task", Assignee: "gastown/polecats/opal",
		Labels: []string{LabelReadyToLand}, Notes: FormatReadyNote(w)}
}

// aheadSeedBead replaces the fixture's bead store with one holding issue, the
// only bead ReviewAhead reads.
func aheadSeedBead(f *landFixture, issue beads.Issue) {
	f.t.Helper()
	f.bd = beadsfake.New(beadsfake.WithPrefix("gt"))
	f.bd.Seed(issue)
}

// aheadBeadWrites counts the writes the bead store has journaled. The journal
// holds every create, update, close, note and comment the Beads interface can
// make, so a write that ReviewAhead made would move the count.
func aheadBeadWrites(t *testing.T, bd *beadsfake.Fake) int {
	t.Helper()
	page, err := bd.EventsTail(0, 0)
	if err != nil {
		t.Fatalf("reading the bead journal: %v", err)
	}
	return len(page.Records)
}

func TestVerdictReuseAheadSkips(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name  string
		store bool // whether the lander has a review-ahead store
		issue func(w Work) beads.Issue
		want  AheadOutcome
		// clockReads is how many times ReviewAhead reads the store's clock: the
		// review's start is taken once it proceeds past the skips, and its verdict
		// is dated when the review returns; a skip reads neither.
		clockReads int
	}{
		{"nil Ahead", false, aheadReadyIssue, AheadSkipped, 0},
		{"bead not actionable", true, func(w Work) beads.Issue {
			i := aheadReadyIssue(w)
			i.Status = "closed"
			return i
		}, AheadSkipped, 0},
		{"bead lacks ready label", true, func(w Work) beads.Issue {
			i := aheadReadyIssue(w)
			i.Labels = nil
			return i
		}, AheadSkipped, 0},
		{"policy refusal", true, func(w Work) beads.Issue {
			i := aheadReadyIssue(w)
			i.AcceptanceCriteria = "- [ ] add b"
			return i
		}, AheadSkipped, 0},
		{"waived head", true, func(w Work) beads.Issue {
			i := aheadReadyIssue(w)
			i.Labels = append(i.Labels, LabelOverseerReviewed)
			i.Notes = FormatReadyNote(w) + "\n" + FormatOverseerReviewedNote(w.Head)
			return i
		}, AheadSkipped, 0},
		// The healthy path: past every guard, through both fetches, to the
		// review the isolated reviewer makes and the record it stores.
		{"ready submission", true, aheadReadyIssue, AheadStored, 2},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, l, _ := aheadKeyFixture(t)
			aheadSeedBead(f, tc.issue(f.work))
			// aheadSeedBead replaces the store the fixture's lander was built with.
			l.Beads = f.bd
			reads := 0
			if tc.store {
				l.Ahead = TownAheadStore(f.town, "gastown")
				l.Ahead.Now = func() time.Time {
					reads++
					return time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
				}
			} else {
				// With no store the bead must not be read: a read that fails
				// would surface as an error, so reading it is observable.
				f.bd.FailWith("show", errors.New("bd show: read with the feature off"))
			}
			before := aheadBeadWrites(t, f.bd)
			got, err := l.ReviewAhead(context.Background(), f.work)
			if err != nil {
				t.Fatalf("ReviewAhead: unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("ReviewAhead = %q, want %q", got, tc.want)
			}
			if n := aheadBeadWrites(t, f.bd) - before; n != 0 {
				t.Errorf("ReviewAhead wrote %d bead records, want 0", n)
			}
			if reads != tc.clockReads {
				t.Errorf("ReviewAhead read the store clock %d times, want %d", reads, tc.clockReads)
			}
			if !l.gitMu.TryLock() {
				t.Errorf("gitMu is still held after ReviewAhead returned")
			} else {
				l.gitMu.Unlock()
			}
		})
	}
}

// aheadFetchRepo is the fixture's clone with a seam over its fetches: it can
// fail the fetch a row names and reports when a fetch runs without the
// lander's git lock held.
type aheadFetchRepo struct {
	Repo
	t    *testing.T
	l    *Lander
	fail func(refspec string) error // nil means no injected failure
}

func (r *aheadFetchRepo) FetchRefspecWithTimeout(remote, refspec string, timeout time.Duration) error {
	// TryLock succeeding from inside the fetch means ReviewAhead holds nothing:
	// the fetch read is not serialized against the landing's git.
	if r.l.gitMu.TryLock() {
		r.l.gitMu.Unlock()
		r.t.Errorf("fetch %s ran without the shared git lock", refspec)
	}
	if r.fail != nil {
		if err := r.fail(refspec); err != nil {
			return err
		}
	}
	return r.Repo.FetchRefspecWithTimeout(remote, refspec, timeout)
}

func TestVerdictReuseAheadFetchFails(t *testing.T) {
	t.Parallel()
	targetErr := errors.New("fatal: couldn't find remote ref refs/heads/main")
	branchErr := errors.New("fatal: could not read Username for 'origin'")
	rows := []struct {
		name      string
		fail      func(refspec string) error
		want      error
		wantStage string
	}{
		{"target fetch fails", func(string) error { return targetErr }, targetErr, "fetch target"},
		{"branch fetch fails", func(refspec string) error {
			if strings.Contains(refspec, fixtureBranch) {
				return branchErr
			}
			return nil
		}, branchErr, "fetch branch"},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			aheadSeedBead(f, aheadReadyIssue(f.work))
			l := f.lander()
			l.Ahead = TownAheadStore(f.town, "gastown")
			l.openRepo = func(dir string) Repo {
				return &aheadFetchRepo{Repo: f.git.Open(dir), t: t, l: l, fail: tc.fail}
			}
			got, err := l.ReviewAhead(context.Background(), f.work)
			if err == nil {
				t.Fatalf("ReviewAhead: got outcome %q and no error, want an error", got)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("ReviewAhead error = %v, want it to wrap %v", err, tc.want)
			}
			// The stage says which of the two fetches failed, so a row cannot
			// pass on the other fetch's error.
			var infra *InfraError
			if !errors.As(err, &infra) {
				t.Fatalf("ReviewAhead error = %v, want an *InfraError", err)
			}
			if infra.Stage != tc.wantStage {
				t.Errorf("error stage = %q, want %q", infra.Stage, tc.wantStage)
			}
			if got != "" {
				t.Errorf("ReviewAhead outcome = %q on error, want empty", got)
			}
			if !l.gitMu.TryLock() {
				t.Errorf("gitMu is still held after ReviewAhead returned an error")
			} else {
				l.gitMu.Unlock()
			}
		})
	}
}

// TestVerdictReuseAheadHeadNotPushed: a branch the seat never pushed has no
// head to review. checkHeadPushed reports that as a rejection with no error;
// the pool writes no bead, so ReviewAhead reports it as an error and the serial
// landing makes the real rejection.
func TestVerdictReuseAheadHeadNotPushed(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	aheadSeedBead(f, aheadReadyIssue(f.work))
	l := f.lander()
	l.Ahead = TownAheadStore(f.town, "gastown")
	f.git.DeleteRef(t, f.origin, "refs/heads/"+fixtureBranch)
	got, err := l.ReviewAhead(context.Background(), f.work)
	if err == nil {
		t.Fatalf("ReviewAhead: got outcome %q and no error, want the not-pushed rejection as an error", got)
	}
	var rej *Rejection
	if !errors.As(err, &rej) {
		t.Fatalf("ReviewAhead error = %v, want a *Rejection", err)
	}
	if rej.Kind != RejectNotPushed {
		t.Errorf("rejection kind = %q, want %q", rej.Kind, RejectNotPushed)
	}
	if got != "" {
		t.Errorf("ReviewAhead outcome = %q on error, want empty", got)
	}
	if !l.gitMu.TryLock() {
		t.Errorf("gitMu is still held after ReviewAhead returned an error")
	} else {
		l.gitMu.Unlock()
	}
}

// aheadNilShowBeads is a bead store whose Show answers no bead and no error,
// the shape an unexpected store answer takes.
type aheadNilShowBeads struct {
	*beadsfake.Fake
}

func (aheadNilShowBeads) Show(string) (*beads.Issue, error) { return nil, nil }

func TestVerdictReuseAheadShowFails(t *testing.T) {
	t.Parallel()
	errShow := errors.New("bd show: store unavailable")
	rows := []struct {
		name    string
		beads   func(f *landFixture) Beads
		wantErr error // nil means any error
	}{
		{"Show returns an error", func(f *landFixture) Beads {
			f.bd.FailWith("show", errShow)
			return f.bd
		}, errShow},
		{"Show returns no bead", func(f *landFixture) Beads {
			return aheadNilShowBeads{f.bd}
		}, nil},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			aheadSeedBead(f, aheadReadyIssue(f.work))
			l := f.lander()
			l.Ahead = TownAheadStore(f.town, "gastown")
			l.Beads = tc.beads(f)
			got, err := l.ReviewAhead(context.Background(), f.work)
			if err == nil {
				t.Fatalf("ReviewAhead: got outcome %q and no error, want an error", got)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("ReviewAhead error = %v, want it to wrap %v", err, tc.wantErr)
			}
			if got != "" {
				t.Errorf("ReviewAhead outcome = %q on error, want empty", got)
			}
		})
	}
}

// aheadWorktreeAdd is one worktree add: the directory it was made at and the
// ref it was cut from.
type aheadWorktreeAdd struct {
	path, ref string
}

// aheadWorktreeAdds is the shared record of the worktree adds ReviewAhead
// made. A recorder is made per openRepo call, so the record is held by pointer.
type aheadWorktreeAdds struct {
	adds []aheadWorktreeAdd
}

// aheadWorktreeRecorder is the fixture's clone with a seam over its worktree
// adds: it records each add, so a test can name the review worktree, see what
// it was cut from, and see that the landing's own wt was never its target.
type aheadWorktreeRecorder struct {
	Repo
	rec *aheadWorktreeAdds
}

func (r *aheadWorktreeRecorder) WorktreeAddDetached(path, ref string) error {
	r.rec.adds = append(r.rec.adds, aheadWorktreeAdd{path: path, ref: ref})
	return r.Repo.WorktreeAddDetached(path, ref)
}

// TestVerdictReuseAheadWorktreeRemoved: a review that reaches the worktree
// step and returns makes its directory under WorkRoot, removes it before it
// returns, and never adds a worktree at the landing's own WorkRoot/wt.
func TestVerdictReuseAheadWorktreeRemoved(t *testing.T) {
	t.Parallel()
	f, l, _ := aheadKeyFixture(t)
	l.Ahead = TownAheadStore(f.town, "gastown")
	rec := &aheadWorktreeAdds{}
	l.openRepo = func(dir string) Repo {
		return &aheadWorktreeRecorder{Repo: f.git.Open(dir), rec: rec}
	}
	got, err := l.ReviewAhead(context.Background(), f.work)
	if err != nil {
		t.Fatalf("ReviewAhead: unexpected error: %v", err)
	}
	if got != AheadStored {
		t.Errorf("ReviewAhead = %q, want %q", got, AheadStored)
	}
	if len(rec.adds) != 1 {
		t.Fatalf("worktree adds = %q, want exactly one", rec.adds)
	}
	dir := rec.adds[0].path
	review := filepath.Dir(dir)
	if filepath.Base(dir) != landWorktreeName || filepath.Dir(review) != f.workRoot || !strings.HasPrefix(filepath.Base(review), reviewDirPrefix) {
		t.Fatalf("worktree added at %s, want %s/%s*/%s", dir, f.workRoot, reviewDirPrefix, landWorktreeName)
	}
	if _, err := os.Stat(review); !os.IsNotExist(err) {
		t.Errorf("review directory %s left behind after ReviewAhead returned (stat err %v)", review, err)
	}
	if _, err := os.Stat(filepath.Join(f.workRoot, landWorktreeName)); !os.IsNotExist(err) {
		t.Errorf("ReviewAhead touched the landing's worktree %s (stat err %v)", filepath.Join(f.workRoot, landWorktreeName), err)
	}
}

// TestVerdictReuseAheadWorktreeAtBase: the review worktree is cut from the
// target tip the fetch brought in — the base the review merges onto — not
// from the head under review.
func TestVerdictReuseAheadWorktreeAtBase(t *testing.T) {
	t.Parallel()
	f, l, _ := aheadKeyFixture(t)
	l.Ahead = TownAheadStore(f.town, "gastown")
	rec := &aheadWorktreeAdds{}
	l.openRepo = func(dir string) Repo {
		return &aheadWorktreeRecorder{Repo: f.git.Open(dir), rec: rec}
	}
	if _, err := l.ReviewAhead(context.Background(), f.work); err != nil {
		t.Fatalf("ReviewAhead: unexpected error: %v", err)
	}
	if len(rec.adds) != 1 {
		t.Fatalf("worktree adds = %q, want exactly one", rec.adds)
	}
	if got := rec.adds[0].ref; got != f.base {
		t.Errorf("review worktree cut at %s, want the target tip %s", got, f.base)
	}
}

// aheadLockProbe is the worktree fake addReviewWorktree is given: a Repo that
// answers the head's author and checks that the lander's gitMu is free at that
// first call, before addReviewWorktree takes the lock for the worktree calls
// (reviewworktree.go). TryLock succeeds and the probe releases it again. A
// ReviewAhead that still held the lock across the worktree step, as one that
// had not released it after its fetches would, is holding it when the call
// arrives and fails the test.
type aheadLockProbe struct {
	Repo
	t      *testing.T
	l      *Lander
	called bool
}

func (p *aheadLockProbe) CommitIdentity(string) (string, string, error) {
	p.called = true
	if !p.l.gitMu.TryLock() {
		// Fatalf, not Errorf: the caller is about to take the lock itself, so
		// the test would deadlock before an Errorf could be reported.
		p.t.Fatalf("the worktree fake was called with the shared git lock held, so addReviewWorktree would deadlock on it")
	}
	p.l.gitMu.Unlock()
	return "Seat Opal", "opal@gastown.local", nil
}

// TestVerdictReuseAheadLockFreeAtWorktreeAdd: ReviewAhead's fetches take the
// shared git lock and release it, so the worktree step that takes the lock
// itself finds it free.
func TestVerdictReuseAheadLockFreeAtWorktreeAdd(t *testing.T) {
	t.Parallel()
	f, l, _ := aheadKeyFixture(t)
	l.Ahead = TownAheadStore(f.town, "gastown")
	// gitfake records no authors, so the probe answers the author read the way
	// the fixture's seam does, and the read reaches the handle.
	l.authorOf = nil
	probe := &aheadLockProbe{t: t, l: l}
	l.openRepo = func(dir string) Repo {
		probe.Repo = f.git.Open(dir)
		return probe
	}
	got, err := l.ReviewAhead(context.Background(), f.work)
	if err != nil {
		t.Fatalf("ReviewAhead: unexpected error: %v", err)
	}
	if got != AheadStored {
		t.Errorf("ReviewAhead = %q, want %q", got, AheadStored)
	}
	if !probe.called {
		t.Fatal("ReviewAhead called no worktree fake, so the lock was never observed at the worktree step")
	}
	if !l.gitMu.TryLock() {
		t.Errorf("gitMu is still held after ReviewAhead returned")
	} else {
		l.gitMu.Unlock()
	}
}

// aheadRevFails is a Repo whose Rev fails for the refs holding needle, as git
// does for a ref the fetch did not write.
type aheadRevFails struct {
	Repo
	needle string
	err    error
}

func (r aheadRevFails) Rev(ref string) (string, error) {
	if strings.Contains(ref, r.needle) {
		return "", r.err
	}
	return r.Repo.Rev(ref)
}

// TestVerdictReuseAheadResolveTargetFails: the base is a git read, and a read
// that fails is returned as an error rather than reviewed at an unknown base.
func TestVerdictReuseAheadResolveTargetFails(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	aheadSeedBead(f, aheadReadyIssue(f.work))
	l := f.lander()
	l.Ahead = TownAheadStore(f.town, "gastown")
	revErr := errors.New("fatal: bad object origin/main")
	l.openRepo = func(dir string) Repo {
		return aheadRevFails{Repo: f.git.Open(dir), needle: "origin/main", err: revErr}
	}
	got, err := l.ReviewAhead(context.Background(), f.work)
	if err == nil {
		t.Fatalf("ReviewAhead: got outcome %q and no error, want the resolve error", got)
	}
	if !errors.Is(err, revErr) {
		t.Errorf("ReviewAhead error = %v, want it to wrap %v", err, revErr)
	}
	var infra *InfraError
	if !errors.As(err, &infra) {
		t.Fatalf("ReviewAhead error = %v, want an *InfraError", err)
	}
	if infra.Stage != "resolve target" {
		t.Errorf("error stage = %q, want %q", infra.Stage, "resolve target")
	}
	if got != "" {
		t.Errorf("ReviewAhead outcome = %q on error, want empty", got)
	}
	if !l.gitMu.TryLock() {
		t.Errorf("gitMu is still held after ReviewAhead returned an error")
	} else {
		l.gitMu.Unlock()
	}
}

// TestVerdictReuseAheadWorktreeAddFails: an add that fails is returned as an
// error, and the review directory it made is not left under WorkRoot.
func TestVerdictReuseAheadWorktreeAddFails(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	aheadSeedBead(f, aheadReadyIssue(f.work))
	l := f.lander()
	l.Ahead = TownAheadStore(f.town, "gastown")
	l.openRepo = func(dir string) Repo { return addFails{Repo: f.git.Open(dir)} }
	got, err := l.ReviewAhead(context.Background(), f.work)
	if err == nil || !strings.Contains(err.Error(), "invalid reference") {
		t.Fatalf("ReviewAhead with a failed worktree add = %q, %v; want the add's error", got, err)
	}
	var infra *InfraError
	if !errors.As(err, &infra) {
		t.Fatalf("ReviewAhead error = %v, want an *InfraError", err)
	}
	if infra.Stage != "review worktree" {
		t.Errorf("error stage = %q, want %q", infra.Stage, "review worktree")
	}
	if got != "" {
		t.Errorf("ReviewAhead outcome = %q on error, want empty", got)
	}
	if entries, err := os.ReadDir(f.workRoot); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), reviewDirPrefix) {
				t.Errorf("review directory %s left behind after a failed add", e.Name())
			}
		}
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if !l.gitMu.TryLock() {
		t.Errorf("gitMu is still held after ReviewAhead returned an error")
	} else {
		l.gitMu.Unlock()
	}
}

// aheadConflictMain rewrites b.txt on origin/main, the file the fixture's
// branch adds, so the head and the new target tip conflict when merged. It
// returns the new target tip, the base the review merges onto.
func aheadConflictMain(f *landFixture) string {
	f.t.Helper()
	return f.pushMain("b.txt", "target wins\n")
}

// aheadNoReviewDir fails the test when a review directory is left under the
// fixture's WorkRoot.
func aheadNoReviewDir(t *testing.T, f *landFixture) {
	t.Helper()
	entries, err := os.ReadDir(f.workRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), reviewDirPrefix) {
			t.Errorf("review directory %s left behind after ReviewAhead returned", e.Name())
		}
	}
}

// TestVerdictReuseAheadConflictRecorded: a head that does not merge onto the
// target tip stores a conflict record — no verdict — and ReviewAhead reports
// AheadConflict. The record is dated by the review's start and its read of the
// clock when the merge came back, and the pool writes no bead.
func TestVerdictReuseAheadConflictRecorded(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	aheadSeedBead(f, aheadReadyIssue(f.work))
	l := f.lander()
	store := TownAheadStore(f.town, "gastown")
	t0 := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	reads := 0
	store.Now = func() time.Time {
		reads++
		return t0.Add(time.Duration(reads) * time.Minute)
	}
	l.Ahead = store
	base := aheadConflictMain(f)
	before := aheadBeadWrites(t, f.bd)
	got, err := l.ReviewAhead(context.Background(), f.work)
	if err != nil {
		t.Fatalf("ReviewAhead: unexpected error: %v", err)
	}
	if got != AheadConflict {
		t.Errorf("ReviewAhead = %q, want %q", got, AheadConflict)
	}
	// One read for started_at, one for reviewed_at: a record that dated both
	// from the same read would pass every field check below but the timing.
	if reads != 2 {
		t.Errorf("ReviewAhead read the store clock %d times, want 2 (start, then reviewed_at)", reads)
	}
	data, err := os.ReadFile(aheadTestPath(f.town, "gastown", f.work.BeadID))
	if err != nil {
		t.Fatalf("reading the conflict record: %v", err)
	}
	var rec AheadRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("the conflict record is not an AheadRecord: %v\n%s", err, data)
	}
	if rec.Outcome != "conflict" || rec.Source != "pool" {
		t.Errorf("record outcome/source = %q/%q, want conflict/pool", rec.Outcome, rec.Source)
	}
	if rec.Bead != f.work.BeadID || rec.Rig != "gastown" {
		t.Errorf("record bead/rig = %q/%q, want %q/gastown", rec.Bead, rec.Rig, f.work.BeadID)
	}
	if rec.Head != f.work.Head {
		t.Errorf("record head = %s, want the conflicting head %s", rec.Head, f.work.Head)
	}
	if rec.Base != base {
		t.Errorf("record base = %s, want the target tip %s", rec.Base, base)
	}
	if rec.Verdict.Verdict != "" {
		t.Errorf("record verdict = %q, want none on a conflict", rec.Verdict.Verdict)
	}
	if want := t0.Add(time.Minute); !rec.StartedAt.Equal(want) {
		t.Errorf("record started_at = %s, want the review's start %s", rec.StartedAt, want)
	}
	if want := t0.Add(2 * time.Minute); !rec.ReviewedAt.Equal(want) {
		t.Errorf("record reviewed_at = %s, want the merge's return %s", rec.ReviewedAt, want)
	}
	if n := aheadBeadWrites(t, f.bd) - before; n != 0 {
		t.Errorf("ReviewAhead wrote %d bead records, want 0", n)
	}
	aheadNoReviewDir(t, f)
	if !l.gitMu.TryLock() {
		t.Errorf("gitMu is still held after ReviewAhead returned")
	} else {
		l.gitMu.Unlock()
	}
}

// aheadMergeFails is the review worktree with a seamed merge: it fails both
// merge shapes with err and, carrying the fixture Repo underneath, reports no
// conflicting file, the shape of a merge that fails for another reason.
type aheadMergeFails struct {
	Repo
	err error
}

func (r aheadMergeFails) MergeNoFF(string, string) error   { return r.err }
func (r aheadMergeFails) MergeSquash(string, string) error { return r.err }

// TestVerdictReuseAheadMergeErrorReturned: a merge that fails for a reason
// other than a conflict is returned as the error it is, with no record stored:
// a review that never happened must not read as one that did.
func TestVerdictReuseAheadMergeErrorReturned(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	aheadSeedBead(f, aheadReadyIssue(f.work))
	l := f.lander()
	l.Ahead = TownAheadStore(f.town, "gastown")
	mergeErr := errors.New("fatal: refusing to merge unrelated histories")
	l.openRepo = func(dir string) Repo {
		return aheadMergeFails{Repo: f.git.Open(dir), err: mergeErr}
	}
	before := aheadBeadWrites(t, f.bd)
	got, err := l.ReviewAhead(context.Background(), f.work)
	if err == nil {
		t.Fatalf("ReviewAhead: got outcome %q and no error, want the merge error", got)
	}
	if !errors.Is(err, mergeErr) {
		t.Errorf("ReviewAhead error = %v, want it to wrap %v", err, mergeErr)
	}
	var infra *InfraError
	if !errors.As(err, &infra) {
		t.Fatalf("ReviewAhead error = %v, want an *InfraError", err)
	}
	if infra.Stage != "merge" {
		t.Errorf("error stage = %q, want %q", infra.Stage, "merge")
	}
	if got != "" {
		t.Errorf("ReviewAhead outcome = %q on error, want empty", got)
	}
	if _, err := os.Stat(aheadTestPath(f.town, "gastown", f.work.BeadID)); !os.IsNotExist(err) {
		t.Errorf("a record was stored for a merge that failed (stat err %v)", err)
	}
	if n := aheadBeadWrites(t, f.bd) - before; n != 0 {
		t.Errorf("ReviewAhead wrote %d bead records, want 0", n)
	}
	aheadNoReviewDir(t, f)
	if !l.gitMu.TryLock() {
		t.Errorf("gitMu is still held after ReviewAhead returned an error")
	} else {
		l.gitMu.Unlock()
	}
}

// TestVerdictReuseAheadStoreFails: a conflict whose record the store cannot
// write is returned as an error naming the store stage, never as AheadConflict:
// a conflict nobody can read back must not read as one that was kept.
func TestVerdictReuseAheadStoreFails(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	aheadSeedBead(f, aheadReadyIssue(f.work))
	l := f.lander()
	store := TownAheadStore(f.town, "gastown")
	writeErr := errors.New("disk full")
	writes := 0
	store.writeFile = func(string, []byte, os.FileMode) error {
		writes++
		return writeErr
	}
	l.Ahead = store
	aheadConflictMain(f)
	before := aheadBeadWrites(t, f.bd)
	got, err := l.ReviewAhead(context.Background(), f.work)
	if err == nil {
		t.Fatalf("ReviewAhead: got outcome %q and no error, want the store error", got)
	}
	if !errors.Is(err, writeErr) {
		t.Errorf("ReviewAhead error = %v, want it to wrap %v", err, writeErr)
	}
	var infra *InfraError
	if !errors.As(err, &infra) {
		t.Fatalf("ReviewAhead error = %v, want an *InfraError", err)
	}
	if infra.Stage != "store record" {
		t.Errorf("error stage = %q, want %q", infra.Stage, "store record")
	}
	if got != "" {
		t.Errorf("ReviewAhead outcome = %q on error, want empty (not %q)", got, AheadConflict)
	}
	if writes != 1 {
		t.Errorf("the store write ran %d times, want 1", writes)
	}
	if _, err := os.Stat(aheadTestPath(f.town, "gastown", f.work.BeadID)); !os.IsNotExist(err) {
		t.Errorf("a record exists although the write failed (stat err %v)", err)
	}
	if n := aheadBeadWrites(t, f.bd) - before; n != 0 {
		t.Errorf("ReviewAhead wrote %d bead records, want 0", n)
	}
	aheadNoReviewDir(t, f)
	if !l.gitMu.TryLock() {
		t.Errorf("gitMu is still held after ReviewAhead returned an error")
	} else {
		l.gitMu.Unlock()
	}
}

// aheadCheckRepo is the fixture's repository with the tree comparison failed on
// demand.
type aheadCheckRepo struct {
	Repo
	failTrees error
}

func (r *aheadCheckRepo) TreesIdentical(a, b string) (bool, error) {
	if r.failTrees != nil {
		return false, r.failTrees
	}
	return r.Repo.TreesIdentical(a, b)
}

// TestVerdictReuseAheadSkipsAfterMerge: a clean merge the landing would refuse
// as empty, or as an edit to the root .om.json, has nothing for om to judge, so
// ReviewAhead skips it and stores no record. Each row asserts the skip's log
// line, which names the reason: a mutant that deletes its check falls through
// to the same AheadSkipped without that line, so the outcome alone would not
// turn it red.
func TestVerdictReuseAheadSkipsAfterMerge(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name  string
		setup func(f *landFixture)
		// log is the line the skip writes, naming the reason it skipped.
		log string
	}{
		{"merged tree equals base's", func(f *landFixture) {
			// The branch's only change is already on the target, so the merge
			// yields base's tree.
			f.pushMain("b.txt", "work\n")
		}, "leaves origin/main unchanged"},
		{"branch edits .om.json", func(f *landFixture) {
			f.branchEditing(omConfigFile, "{\"threshold\": 0.9}\n")
		}, "edits " + omConfigFile + ", which waits for the overseer"},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			tc.setup(f)
			aheadSeedBead(f, aheadReadyIssue(f.work))
			l := f.lander()
			l.Ahead = TownAheadStore(f.town, "gastown")
			var out bytes.Buffer
			l.Out = &out
			before := aheadBeadWrites(t, f.bd)
			got, err := l.ReviewAhead(context.Background(), f.work)
			if err != nil {
				t.Fatalf("ReviewAhead: unexpected error: %v", err)
			}
			if got != AheadSkipped {
				t.Errorf("ReviewAhead = %q, want %q", got, AheadSkipped)
			}
			if !strings.Contains(out.String(), tc.log) {
				t.Errorf("ReviewAhead logged %q, want a line containing %q", out.String(), tc.log)
			}
			if _, err := os.Stat(aheadTestPath(f.town, "gastown", f.work.BeadID)); !os.IsNotExist(err) {
				t.Errorf("a record was stored for a skipped review (stat err %v)", err)
			}
			if n := aheadBeadWrites(t, f.bd) - before; n != 0 {
				t.Errorf("ReviewAhead wrote %d bead records, want 0", n)
			}
			aheadNoReviewDir(t, f)
			if !l.gitMu.TryLock() {
				t.Errorf("gitMu is still held after ReviewAhead returned")
			} else {
				l.gitMu.Unlock()
			}
		})
	}
}

// TestVerdictReuseAheadSkipsAfterMergeTreeReadFails: the empty-merge check is a
// git read; a read that fails is returned as an error rather than skipping a
// review at an unknown tree.
func TestVerdictReuseAheadSkipsAfterMergeTreeReadFails(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	aheadSeedBead(f, aheadReadyIssue(f.work))
	l := f.lander()
	l.Ahead = TownAheadStore(f.town, "gastown")
	cmpErr := errors.New("fatal: bad object HEAD^{tree}")
	l.openRepo = func(dir string) Repo {
		return &aheadCheckRepo{Repo: f.git.Open(dir), failTrees: cmpErr}
	}
	before := aheadBeadWrites(t, f.bd)
	got, err := l.ReviewAhead(context.Background(), f.work)
	if err == nil {
		t.Fatalf("ReviewAhead: got outcome %q and no error, want the tree-comparison error", got)
	}
	if !errors.Is(err, cmpErr) {
		t.Errorf("ReviewAhead error = %v, want it to wrap %v", err, cmpErr)
	}
	var infra *InfraError
	if !errors.As(err, &infra) {
		t.Fatalf("ReviewAhead error = %v, want an *InfraError", err)
	}
	if infra.Stage != "compare trees" {
		t.Errorf("error stage = %q, want %q", infra.Stage, "compare trees")
	}
	if got != "" {
		t.Errorf("ReviewAhead outcome = %q on error, want empty", got)
	}
	if _, err := os.Stat(aheadTestPath(f.town, "gastown", f.work.BeadID)); !os.IsNotExist(err) {
		t.Errorf("a record was stored for a review whose tree read failed (stat err %v)", err)
	}
	if n := aheadBeadWrites(t, f.bd) - before; n != 0 {
		t.Errorf("ReviewAhead wrote %d bead records, want 0", n)
	}
	aheadNoReviewDir(t, f)
	if !l.gitMu.TryLock() {
		t.Errorf("gitMu is still held after ReviewAhead returned an error")
	} else {
		l.gitMu.Unlock()
	}
}

// aheadBeadWriteLog is a bead store that records every write ReviewAhead asks
// for through the Beads interface and forwards it to the fixture's fake. The
// pool's contract is that only the serial landing changes a bead, and reject is
// the way it could break it: reject reaches a bead only through these five
// writes, a rejection note, a label update, a comment, or a close. A reject
// that returns before writing returns the rejection as an error, which a row
// requiring err == nil catches too, so a row with both assertions covers
// every reject path as well as every direct write.
type aheadBeadWriteLog struct {
	*beadsfake.Fake
	calls []string
}

func (b *aheadBeadWriteLog) Update(id string, opts beads.UpdateOptions) error {
	b.calls = append(b.calls, "Update "+id)
	return b.Fake.Update(id, opts)
}

func (b *aheadBeadWriteLog) AppendNotes(id, note string) error {
	b.calls = append(b.calls, "AppendNotes "+id)
	return b.Fake.AppendNotes(id, note)
}

func (b *aheadBeadWriteLog) AddComment(id, text string) error {
	b.calls = append(b.calls, "AddComment "+id)
	return b.Fake.AddComment(id, text)
}

func (b *aheadBeadWriteLog) ForceCloseWithReason(reason string, ids ...string) error {
	b.calls = append(b.calls, "ForceCloseWithReason "+strings.Join(ids, ","))
	return b.Fake.ForceCloseWithReason(reason, ids...)
}

func (b *aheadBeadWriteLog) CloseWithReason(reason string, ids ...string) error {
	b.calls = append(b.calls, "CloseWithReason "+strings.Join(ids, ","))
	return b.Fake.CloseWithReason(reason, ids...)
}

// TestVerdictReuseAheadWritesNoBeadState: on every path the pool has built so
// far — a skip before the review, a conflict, and a stored verdict — ReviewAhead
// makes no bead write and calls no reject. The store counts the writes, so a
// bead whose state the pool changed would be named by the record it left.
func TestVerdictReuseAheadWritesNoBeadState(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name  string
		store bool // whether the lander has a review-ahead store
		issue func(w Work) beads.Issue
		setup func(f *landFixture)
		want  AheadOutcome
	}{
		{"skip with the feature off", false, aheadReadyIssue, nil, AheadSkipped},
		{"skip on a bead that is not actionable", true, func(w Work) beads.Issue {
			i := aheadReadyIssue(w)
			i.Status = "closed"
			return i
		}, nil, AheadSkipped},
		{"conflict", true, aheadReadyIssue, func(f *landFixture) {
			aheadConflictMain(f)
		}, AheadConflict},
		{"clean merge", true, aheadReadyIssue, nil, AheadStored},
		{"clean merge leaving the target's tree unchanged", true, aheadReadyIssue, func(f *landFixture) {
			f.pushMain("b.txt", "work\n")
		}, AheadSkipped},
		{"clean merge editing .om.json", true, aheadReadyIssue, func(f *landFixture) {
			f.branchEditing(omConfigFile, "{\"threshold\": 0.9}\n")
		}, AheadSkipped},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, l, _ := aheadKeyFixture(t)
			// branchEditing re-seeds the fixture's bead and work, so the row's
			// setup runs before the bead is seeded from f.work.
			if tc.setup != nil {
				tc.setup(f)
			}
			aheadSeedBead(f, tc.issue(f.work))
			if tc.store {
				l.Ahead = TownAheadStore(f.town, "gastown")
			}
			writes := &aheadBeadWriteLog{Fake: f.bd}
			l.Beads = writes
			got, err := l.ReviewAhead(context.Background(), f.work)
			if err != nil {
				t.Fatalf("ReviewAhead: unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("ReviewAhead = %q, want %q", got, tc.want)
			}
			if len(writes.calls) != 0 {
				t.Errorf("ReviewAhead wrote bead state on the %s path: %v", tc.name, writes.calls)
			}
		})
	}
}

// TestVerdictReuseAheadChangedPathsReadFails: the .om.json check is a git read;
// a read that fails is returned as an error, not skipped as the config edit is,
// so a caller switching on the outcome cannot read the failed read as a
// submission with nothing to review.
func TestVerdictReuseAheadChangedPathsReadFails(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	aheadSeedBead(f, aheadReadyIssue(f.work))
	l := f.lander()
	l.Ahead = TownAheadStore(f.town, "gastown")
	l.openRepo = func(dir string) Repo {
		return diffFailsRepo{Repo: f.git.Open(dir)}
	}
	before := aheadBeadWrites(t, f.bd)
	got, err := l.ReviewAhead(context.Background(), f.work)
	if err == nil {
		t.Fatalf("ReviewAhead: got outcome %q and no error, want the changed-paths read error", got)
	}
	var infra *InfraError
	if !errors.As(err, &infra) {
		t.Fatalf("ReviewAhead error = %v, want an *InfraError", err)
	}
	if infra.Stage != "read changed paths" {
		t.Errorf("error stage = %q, want %q", infra.Stage, "read changed paths")
	}
	if got != "" {
		t.Errorf("ReviewAhead outcome = %q on error, want empty (not %q)", got, AheadSkipped)
	}
	if _, err := os.Stat(aheadTestPath(f.town, "gastown", f.work.BeadID)); !os.IsNotExist(err) {
		t.Errorf("a record was stored for a review whose changed-paths read failed (stat err %v)", err)
	}
	if n := aheadBeadWrites(t, f.bd) - before; n != 0 {
		t.Errorf("ReviewAhead wrote %d bead records, want 0", n)
	}
	aheadNoReviewDir(t, f)
	if !l.gitMu.TryLock() {
		t.Errorf("gitMu is still held after ReviewAhead returned an error")
	} else {
		l.gitMu.Unlock()
	}
}

// aheadKeyReviewer is a bead reviewer that also answers om's version probe, so
// a review key can be computed from the input it is handed. Land calls ReviewBead
// and records each input it is given.
type aheadKeyReviewer struct {
	recordBeadReviewer
	version string
}

func (r *aheadKeyReviewer) OMVersion(context.Context, ReviewInput) string { return r.version }

// Isolated returns a reviewer of its own with the same verdict, so the review
// made ahead of a landing leaves inputs holding the landing's calls alone.
func (r *aheadKeyReviewer) Isolated(string) Reviewer {
	return &recordBeadReviewer{verdict: r.verdict}
}

// aheadKeyFixture is keyFixture for a test whose review reaches om: its
// reviewer can be isolated, and the isolated copy approves.
func aheadKeyFixture(t *testing.T) (*landFixture, *Lander, Repo) {
	t.Helper()
	f, l, wt := keyFixture(t)
	l.Reviewer = &aheadKeyReviewer{
		recordBeadReviewer: recordBeadReviewer{verdict: Verdict{Verdict: VerdictApprove, Score: 0.9}},
		version:            stubOMVersion,
	}
	return f, l, wt
}

// comparableInput clears the fields the review digest leaves out: the checkout,
// the range, the repository and the log sink. What remains is what om is given,
// so two inputs equal here digest equal.
func comparableInput(in ReviewInput) ReviewInput {
	in.Dir, in.Base, in.Head = "", "", ""
	in.Repo = nil
	in.Logf = nil
	return in
}

// aheadTownOperator writes the town settings that name one operator identity, the
// entitlement a decision or an accepted risk must carry to count.
func aheadTownOperator(t *testing.T, town, name string) {
	t.Helper()
	path := config.TownSettingsPath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"operator":{"names":["` + name + `"]}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// aheadRoundsDir is a rounds directory holding one round for bead, so the rounds
// text a review is given is not empty.
func aheadRoundsDir(t *testing.T, bead string) string {
	t.Helper()
	dir := t.TempDir()
	ledger := &RoundsLedger{OutDir: dir, Bead: bead}
	if err := ledger.Append(1, "head0", []Finding{{ID: "f1", Severity: "major", Path: "b.txt", Title: "first"}}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	return dir
}

// TestVerdictReuseAheadBuildsTheLandingsInput: for the same bead and merge, the
// input ReviewAhead builds is the input Land's om stage gives om, and the key
// ReviewAhead computes equals the key the landing's reuse path computes from its
// own input. Land runs for real and its input is captured, so the comparison is
// with what the landing builds rather than with a copy of it.
func TestVerdictReuseAheadBuildsTheLandingsInput(t *testing.T) {
	t.Parallel()
	f, l, _ := keyFixture(t)
	rev := &aheadKeyReviewer{
		recordBeadReviewer: recordBeadReviewer{verdict: Verdict{Verdict: VerdictApprove, Score: 0.9}},
		version:            stubOMVersion,
	}
	l.Reviewer = rev
	l.RoundsDir = aheadRoundsDir(t, f.work.BeadID)
	// An operator named in the town settings, and an accepted risk that operator
	// recorded: the input then carries text that depends on the operator names
	// and on the bead's comments, so the comparison sees both.
	l.TownRoot = f.town
	aheadTownOperator(t, f.town, "overseer")
	if err := f.bd.AddCommentAs(f.work.BeadID, "overseer", "ACCEPTED-RISK: b.txt is accepted as written"); err != nil {
		t.Fatalf("AddCommentAs: %v", err)
	}
	l.Ahead = TownAheadStore(f.town, "gastown")
	got, built, err := l.reviewAhead(context.Background(), f.work)
	if err != nil {
		t.Fatalf("reviewAhead: unexpected error: %v", err)
	}
	if got != AheadStored {
		t.Fatalf("reviewAhead = %q, want %q after the review's verdict is stored", got, AheadStored)
	}
	if built.In.RoundsText == "" {
		t.Fatal("reviewAhead built no rounds text: the comparison would not see the ledger")
	}
	if built.In.AcceptedRisksText == "" {
		t.Fatal("reviewAhead built no accepted-risk text: the comparison would not see the operator or the comments")
	}
	// The review worktree is removed before reviewAhead returns, so the input it
	// hands back names no checkout.
	if built.In.Dir != "" {
		t.Errorf("built input names the review checkout %q, which the returned worktree no longer has", built.In.Dir)
	}
	// Land runs after the pool and with no store, so its own review is the
	// inline one the landing makes without a review-ahead record.
	l.Ahead = nil
	res, err := l.Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	if len(rev.inputs) != 1 {
		t.Fatalf("landing reviewer calls = %d, want 1", len(rev.inputs))
	}
	landIn := rev.inputs[0]
	// The landing's key is computed by the reuse path Land's review stage will
	// call, over the clone that stage reads om's config from, at the merge
	// commit the landing made. reviewKey's checkout argument is used only for
	// the commit-to-commit reads, which the clone answers.
	l.Ahead = TownAheadStore(f.town, "gastown")
	landRepo := l.open(l.Repo)
	_, landKey, hit, err := l.reuseAhead(context.Background(), landRepo, landRepo, f.work, landIn, f.base, res.LandedCommit)
	if err != nil {
		t.Fatalf("reuseAhead over the landing's input: %v", err)
	}
	if hit {
		t.Fatal("reuseAhead hit on a store holding no record")
	}
	if !reflect.DeepEqual(comparableInput(built.In), comparableInput(landIn)) {
		t.Errorf("reviewAhead's input differs from the landing's:\n pool:    %+v\n landing: %+v", comparableInput(built.In), comparableInput(landIn))
	}
	if built.Key != landKey {
		t.Errorf("reviewAhead's key = %+v, want the landing's key %+v", built.Key, landKey)
	}
}

// aheadCommentsFails is the bead store with its comment read failing.
type aheadCommentsFails struct{ *beadsfake.Fake }

func (aheadCommentsFails) Comments(string) ([]beads.Comment, error) {
	return nil, errors.New("bd comments: read failed")
}

// writeAheadOMConfig writes a readable om config file and returns its path, the
// file a review key reads the config from.
func writeAheadOMConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "om.json")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestVerdictReuseAheadKeyErrorReturned: a key or input that cannot be built is
// returned as the error, with no outcome, no stored record and no bead write, so
// a caller never reads a failed build as a skipped review.
func TestVerdictReuseAheadKeyErrorReturned(t *testing.T) {
	t.Parallel()
	rows := []struct {
		name  string
		stage string
		setup func(t *testing.T) (*landFixture, *Lander)
	}{
		{"reviewer cannot report its om version", "review key", func(t *testing.T) (*landFixture, *Lander) {
			f, l, _ := keyFixture(t)
			l.Reviewer = &recordBeadReviewer{}
			return f, l
		}},
		{"om config unreadable", "review key", func(t *testing.T) (*landFixture, *Lander) {
			f, l, _ := keyFixture(t)
			// A directory: the config read fails, which is not an absent file.
			l.OMConfigPath = t.TempDir()
			return f, l
		}},
		{"base has no .om.json", "review key", func(t *testing.T) (*landFixture, *Lander) {
			f := newLandFixture(t)
			l := f.lander()
			l.Reviewer = &aheadKeyReviewer{version: stubOMVersion}
			l.OMConfigPath = writeAheadOMConfig(t)
			return f, l
		}},
		{"operator settings unreadable", "read operator config", func(t *testing.T) (*landFixture, *Lander) {
			f, l, _ := keyFixture(t)
			path := config.TownSettingsPath(f.town)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
			l.TownRoot = f.town
			return f, l
		}},
		{"bead comments unreadable", "read comments", func(t *testing.T) (*landFixture, *Lander) {
			f, l, _ := keyFixture(t)
			l.Beads = aheadCommentsFails{f.bd}
			return f, l
		}},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, l := tc.setup(t)
			l.Ahead = TownAheadStore(f.town, "gastown")
			before := aheadBeadWrites(t, f.bd)
			got, built, err := l.reviewAhead(context.Background(), f.work)
			if err == nil {
				t.Fatalf("reviewAhead: got outcome %q and no error, want the %q error", got, tc.stage)
			}
			var infra *InfraError
			if !errors.As(err, &infra) {
				t.Fatalf("reviewAhead error = %v, want an *InfraError", err)
			}
			if infra.Stage != tc.stage {
				t.Errorf("error stage = %q, want %q", infra.Stage, tc.stage)
			}
			if got != "" {
				t.Errorf("reviewAhead outcome = %q on error, want empty (not %q)", got, AheadSkipped)
			}
			if !reflect.DeepEqual(built, aheadBuilt{}) {
				t.Errorf("reviewAhead built %+v on error, want nothing", built)
			}
			if _, err := os.Stat(aheadTestPath(f.town, "gastown", f.work.BeadID)); !os.IsNotExist(err) {
				t.Errorf("a record was stored for a review whose key could not be built (stat err %v)", err)
			}
			if n := aheadBeadWrites(t, f.bd) - before; n != 0 {
				t.Errorf("reviewAhead wrote %d bead records, want 0", n)
			}
			aheadNoReviewDir(t, f)
			if !l.gitMu.TryLock() {
				t.Errorf("gitMu is still held after reviewAhead returned an error")
			} else {
				l.gitMu.Unlock()
			}
		})
	}
}

// aheadPoolReviewer is a Reviewer that can be isolated. It fails the test when
// it is run itself: a review made ahead of a landing belongs to the copy
// Isolated returns, which records what it is handed.
type aheadPoolReviewer struct {
	t *testing.T
	// verdicts are returned by the copy's runs in order; the last repeats.
	verdicts []Verdict
	err      error
	// under holds the directory of each Isolated call, runs each input the copy
	// was handed, and checkouts whether the input's checkout existed then.
	under     []string
	runs      []ReviewInput
	checkouts []bool
}

func (r *aheadPoolReviewer) Review(context.Context, string, string, string) (Verdict, error) {
	r.t.Error("the Lander's own reviewer ran a review made ahead of a landing")
	return Verdict{}, errors.New("unreachable")
}

func (r *aheadPoolReviewer) OMVersion(context.Context, ReviewInput) string { return stubOMVersion }

func (r *aheadPoolReviewer) Isolated(tmp string) Reviewer {
	r.under = append(r.under, tmp)
	return &aheadPoolCopy{r}
}

type aheadPoolCopy struct{ of *aheadPoolReviewer }

func (c *aheadPoolCopy) Review(context.Context, string, string, string) (Verdict, error) {
	c.of.t.Error("the isolated copy was run without the bead's input")
	return Verdict{}, errors.New("unreachable")
}

func (c *aheadPoolCopy) ReviewBead(_ context.Context, in ReviewInput) (Verdict, error) {
	r := c.of
	_, err := os.Stat(in.Dir)
	r.checkouts = append(r.checkouts, err == nil)
	r.runs = append(r.runs, in)
	if r.err != nil {
		return Verdict{}, r.err
	}
	return r.verdicts[min(len(r.runs), len(r.verdicts))-1], nil
}

// TestVerdictReuseAheadRunsIsolated: a clean merge is reviewed by the reviewer
// isolated under the review directory, over the base and the merged commit, with
// the landing's own re-score of a borderline rejection, and the verdict is
// stored. A review that fails is an error with no outcome; a reviewer that
// cannot be isolated is skipped instead, which
// TestVerdictReuseAheadNotIsolatorSkips holds.
func TestVerdictReuseAheadRunsIsolated(t *testing.T) {
	t.Parallel()
	approve := Verdict{Verdict: VerdictApprove, Score: 0.9}
	// keyOMConfig's bar is 0.8, so 0.78 with no finding is borderline.
	borderline := Verdict{Verdict: VerdictRequestChanges, Score: 0.78}
	failed := errors.New("om: no verdict")
	rows := []struct {
		name     string
		verdicts []Verdict
		err      error
		runs     int
		wantErr  string
		wantLog  string
	}{
		{name: "approval", verdicts: []Verdict{approve}, runs: 1},
		{name: "borderline rejection is scored again", verdicts: []Verdict{borderline, approve}, runs: 2, wantLog: "om runs once more"},
		{name: "review error", err: failed, runs: 1, wantErr: "om: no verdict"},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, l, _ := keyFixture(t)
			l.Ahead = TownAheadStore(f.town, "gastown")
			var log bytes.Buffer
			l.Out = &log
			rev := &aheadPoolReviewer{t: t, verdicts: tc.verdicts, err: tc.err}
			l.Reviewer = rev
			got, built, err := l.reviewAhead(context.Background(), f.work)
			if len(rev.runs) != tc.runs {
				t.Fatalf("isolated reviewer runs = %d, want %d (outcome %q, err %v)", len(rev.runs), tc.runs, got, err)
			}
			for i, in := range rev.runs {
				if len(rev.under) != 1 || rev.under[0] != filepath.Dir(in.Dir) || filepath.Dir(rev.under[0]) != f.workRoot {
					t.Errorf("run %d: isolated under %q for checkout %s, want once, under the review directory in %s", i, rev.under, in.Dir, f.workRoot)
				}
				if !rev.checkouts[i] {
					t.Errorf("run %d: the checkout %s did not exist while om ran", i, in.Dir)
				}
				if in.Base != f.base || in.Head == f.base || in.Head == f.work.Head || in.BeadID != f.work.BeadID {
					t.Errorf("run %d: reviewed %s..%s for %q, want %s to the merged commit for %s", i, in.Base, in.Head, in.BeadID, f.base, f.work.BeadID)
				}
			}
			if !strings.Contains(log.String(), tc.wantLog) {
				t.Errorf("land log %q does not say %q", log.String(), tc.wantLog)
			}
			aheadNoReviewDir(t, f)
			if tc.wantErr == "" {
				if err != nil || got != AheadStored {
					t.Fatalf("reviewAhead = %q, %v; want %q and no error", got, err, AheadStored)
				}
				if built.In.Head != rev.runs[0].Head {
					t.Errorf("reviewed head %s, want the merged commit %s the key was built for", rev.runs[0].Head, built.In.Head)
				}
				return
			}
			var infra *InfraError
			if !errors.As(err, &infra) || infra.Stage != "review" || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("reviewAhead error = %v, want a review *InfraError saying %q", err, tc.wantErr)
			}
			if got != "" || !reflect.DeepEqual(built, aheadBuilt{}) {
				t.Errorf("reviewAhead = %q with built %+v on error, want neither", got, built)
			}
		})
	}
}

// aheadStoringFixture is keyFixture with a reviewer the pool can isolate that
// reaches verdict, and a store whose clock steps a minute per read, so a stored
// record's start and review time are told apart. t0 is the first reading; the
// review begins at t0+1m and is dated at t0+2m.
func aheadStoringFixture(t *testing.T, verdict Verdict) (*landFixture, *Lander, time.Time) {
	t.Helper()
	f, l, _ := keyFixture(t)
	l.Reviewer = &aheadKeyReviewer{
		recordBeadReviewer: recordBeadReviewer{verdict: verdict},
		version:            stubOMVersion,
	}
	t0 := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	reads := 0
	store := TownAheadStore(f.town, "gastown")
	store.Now = func() time.Time {
		reads++
		return t0.Add(time.Duration(reads) * time.Minute)
	}
	l.Ahead = store
	return f, l, t0
}

// aheadStoredVerdict runs reviewAhead and holds the record it stored against
// the key the same call built: every key field, the base, both times, the
// verdict, and one minute of review time. The review logs no line and writes no
// round: the landing writes those when it consumes a verdict.
func aheadStoredVerdict(t *testing.T, verdict Verdict) {
	t.Helper()
	f, l, t0 := aheadStoringFixture(t, verdict)
	reviewsPath := filepath.Join(testutil.PrivateTempDir(t), "reviews.jsonl")
	l.Reviews = &ReviewsFile{Path: reviewsPath}
	rounds := testutil.PrivateTempDir(t)
	l.RoundsDir = rounds
	before := aheadBeadWrites(t, f.bd)
	got, built, err := l.reviewAhead(context.Background(), f.work)
	if err != nil {
		t.Fatalf("reviewAhead: %v", err)
	}
	if got != AheadStored {
		t.Fatalf("reviewAhead = %q, want %q", got, AheadStored)
	}
	data, err := os.ReadFile(aheadTestPath(f.town, "gastown", f.work.BeadID))
	if err != nil {
		t.Fatalf("reading the stored record: %v", err)
	}
	var rec AheadRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		t.Fatalf("the stored record is not an AheadRecord: %v\n%s", err, data)
	}
	if rec.Outcome != aheadOutcomeVerdict || rec.Source != aheadSourcePool {
		t.Errorf("record outcome/source = %q/%q, want %q/%q", rec.Outcome, rec.Source, aheadOutcomeVerdict, aheadSourcePool)
	}
	if rec.Rig != "gastown" {
		t.Errorf("record rig = %q, want gastown", rec.Rig)
	}
	stored := ReviewKey{Bead: rec.Bead, Head: rec.Head, PatchID: rec.PatchID, OMVersion: rec.OMVersion,
		OMConfigHash: rec.OMConfigHash, InputDigest: rec.InputDigest}
	if stored != built.Key {
		t.Errorf("stored key = %+v, want the key reviewAhead built %+v", stored, built.Key)
	}
	if rec.Base != f.base {
		t.Errorf("record base = %s, want the target tip %s", rec.Base, f.base)
	}
	if want := t0.Add(time.Minute); !rec.StartedAt.Equal(want) {
		t.Errorf("record started_at = %s, want the review's start %s", rec.StartedAt, want)
	}
	if want := t0.Add(2 * time.Minute); !rec.ReviewedAt.Equal(want) {
		t.Errorf("record reviewed_at = %s, want the review's return %s", rec.ReviewedAt, want)
	}
	if rec.ReviewSecs != 60 {
		t.Errorf("record review_secs = %v, want 60", rec.ReviewSecs)
	}
	if !reflect.DeepEqual(rec.Verdict, verdict) {
		t.Errorf("record verdict = %+v, want %+v", rec.Verdict, verdict)
	}
	if _, err := os.Stat(reviewsPath); !os.IsNotExist(err) {
		t.Errorf("ReviewAhead wrote the reviews log (stat err %v)", err)
	}
	entries, err := os.ReadDir(rounds)
	if err != nil {
		t.Fatalf("reading the rounds directory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("ReviewAhead wrote %d rounds entries, want none", len(entries))
	}
	if n := aheadBeadWrites(t, f.bd) - before; n != 0 {
		t.Errorf("ReviewAhead wrote %d bead records, want 0", n)
	}
	aheadNoReviewDir(t, f)
	if !l.gitMu.TryLock() {
		t.Errorf("gitMu is still held after ReviewAhead returned")
	} else {
		l.gitMu.Unlock()
	}
}

// TestVerdictReuseAheadStoresApprove: a review that approves stores a verdict
// record carrying every key field, the base and the approval, so the landing
// can reuse the verdict instead of running om again.
func TestVerdictReuseAheadStoresApprove(t *testing.T) {
	t.Parallel()
	aheadStoredVerdict(t, Verdict{Verdict: VerdictApprove, Score: 0.9})
}

// TestVerdictReuseAheadStoresReject: a review that requests changes stores the
// same record with the rejection and its findings, so the landing reuses a
// refusal as it reuses an approval.
func TestVerdictReuseAheadStoresReject(t *testing.T) {
	t.Parallel()
	aheadStoredVerdict(t, Verdict{Verdict: VerdictRequestChanges, Score: 0.4,
		Findings: []Finding{{ID: "f1", Severity: omSeverityMajor, Path: "b.txt", Title: "breaks the tree"}}})
}

// TestVerdictReuseAheadPutFails: a verdict whose record the store cannot write
// is returned as an error naming the store stage, never as AheadStored. A
// verdict nobody can read back must not read as one that was kept.
func TestVerdictReuseAheadPutFails(t *testing.T) {
	t.Parallel()
	f, l, _ := aheadStoringFixture(t, Verdict{Verdict: VerdictApprove, Score: 0.9})
	writeErr := errors.New("disk full")
	l.Ahead.writeFile = func(string, []byte, os.FileMode) error { return writeErr }
	before := aheadBeadWrites(t, f.bd)
	got, built, err := l.reviewAhead(context.Background(), f.work)
	if err == nil {
		t.Fatalf("reviewAhead: got outcome %q and no error, want the store error", got)
	}
	if !errors.Is(err, writeErr) {
		t.Errorf("reviewAhead error = %v, want it to wrap %v", err, writeErr)
	}
	var infra *InfraError
	if !errors.As(err, &infra) {
		t.Fatalf("reviewAhead error = %v, want an *InfraError", err)
	}
	if infra.Stage != "store record" {
		t.Errorf("error stage = %q, want %q", infra.Stage, "store record")
	}
	if got != "" || !reflect.DeepEqual(built, aheadBuilt{}) {
		t.Errorf("reviewAhead = %q with built %+v on error, want neither", got, built)
	}
	if _, err := os.Stat(aheadTestPath(f.town, "gastown", f.work.BeadID)); !os.IsNotExist(err) {
		t.Errorf("a record exists although the write failed (stat err %v)", err)
	}
	if n := aheadBeadWrites(t, f.bd) - before; n != 0 {
		t.Errorf("ReviewAhead wrote %d bead records, want 0", n)
	}
	aheadNoReviewDir(t, f)
	if !l.gitMu.TryLock() {
		t.Errorf("gitMu is still held after ReviewAhead returned an error")
	} else {
		l.gitMu.Unlock()
	}
}

// aheadPlainReviewer is a Reviewer the pool cannot isolate: it has no Isolated
// method. It records each review it is handed and fails the test, so a skip that
// let a review through is red rather than quietly observed. It answers the
// version probe, so the review key is built and the skip is the reviewer's
// isolation, not a key that could not be made.
type aheadPlainReviewer struct {
	t    *testing.T
	runs []ReviewInput
}

func (r *aheadPlainReviewer) Review(context.Context, string, string, string) (Verdict, error) {
	r.t.Error("the Lander's plain reviewer ran a review it cannot isolate beside a landing")
	return Verdict{}, errors.New("unreachable")
}

func (r *aheadPlainReviewer) ReviewBead(_ context.Context, in ReviewInput) (Verdict, error) {
	r.runs = append(r.runs, in)
	r.t.Error("the Lander's plain reviewer ran a review it cannot isolate beside a landing")
	return Verdict{}, errors.New("unreachable")
}

func (r *aheadPlainReviewer) OMVersion(context.Context, ReviewInput) string { return stubOMVersion }

// TestVerdictReuseAheadNotIsolatorSkips: a reviewer the pool cannot isolate
// cannot run beside a landing, so ReviewAhead makes no review and reports
// AheadSkipped with no built value and nothing stored; the landing runs om
// inline, as it does with no review-ahead store.
func TestVerdictReuseAheadNotIsolatorSkips(t *testing.T) {
	t.Parallel()
	f, l, _ := keyFixture(t)
	rev := &aheadPlainReviewer{t: t}
	l.Reviewer = rev
	l.Ahead = TownAheadStore(f.town, "gastown")
	var log bytes.Buffer
	l.Out = &log
	before := aheadBeadWrites(t, f.bd)
	got, built, err := l.reviewAhead(context.Background(), f.work)
	if err != nil {
		t.Fatalf("reviewAhead: %v, want %q and no error", err, AheadSkipped)
	}
	if got != AheadSkipped {
		t.Errorf("reviewAhead = %q, want %q", got, AheadSkipped)
	}
	if len(rev.runs) != 0 {
		t.Errorf("the reviewer ran %d review(s), want none", len(rev.runs))
	}
	if !reflect.DeepEqual(built, aheadBuilt{}) {
		t.Errorf("reviewAhead built %+v for a review it did not make, want nothing", built)
	}
	if !strings.Contains(log.String(), "cannot be isolated") {
		t.Errorf("land log %q does not say the reviewer cannot be isolated", log.String())
	}
	if _, err := os.Stat(aheadTestPath(f.town, "gastown", f.work.BeadID)); !os.IsNotExist(err) {
		t.Errorf("a record was stored for a review that was skipped (stat err %v)", err)
	}
	if n := aheadBeadWrites(t, f.bd) - before; n != 0 {
		t.Errorf("reviewAhead wrote %d bead records, want 0", n)
	}
	aheadNoReviewDir(t, f)
	if !l.gitMu.TryLock() {
		t.Errorf("gitMu is still held after ReviewAhead returned")
	} else {
		l.gitMu.Unlock()
	}
}

// aheadContextReviewer is a reviewer the pool can isolate whose copy answers the
// context's error once the context is done, and verdict or err otherwise: a
// cancelled or expired context reaches the pool the way it reaches om, as the
// review's own error. The original fails the test when it is run, so a row can
// only pass on the copy the pool isolated.
type aheadContextReviewer struct {
	t       *testing.T
	verdict Verdict
	err     error
}

func (r *aheadContextReviewer) Review(context.Context, string, string, string) (Verdict, error) {
	r.t.Error("the Lander's own reviewer ran a review made ahead of a landing")
	return Verdict{}, errors.New("unreachable")
}

func (r *aheadContextReviewer) OMVersion(context.Context, ReviewInput) string { return stubOMVersion }

func (r *aheadContextReviewer) Isolated(string) Reviewer { return &aheadContextCopy{of: r} }

type aheadContextCopy struct{ of *aheadContextReviewer }

func (c *aheadContextCopy) Review(context.Context, string, string, string) (Verdict, error) {
	c.of.t.Error("the isolated copy was run without the bead's input")
	return Verdict{}, errors.New("unreachable")
}

func (c *aheadContextCopy) ReviewBead(ctx context.Context, _ ReviewInput) (Verdict, error) {
	if err := ctx.Err(); err != nil {
		return Verdict{}, err
	}
	return c.of.verdict, c.of.err
}

// TestVerdictReuseAheadErrorStoresNothing: a review the pool cannot stand on
// returns its error with no outcome, and a verdict word that is neither approve
// nor request_changes is no decision a landing can reuse and is skipped
// instead. In every row nothing is stored, no bead is written and the review
// directory is removed, so the landing runs om inline as it does without a
// store.
func TestVerdictReuseAheadErrorStoresNothing(t *testing.T) {
	t.Parallel()
	omErr := errors.New("om: no verdict")
	rows := []struct {
		name  string
		setup func(t *testing.T) (*landFixture, *Lander, context.Context)
		// stage is the InfraError stage a failed row names; "" is a row that is
		// skipped rather than failed.
		stage string
		// wantErr is the cause the error must wrap; nil checks only the stage.
		wantErr error
	}{
		{"om execution error", func(t *testing.T) (*landFixture, *Lander, context.Context) {
			f, l, _ := keyFixture(t)
			l.Reviewer = &aheadContextReviewer{t: t, err: omErr}
			return f, l, context.Background()
		}, "review", omErr},
		{"timeout", func(t *testing.T) (*landFixture, *Lander, context.Context) {
			f, l, _ := keyFixture(t)
			l.Reviewer = &aheadContextReviewer{t: t}
			// The zero time is already past, so the context is done with
			// DeadlineExceeded before the review runs, without a timer.
			ctx, cancel := context.WithDeadline(context.Background(), time.Time{})
			t.Cleanup(cancel)
			return f, l, ctx
		}, "review", context.DeadlineExceeded},
		{"cancelled context", func(t *testing.T) (*landFixture, *Lander, context.Context) {
			f, l, _ := keyFixture(t)
			l.Reviewer = &aheadContextReviewer{t: t}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return f, l, ctx
		}, "review", context.Canceled},
		{"key error", func(t *testing.T) (*landFixture, *Lander, context.Context) {
			f, l, _ := keyFixture(t)
			// A directory: the om config read fails, which is not an absent file.
			l.OMConfigPath = t.TempDir()
			return f, l, context.Background()
		}, "review key", nil},
		{"non-om verdict word", func(t *testing.T) (*landFixture, *Lander, context.Context) {
			f, l, _ := keyFixture(t)
			l.Reviewer = &aheadContextReviewer{t: t, verdict: Verdict{Verdict: VerdictSkipped}}
			return f, l, context.Background()
		}, "", nil},
		{"unknown verdict word", func(t *testing.T) (*landFixture, *Lander, context.Context) {
			f, l, _ := keyFixture(t)
			l.Reviewer = &aheadContextReviewer{t: t, verdict: Verdict{Verdict: "maybe"}}
			return f, l, context.Background()
		}, "", nil},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, l, ctx := tc.setup(t)
			l.Ahead = TownAheadStore(f.town, "gastown")
			var log bytes.Buffer
			l.Out = &log
			before := aheadBeadWrites(t, f.bd)
			got, built, err := l.reviewAhead(ctx, f.work)
			if tc.stage == "" {
				if err != nil {
					t.Fatalf("reviewAhead: %v, want no error and outcome %q", err, AheadSkipped)
				}
				if got != AheadSkipped {
					t.Errorf("reviewAhead = %q, want %q", got, AheadSkipped)
				}
				if !reflect.DeepEqual(built, aheadBuilt{}) {
					t.Errorf("reviewAhead built %+v for a verdict it did not store, want nothing", built)
				}
				if !strings.Contains(log.String(), "skipped") {
					t.Errorf("land log %q does not say the review was skipped", log.String())
				}
			} else {
				if err == nil {
					t.Fatalf("reviewAhead: got outcome %q and no error, want the %q error", got, tc.stage)
				}
				var infra *InfraError
				if !errors.As(err, &infra) || infra.Stage != tc.stage {
					t.Fatalf("reviewAhead error = %v, want an *InfraError at %q", err, tc.stage)
				}
				if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
					t.Errorf("reviewAhead error = %v, want it to wrap %v", err, tc.wantErr)
				}
				if got != "" {
					t.Errorf("reviewAhead outcome = %q on error, want empty (not %q)", got, AheadSkipped)
				}
				if !reflect.DeepEqual(built, aheadBuilt{}) {
					t.Errorf("reviewAhead built %+v on error, want nothing", built)
				}
			}
			if _, err := os.Stat(aheadTestPath(f.town, "gastown", f.work.BeadID)); !os.IsNotExist(err) {
				t.Errorf("a record was stored for a review that decided nothing (stat err %v)", err)
			}
			if n := aheadBeadWrites(t, f.bd) - before; n != 0 {
				t.Errorf("reviewAhead wrote %d bead records, want 0", n)
			}
			aheadNoReviewDir(t, f)
			if !l.gitMu.TryLock() {
				t.Errorf("gitMu is still held after reviewAhead returned")
			} else {
				l.gitMu.Unlock()
			}
		})
	}
}
