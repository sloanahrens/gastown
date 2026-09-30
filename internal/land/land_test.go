package land

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/checkpoint"
	"github.com/steveyegge/gastown/internal/git/gitfake"
)

// landFixture is an origin with main and one pushed work branch, a clone
// the Lander adds worktrees from, and a work bead marked ready. The
// repositories live in a gitfake world; newRealLandFixture builds the same
// shape with real git for the integration tier (git is nil there).
type landFixture struct {
	t        *testing.T
	git      *gitfake.Fake
	origin   string
	repo     string // the lander's clone
	workRoot string
	town     string
	base     string // origin/main before landing
	work     Work
	bd       *beadsfake.Fake
	gate     *fakeGate
	review   *fakeReviewer

	realPushMain   func(name, body string) string
	realOriginMain func() string
	realParents    func(commit string) []string
}

const fixtureBranch = "polecat/opal/gt-abc+x1"

func newLandFixtureAt(t *testing.T) *landFixture {
	t.Helper()
	root := t.TempDir()
	return &landFixture{t: t, origin: filepath.Join(root, "origin.git"), repo: filepath.Join(root, "lander"),
		workRoot: filepath.Join(root, "work"), town: filepath.Join(root, "town")}
}

// newLandFixture builds origin with a.txt on main and a branch that adds
// b.txt, in a gitfake world.
func newLandFixture(t *testing.T) *landFixture {
	t.Helper()
	f := newLandFixtureAt(t)
	f.git = gitfake.New()
	f.git.InitBare(t, f.origin)
	f.base = f.git.Commit(t, f.origin, "main", "main: seed", map[string]string{"a.txt": "one\ntwo\nthree\n"})
	f.git.SetRef(t, f.origin, "refs/heads/"+fixtureBranch, f.base)
	head := f.git.Commit(t, f.origin, fixtureBranch, "feat: add b", map[string]string{"b.txt": "work\n"})
	f.git.Clone(t, f.origin, f.repo)
	f.ready(head)
	return f
}

// ready declares the work at head and seeds its bead, gate and reviewer.
func (f *landFixture) ready(head string) {
	f.work = Work{BeadID: "gt-abc", Rig: "gastown", Branch: fixtureBranch, Head: head, Target: "main", Worker: "opal"}
	f.bd = beadsfake.New(beadsfake.WithPrefix("gt"))
	f.bd.Seed(beads.Issue{ID: "gt-abc", Title: "add b", Status: "hooked", Type: "task", Assignee: "gastown/polecats/opal",
		Labels: []string{LabelReadyToLand}, Notes: FormatReadyNote(f.work)})
	f.gate = &fakeGate{fn: func(string) GateResult { return GateResult{Passed: true, Steps: []StepResult{{Name: "test"}}} }}
	f.review = &fakeReviewer{fn: func(string) (Verdict, error) { return Verdict{Verdict: VerdictApprove, Score: 0.9}, nil }}
}

// pushMain commits body to name on origin/main.
func (f *landFixture) pushMain(name, body string) string {
	f.t.Helper()
	if f.git == nil {
		return f.realPushMain(name, body)
	}
	return f.git.Commit(f.t, f.origin, "main", "main: "+name, map[string]string{name: body})
}

// setBranch points the work branch on origin at a new commit on base with
// message, carrying the branch's change, and declares it the head.
func (f *landFixture) setBranch(message string) {
	f.t.Helper()
	f.git.SetRef(f.t, f.origin, "refs/heads/"+fixtureBranch, f.base)
	head := f.git.Commit(f.t, f.origin, fixtureBranch, message, map[string]string{"b.txt": "work\n"})
	f.work.Head = head
}

func (f *landFixture) lander() *Lander {
	lf, err := RigLandingsFile(f.town, "gastown")
	if err != nil {
		f.t.Fatal(err)
	}
	l := &Lander{Repo: f.repo, WorkRoot: f.workRoot, Gate: f.gate, Reviewer: f.review, Beads: f.bd, Landings: lf,
		Now: func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) }}
	if f.git != nil {
		l.openRepo = func(dir string) Repo { return f.git.Open(dir) }
	}
	return l
}

func (f *landFixture) parents(commit string) []string {
	if f.git == nil {
		return f.realParents(commit)
	}
	return f.git.Parents(commit)
}

