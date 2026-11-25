package land

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
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
		// clockReads is how many times ReviewAhead reads the store's clock: the
		// review's start is taken once it proceeds past the skips, and not at
		// all when it skips.
		clockReads int
	}{
		{"nil Ahead", false, aheadReadyIssue, 0},
		{"bead not actionable", true, func(w Work) beads.Issue {
			i := aheadReadyIssue(w)
			i.Status = "closed"
			return i
		}, 0},
		{"bead lacks ready label", true, func(w Work) beads.Issue {
			i := aheadReadyIssue(w)
			i.Labels = nil
			return i
		}, 0},
		{"policy refusal", true, func(w Work) beads.Issue {
			i := aheadReadyIssue(w)
			i.AcceptanceCriteria = "- [ ] add b"
			return i
		}, 0},
		{"waived head", true, func(w Work) beads.Issue {
			i := aheadReadyIssue(w)
			i.Labels = append(i.Labels, LabelOverseerReviewed)
			i.Notes = FormatReadyNote(w) + "\n" + FormatOverseerReviewedNote(w.Head)
			return i
		}, 0},
		// The healthy path: past every guard, through both fetches, to the
		// marked point where the worktree is added.
		{"ready submission", true, aheadReadyIssue, 1},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			aheadSeedBead(f, tc.issue(f.work))
			l := f.lander()
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
			if got != AheadSkipped {
				t.Errorf("ReviewAhead = %q, want %q", got, AheadSkipped)
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
	f := newLandFixture(t)
	aheadSeedBead(f, aheadReadyIssue(f.work))
	l := f.lander()
	l.Ahead = TownAheadStore(f.town, "gastown")
	rec := &aheadWorktreeAdds{}
	l.openRepo = func(dir string) Repo {
		return &aheadWorktreeRecorder{Repo: f.git.Open(dir), rec: rec}
	}
	got, err := l.ReviewAhead(context.Background(), f.work)
	if err != nil {
		t.Fatalf("ReviewAhead: unexpected error: %v", err)
	}
	if got != AheadSkipped {
		t.Errorf("ReviewAhead = %q, want %q", got, AheadSkipped)
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
	f := newLandFixture(t)
	aheadSeedBead(f, aheadReadyIssue(f.work))
	l := f.lander()
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
	f := newLandFixture(t)
	aheadSeedBead(f, aheadReadyIssue(f.work))
	l := f.lander()
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
	if got != AheadSkipped {
		t.Errorf("ReviewAhead = %q, want %q", got, AheadSkipped)
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