func (f *landFixture) originMain() string {
	if f.git == nil {
		return f.realOriginMain()
	}
	return f.git.Ref(f.origin, "refs/heads/main")
}

func (f *landFixture) bead() *beads.Issue {
	f.t.Helper()
	is, err := f.bd.Show("gt-abc")
	if err != nil {
		f.t.Fatal(err)
	}
	return is
}

func (f *landFixture) landingLines() []string {
	data, err := os.ReadFile(filepath.Join(f.town, ".runtime", "landings", "gastown.jsonl"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		f.t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

type fakeGate struct {
	mu   sync.Mutex
	fn   func(dir string) GateResult
	dirs []string
}

func (g *fakeGate) Run(_ context.Context, dir string) GateResult {
	g.mu.Lock()
	g.dirs = append(g.dirs, dir)
	g.mu.Unlock()
	return g.fn(dir)
}

type fakeReviewer struct {
	mu    sync.Mutex
	fn    func(dir string) (Verdict, error)
	calls [][3]string
}

func (r *fakeReviewer) Review(_ context.Context, dir, base, head string) (Verdict, error) {
	r.mu.Lock()
	r.calls = append(r.calls, [3]string{dir, base, head})
	r.mu.Unlock()
	return r.fn(dir)
}

// assertUntouched: nothing landed and the bead still waits to land.
func (f *landFixture) assertUntouched(t *testing.T) {
	t.Helper()
	if got := f.landingLines(); len(got) != 0 {
		t.Errorf("landings file written: %q", got)
	}
	b := f.bead()
	if b.Status == "closed" || !beads.HasLabel(b, LabelReadyToLand) || strings.Contains(b.Notes, MergeRejectionNoteMarker) {
		t.Errorf("bead changed: status=%s labels=%v notes=%q", b.Status, b.Labels, b.Notes)
	}
}

// assertRejected: origin/main did not move, and the bead carries one
// rejection of kind, is open, unassigned and labeled label.
func (f *landFixture) assertRejected(t *testing.T, err error, kind RejectionKind, label string) *Rejection {
	t.Helper()
	var rej *Rejection
	if !errors.As(err, &rej) || rej.Kind != kind {
		t.Fatalf("Land error = %T %v, want *Rejection of kind %s", err, err, kind)
	}
	if rej.RecordErr != nil {
		t.Fatalf("rejection not recorded: %v", rej.RecordErr)
	}
	if got := f.originMain(); got != f.base {
		t.Errorf("origin/main moved to %s on a rejection", got)
	}
	if got := f.landingLines(); len(got) != 0 {
		t.Errorf("landings file written on a rejection: %q", got)
	}
	b := f.bead()
	if b.Status != "open" || b.Assignee != "" || beads.HasLabel(b, LabelReadyToLand) || !beads.HasLabel(b, label) {
		t.Errorf("bead after rejection: status=%s assignee=%q labels=%v", b.Status, b.Assignee, b.Labels)
	}
	if CountRejections(b.Notes) != 1 || !strings.Contains(b.Notes, "MERGE REJECTION (attempt 1): "+string(kind)+" - ") ||
		!strings.Contains(b.Notes, "\nHead: "+f.work.Head) || !strings.Contains(b.Notes, "\nBranch: "+f.work.Branch) {
		t.Errorf("rejection note:\n%s", b.Notes)
	}
	return rej
}

func TestLandMergesGatesPushesAndRecords(t *testing.T) {
	t.Parallel()
	landsAndRecords(t, newLandFixture(t))
}

// landsAndRecords lands the fixture's work onto a main that moved on, and
// checks the merge, the gate and review, the push and the whole record. The
// integration tier runs it over real git.
func landsAndRecords(t *testing.T, f *landFixture) {
	moved := f.pushMain("c.txt", "other\n") // main moved on since the branch
	f.base = moved
	var gatedHasBoth bool
	f.gate.fn = func(dir string) GateResult {
		_, errB := os.Stat(filepath.Join(dir, "b.txt"))
		_, errC := os.Stat(filepath.Join(dir, "c.txt"))
		gatedHasBoth = errB == nil && errC == nil
		return GateResult{Passed: true, Steps: []StepResult{{Name: "test"}}}
	}
	res, err := f.lander().Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	if !gatedHasBoth {
		t.Error("the gate did not run on the merged tree (b.txt from the branch and c.txt from main)")
	}
	if got := f.originMain(); got != res.LandedCommit {
		t.Fatalf("origin/main = %s, want landed %s", got, res.LandedCommit)
	}
	if parents := f.parents(res.LandedCommit); len(parents) != 2 || parents[0] != moved || parents[1] != f.work.Head {
		t.Fatalf("landed commit parents = %v, want [%s %s]", parents, moved, f.work.Head)
	}
	if res.Base != moved || res.PatchID == "" {
		t.Fatalf("result = %+v", res)
	}
	rc := f.review.calls
	if len(rc) != 1 || rc[0][1] != moved || rc[0][2] != res.LandedCommit || rc[0][0] != f.gate.dirs[0] {
		t.Fatalf("review calls %v, gate dirs %v: review must see the same tree and range", rc, f.gate.dirs)
	}
	if _, err := os.Stat(f.gate.dirs[0]); !os.IsNotExist(err) {
		t.Errorf("throwaway worktree %s left behind", f.gate.dirs[0])
	}
	b := f.bead()
	if b.Status != "closed" || beads.HasLabel(b, LabelReadyToLand) {
		t.Errorf("bead status=%s labels=%v", b.Status, b.Labels)
	}
	if !strings.Contains(b.CloseReason, "landed_commit: "+res.LandedCommit) || !strings.Contains(b.CloseReason, "patch_id: "+res.PatchID) {
		t.Errorf("close reason = %q", b.CloseReason)
	}
	if strings.Count(b.Notes, LandingNoteMarker) != 1 || !strings.Contains(b.Notes, "om_verdict: approve") || !strings.Contains(b.Notes, "route: daemon") {
		t.Errorf("notes = %q", b.Notes)
	}
	lines := f.landingLines()
	if len(lines) != 1 {
		t.Fatalf("landings lines = %q", lines)
	}
	var rec LandingRecord
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.LandedCommit != res.LandedCommit || rec.PatchID != res.PatchID || rec.BeadID != "gt-abc" || rec.Head != f.work.Head || rec.OMScore != 0.9 {
		t.Errorf("landing record = %+v", rec)
	}
}

// An auto-save commit in the range is squashed away: the landed commit has
// one parent, main, and carries the branch's whole change.
func TestLandSquashesARangeWithAutoSaveCommits(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	head := f.git.Commit(t, f.origin, fixtureBranch, checkpoint.WIPCommitPrefix+" 2026-09-30", map[string]string{"c.txt": "wip\n"})
	f.work.Head = head
	res, err := f.lander().Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	if parents := f.git.Parents(res.LandedCommit); len(parents) != 1 || parents[0] != f.base {
		t.Fatalf("landed commit parents = %v, want the squash's single parent %s", parents, f.base)
	}
	if tree := f.git.Tree(res.LandedCommit); tree["b.txt"] != "work\n" || tree["c.txt"] != "wip\n" {
		t.Fatalf("landed tree = %v, want the branch's b.txt and c.txt", tree)
	}
	if f.originMain() != res.LandedCommit {
		t.Fatalf("origin/main = %s, want %s", f.originMain(), res.LandedCommit)
	}
}

func TestLandConflictIsARejectionWithFiles(t *testing.T) {
	t.Parallel()
	conflictIsARejection(t, newLandFixture(t))
}

// conflictIsARejection lands a branch that conflicts with main. The
// integration tier runs it over real git.
func conflictIsARejection(t *testing.T, f *landFixture) {
	// The branch and main both write b.txt differently.
	f.base = f.pushMain("b.txt", "main's b\n")
	_, err := f.lander().Land(context.Background(), f.work)
	rej := f.assertRejected(t, err, RejectConflict, LabelRework)
	if len(rej.Conflicting) != 1 || rej.Conflicting[0] != "b.txt" {
		t.Errorf("conflicting = %v", rej.Conflicting)
	}
	if !strings.Contains(f.bead().Notes, "\nConflicting: b.txt") {
		t.Errorf("notes lack the file list:\n%s", f.bead().Notes)
	}
	if len(f.gate.dirs) != 0 {
		t.Error("a conflicted merge was gated")
	}
}

func TestLandRedGateNeverPushes(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.gate.fn = func(string) GateResult {
		return GateResult{Steps: []StepResult{{Name: "test", ExitCode: 2, Tail: "--- FAIL: TestB\nFAIL\tgithub.com/x/b\n"}}}
	}
	_, err := f.lander().Land(context.Background(), f.work)
	f.assertRejected(t, err, RejectGate, LabelRework)
	if !strings.Contains(f.bead().Notes, "\n  | FAIL\tgithub.com/x/b") {
		t.Errorf("notes lack the gate tail:\n%s", f.bead().Notes)
	}
}

func TestLandRequestChangesRejectsWithFindings(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.review.fn = func(string) (Verdict, error) {
		return Verdict{Verdict: VerdictRequestChanges, Score: 0.3, Summary: "b.txt is wrong.", Findings: []Finding{{ID: "abc123", Severity: "major", Path: "b.txt", Line: 1, Title: "wrong"}}}, nil
	}
	_, err := f.lander().Land(context.Background(), f.work)
	rej := f.assertRejected(t, err, RejectReview, LabelRework)
	if !rej.Rework || rej.ReviewSummary != "b.txt is wrong." || rej.ReviewScore != 0.3 || len(rej.Findings) != 1 {
		t.Errorf("rejection does not carry om's verdict: %+v", rej)
	}
	notes := f.bead().Notes
	if !strings.Contains(notes, "- id:abc123 sev:major b.txt:1 — wrong") || !strings.Contains(notes, "Score: 0.3000") {
		t.Errorf("notes lack the findings:\n%s", notes)
	}
}

func TestLandLostLeaseIsRaceErrorAndLeavesBead(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	var raced string
	f.gate.fn = func(string) GateResult {
		raced = f.pushMain("c.txt", "someone else landed\n")
		return GateResult{Passed: true, Steps: []StepResult{{Name: "test"}}}
	}
	_, err := f.lander().Land(context.Background(), f.work)
	var race *RaceError
	if !errors.As(err, &race) || race.Expected != f.base || race.Actual != raced {
		t.Fatalf("Land error = %T %v, want *RaceError %s -> %s", err, err, f.base, raced)
	}
	if got := f.originMain(); got != raced {
		t.Errorf("origin/main = %s, the other landing %s was clobbered", got, raced)
	}
	f.assertUntouched(t)
}

func TestLandReadBackFailureWritesNoRecord(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	l.afterPush = func() { f.git.SetRef(t, f.origin, "refs/heads/main", f.base) }
	_, err := l.Land(context.Background(), f.work)
	if !errors.Is(err, ErrReadBack) {
		t.Fatalf("Land error = %v, want ErrReadBack", err)
	}
	f.assertUntouched(t)
}

func TestLandRefusesEmptyMerge(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	// main already carries the branch's exact change under another commit.
	f.base = f.pushMain("b.txt", "work\n")
	_, err := f.lander().Land(context.Background(), f.work)
	rej := f.assertRejected(t, err, RejectEmpty, LabelRework)
	if !strings.Contains(rej.Reason, "empty merge") {
		t.Errorf("reason = %q", rej.Reason)
	}
}

func TestLandPolicyRejections(t *testing.T) {
	t.Parallel()
	t.Run("no_merge needs a human", func(t *testing.T) {
		t.Parallel()
		f := newLandFixture(t)
		f.bd.Seed(beads.Issue{ID: "gt-abc", Title: "add b", Status: "hooked", Type: "task", Description: "no_merge: true",
			Labels: []string{LabelReadyToLand}, Notes: FormatReadyNote(f.work)})
		_, err := f.lander().Land(context.Background(), f.work)
		rej := f.assertRejected(t, err, RejectPolicy, LabelNeedsHuman)
		if rej.Rework || !strings.Contains(rej.Reason, "no_merge") {
			t.Errorf("rejection = %+v", rej)
		}
	})
	t.Run("unchecked criteria go back for rework", func(t *testing.T) {
		t.Parallel()
		f := newLandFixture(t)
		f.bd.Seed(beads.Issue{ID: "gt-abc", Title: "add b", Status: "hooked", Type: "task", AcceptanceCriteria: "- [x] one\n- [ ] two\n",
			Labels: []string{LabelReadyToLand}, Notes: FormatReadyNote(f.work)})
		_, err := f.lander().Land(context.Background(), f.work)
		rej := f.assertRejected(t, err, RejectPolicy, LabelRework)
		if !rej.Rework || !strings.Contains(rej.Reason, "1 unchecked acceptance criteria") {
			t.Errorf("rejection = %+v", rej)
		}
	})
}

func TestLandHeadMustBeOnOrigin(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	// A head the author never pushed: origin/<branch> does not carry it.
	f.work.Head = strings.Repeat("1", 40)
	_, err := f.lander().Land(context.Background(), f.work)
	f.assertRejected(t, err, RejectNotPushed, LabelRework)
}

func TestLandNotReadyWritesNothing(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.bd.Seed(beads.Issue{ID: "gt-abc", Title: "add b", Status: "hooked", Type: "task", Notes: FormatReadyNote(f.work)})
	_, err := f.lander().Land(context.Background(), f.work)
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("Land error = %v, want ErrNotReady", err)
	}
	if got := f.originMain(); got != f.base {
		t.Error("origin/main moved")
	}
}

func TestLandGateInfraErrorIsNotARejection(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.gate.fn = func(string) GateResult { return GateResult{Err: errors.New("container slot unavailable")} }
	_, err := f.lander().Land(context.Background(), f.work)
	var infra *InfraError
	if !errors.As(err, &infra) || infra.Stage != "gate" {
		t.Fatalf("Land error = %T %v, want *InfraError at gate", err, err)
	}
	f.assertUntouched(t)

	f2 := newLandFixture(t)
	f2.review.fn = func(string) (Verdict, error) { return Verdict{}, errors.New("om exited 2") }
	_, err = f2.lander().Land(context.Background(), f2.work)
	if !errors.As(err, &infra) || infra.Stage != "review" {
		t.Fatalf("Land error = %T %v, want *InfraError at review", err, err)
	}
	f2.assertUntouched(t)
}

func TestLandRunsGateAndReviewConcurrently(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	gateIn, reviewIn := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f.gate.fn = func(string) GateResult {
		close(gateIn)
		select {
		case <-reviewIn:
			return GateResult{Passed: true}
		case <-ctx.Done():
			return GateResult{Err: errors.New("review never started while the gate ran")}
		}
	}
	f.review.fn = func(string) (Verdict, error) {
		close(reviewIn)
		select {
		case <-gateIn:
			return Verdict{Verdict: VerdictApprove}, nil
		case <-ctx.Done():
			return Verdict{}, errors.New("gate never started while the review ran")
		}
	}
	if _, err := f.lander().Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land: %v", err)
	}
}

// flakyCloseBeads fails the first close, as a Dolt blip after the push would.
type flakyCloseBeads struct {
	*beadsfake.Fake
	failed bool
}

func (b *flakyCloseBeads) ForceCloseWithReason(reason string, ids ...string) error {
	if !b.failed {
		b.failed = true
		return errors.New("database is locked")
	}
	return b.Fake.ForceCloseWithReason(reason, ids...)
}

// TestLandRepairsAnIncompleteRecord: the work landed but the close failed. The
// next Land of the same bead finds its own line in the landings file and
// finishes the record instead of landing twice or rejecting landed work.
func TestLandRepairsAnIncompleteRecord(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	l.Beads = &flakyCloseBeads{Fake: f.bd}
	res, err := l.Land(context.Background(), f.work)
	var rec *RecordError
	if !errors.As(err, &rec) || rec.Result.LandedCommit != f.originMain() {
		t.Fatalf("first Land = %v, want a RecordError for the landed %s", err, f.originMain())
	}
	if f.bead().Status == "closed" {
		t.Fatal("the failed close closed the bead")
	}
	res2, err := l.Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("repair Land: %v", err)
	}
	if res2.LandedCommit != res.LandedCommit || res2.PatchID != res.PatchID || f.originMain() != res.LandedCommit {
		t.Fatalf("repair landed again: %+v vs %+v, origin %s", res2, res, f.originMain())
	}
	b := f.bead()
	if b.Status != "closed" || strings.Count(b.Notes, LandingNoteMarker) != 1 || CountRejections(b.Notes) != 0 {
		t.Errorf("after repair: status=%s notes=%q", b.Status, b.Notes)
	}
	if n := len(f.landingLines()); n != 1 {
		t.Errorf("landings lines = %d, want 1", n)
	}
	if len(f.gate.dirs) != 1 {
		t.Errorf("the repair gated again: %d gate runs", len(f.gate.dirs))
	}
}

// TestLandHeadAlreadyOnTargetNeedsAHuman: the head reached the target by some
// route that left no landing record (an operator push). That is not the
// author's fault, so it is not rework.
func TestLandHeadAlreadyOnTargetNeedsAHuman(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.git.SetRef(t, f.origin, "refs/heads/main", f.work.Head)
	f.base = f.work.Head
	_, err := f.lander().Land(context.Background(), f.work)
	rej := f.assertRejected(t, err, RejectEmpty, LabelNeedsHuman)
	if rej.Rework || !strings.Contains(rej.Reason, "already reachable") {
		t.Errorf("rejection = %+v", rej)
	}
}

// TestLandUnknownVerdictIsInfra: a reviewer that returns no recognizable
// verdict and no error produced no verdict; it is never read as a rejection.
func TestLandUnknownVerdictIsInfra(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.review.fn = func(string) (Verdict, error) { return Verdict{Verdict: "maybe"}, nil }
	_, err := f.lander().Land(context.Background(), f.work)
	var infra *InfraError
	if !errors.As(err, &infra) || infra.Stage != "review" {
		t.Fatalf("Land error = %T %v, want *InfraError at review", err, err)
	}
	f.assertUntouched(t)
}

// TestLandRejectionLeavesABeadThatChangedHands: someone claimed the bead
// while the gate ran. The rejection is noted, but the bead is not reopened or
// unassigned out from under them.
func TestLandRejectionLeavesABeadThatChangedHands(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.gate.fn = func(string) GateResult {
		mayor := "mayor"
		if err := f.bd.Update("gt-abc", beads.UpdateOptions{Assignee: &mayor, Force: true}); err != nil {
			t.Error(err)
		}
		return GateResult{Steps: []StepResult{{Name: "test", ExitCode: 1}}}
	}
	_, err := f.lander().Land(context.Background(), f.work)
	var rej *Rejection
	if !errors.As(err, &rej) || rej.Kind != RejectGate || rej.RecordErr == nil {
		t.Fatalf("Land error = %v, want a gate rejection whose reopen was withheld", err)
	}
	b := f.bead()
	if b.Assignee != "mayor" || b.Status == "open" || beads.HasLabel(b, LabelRework) {
		t.Errorf("bead taken from its new holder: status=%s assignee=%q labels=%v", b.Status, b.Assignee, b.Labels)
	}
	if CountRejections(b.Notes) != 1 {
		t.Errorf("rejection not noted: %q", b.Notes)
	}
}

// failingBeads fails AppendNotes, or Show after the first call.
type failingBeads struct {
	*beadsfake.Fake
	failNotes bool
	shows     int
	failShow  bool
}

func (b *failingBeads) AppendNotes(id, note string) error {
	if b.failNotes {
		return errors.New("database is locked")
	}
	return b.Fake.AppendNotes(id, note)
}

func (b *failingBeads) Show(id string) (*beads.Issue, error) {
	b.shows++
	if b.failShow && b.shows > 1 {
		return nil, errors.New("dolt: connection refused")
	}
	return b.Fake.Show(id)
}

// A rejection whose write failed still reports as a rejection, with the
// failure on RecordErr and in Error(), and leaves the bead ready: the caller
// sees the alarm instead of a clean rejection.
func TestLandRejectionRecordFailuresAreObservable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		b    func(*beadsfake.Fake) Beads
		want string
	}{
		{"note write fails", func(f *beadsfake.Fake) Beads { return &failingBeads{Fake: f, failNotes: true} }, "appending the rejection note"},
		{"re-read fails", func(f *beadsfake.Fake) Beads { return &failingBeads{Fake: f, failShow: true} }, "re-reading the bead"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			f.gate.fn = func(string) GateResult { return GateResult{Steps: []StepResult{{Name: "test", ExitCode: 1}}} }
			l := f.lander()
			l.Beads = tc.b(f.bd)
			_, err := l.Land(context.Background(), f.work)
			var rej *Rejection
			if !errors.As(err, &rej) || rej.RecordErr == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Land error = %v, want a rejection carrying %q", err, tc.want)
			}
			if b := f.bead(); b.Status == "open" || !beads.HasLabel(b, LabelReadyToLand) {
				t.Errorf("bead changed although the rejection write failed: status=%s labels=%v", b.Status, b.Labels)
			}
		})
	}
}
