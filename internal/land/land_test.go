package land

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
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
	ids  []string // LandingID(ctx) per run
}

func (g *fakeGate) Run(ctx context.Context, dir string) GateResult {
	g.mu.Lock()
	g.dirs = append(g.dirs, dir)
	g.ids = append(g.ids, LandingID(ctx))
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

// TestLandReusesOneWorktreePath is gt-2ycne.2: Go's build cache keys include
// the package directory, so a worktree at a new random path per landing
// recompiled every package and wrote a full new cache generation each time
// (~470 GB). Every landing checks out at the same WorkRoot/wt; what tells two
// landings apart (their log directories) is LandingID on the gate's context.
func TestLandReusesOneWorktreePath(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	red := true
	f.gate.fn = func(string) GateResult {
		if red {
			red = false
			return GateResult{Passed: false, Steps: []StepResult{{Name: "test", ExitCode: 1}}}
		}
		return GateResult{Passed: true, Steps: []StepResult{{Name: "test"}}}
	}
	if _, err := f.lander().Land(context.Background(), f.work); err == nil || !strings.Contains(err.Error(), "landing rejected (gate)") {
		t.Fatalf("first Land (red gate) = %v, want a gate rejection", err)
	}
	f.ready(f.work.Head)
	f.gate.fn = func(string) GateResult { return GateResult{Passed: true, Steps: []StepResult{{Name: "test"}}} }
	if _, err := f.lander().Land(context.Background(), f.work); err != nil {
		t.Fatalf("second Land: %v", err)
	}
	want := filepath.Join(f.workRoot, "wt")
	// f.ready replaced f.gate; the first landing's run is on the first gate.
	if len(f.gate.dirs) != 1 || f.gate.dirs[0] != want {
		t.Fatalf("second landing gated in %v, want [%s]", f.gate.dirs, want)
	}
	if f.gate.ids[0] == "" {
		t.Fatal("the gate's context carries no LandingID")
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Errorf("worktree %s left behind after the landing (stat err %v)", want, err)
	}
}

// TestLandReusesTheWorktreePathPastALeftover: a landing that died before its
// cleanup leaves WorkRoot/wt behind; the next landing clears it, not fails.
func TestLandReusesTheWorktreePathPastALeftover(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	leftover := filepath.Join(f.workRoot, "wt")
	if err := os.MkdirAll(leftover, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(leftover, "stale.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	var sawStale bool
	f.gate.fn = func(dir string) GateResult {
		_, err := os.Stat(filepath.Join(dir, "stale.txt"))
		sawStale = err == nil
		return GateResult{Passed: true, Steps: []StepResult{{Name: "test"}}}
	}
	if _, err := f.lander().Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land past a leftover worktree: %v", err)
	}
	if sawStale {
		t.Error("the gate saw the dead landing's files")
	}
}

// TestLandingIDsDiffer: two landings get two log directories.
func TestLandingIDsDiffer(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	g := f.gate
	g.fn = func(string) GateResult {
		return GateResult{Passed: false, Steps: []StepResult{{Name: "test", ExitCode: 1}}}
	}
	for i := 0; i < 2; i++ {
		f.ready(f.work.Head)
		f.gate = g
		if _, err := f.lander().Land(context.Background(), f.work); err == nil || !strings.Contains(err.Error(), "landing rejected (gate)") {
			t.Fatalf("Land %d (red gate) = %v, want a gate rejection", i+1, err)
		}
	}
	if len(g.ids) != 2 || g.ids[0] == "" || g.ids[0] == g.ids[1] {
		t.Fatalf("landing IDs = %q, want two distinct non-empty IDs", g.ids)
	}
}

func TestLandMergesGatesPushesAndRecords(t *testing.T) {
	t.Parallel()
	landsAndRecords(t, newLandFixture(t))
}

// gt-84gcp: a landing reports each stage as it enters it, so the daemon's
// landing-stuck alarm can judge the stage the pass is running rather than the
// pipeline as a whole. The stages before the gate are reported by nothing:
// they have no timeout of their own.
func TestLandReportsItsStages(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	var stages []string
	l := f.lander()
	l.Stage = func(beadID, stage string) {
		if beadID != f.work.BeadID {
			t.Errorf("stage %q reported for bead %q, want %q", stage, beadID, f.work.BeadID)
		}
		stages = append(stages, stage)
	}
	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land: %v", err)
	}
	if want := []string{StageGate, StageOM}; !slices.Equal(stages, want) {
		t.Fatalf("stages = %v, want %v", stages, want)
	}
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

// TestLandCapsTheReworkLoop is gt-28ibg: the rejection at MaxReworkAttempts
// goes to a human instead of back to a polecat, so a refusal the author keeps
// reproducing ends in an escalation rather than another redispatch.
func TestLandCapsTheReworkLoop(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.gate.fn = func(string) GateResult {
		return GateResult{Steps: []StepResult{{Name: "test", ExitCode: 2}}}
	}
	l := f.lander()
	for attempt := 1; attempt <= MaxReworkAttempts; attempt++ {
		_, err := l.Land(context.Background(), f.work)
		var rej *Rejection
		if !errors.As(err, &rej) || rej.Kind != RejectGate {
			t.Fatalf("attempt %d: Land = %T %v, want a gate rejection", attempt, err, err)
		}
		if rej.RecordErr != nil {
			t.Fatalf("attempt %d: rejection not recorded: %v", attempt, rej.RecordErr)
		}
		capped := attempt >= MaxReworkAttempts
		if rej.Rework == capped {
			t.Errorf("attempt %d: rej.Rework = %v, want %v", attempt, rej.Rework, !capped)
		}
		b := f.bead()
		wantLabel := LabelRework
		if capped {
			wantLabel = LabelNeedsHuman
		}
		if !beads.HasLabel(b, wantLabel) || beads.HasLabel(b, LabelRework) == capped || beads.HasLabel(b, LabelReadyToLand) {
			t.Errorf("attempt %d: labels after the rejection = %v, want %s", attempt, b.Labels, wantLabel)
		}
		if got := CountRejections(b.Notes); got != attempt {
			t.Errorf("attempt %d: %d rejection block(s) in the notes", attempt, got)
		}
		if capped != strings.Contains(b.Notes, "the loop is escalated") {
			t.Errorf("attempt %d: capped=%v, notes:\n%s", attempt, capped, b.Notes)
		}
		// The next round is a fresh submission of the same head, which is what
		// a redispatched polecat's gt done produces.
		if err := f.bd.Update(f.work.BeadID, beads.UpdateOptions{AddLabels: []string{LabelReadyToLand}}); err != nil {
			t.Fatalf("attempt %d: re-submitting: %v", attempt, err)
		}
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

// TestLandRefusesAParkedBead is the defer that arrives while the gate ran: the
// worker read the bead before the park and lands nothing it can no longer see
// (gt-y7n1u).
func TestLandRefusesAParkedBead(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.bd.Seed(beads.Issue{ID: "gt-abc", Title: "add b", Status: "deferred", Type: "task", Assignee: "gastown/polecats/opal",
		Labels: []string{LabelReadyToLand}, Notes: FormatReadyNote(f.work)})
	_, err := f.lander().Land(context.Background(), f.work)
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("Land error = %v, want ErrNotReady", err)
	}
	f.assertUntouched(t)
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

// TestLandReviewsOnlyAfterTheGatePasses: the gate's stages run first and om
// only once they pass, so a red gate never pays for a review (gt-b5ugw).
func TestLandReviewsOnlyAfterTheGatePasses(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	var order []string
	f.gate.fn = func(string) GateResult {
		order = append(order, "gate")
		return GateResult{Passed: true, Steps: []StepResult{{Name: "lint"}, {Name: "gate"}}}
	}
	f.review.fn = func(string) (Verdict, error) {
		order = append(order, "om")
		return Verdict{Verdict: VerdictApprove}, nil
	}
	if _, err := f.lander().Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land: %v", err)
	}
	if !slices.Equal(order, []string{"gate", "om"}) {
		t.Errorf("order = %v, want the gate then om", order)
	}

	red := newLandFixture(t)
	red.gate.fn = func(string) GateResult {
		return GateResult{Steps: []StepResult{{Name: "lint", ExitCode: 1}}}
	}
	red.review.fn = func(string) (Verdict, error) {
		t.Error("om ran on a tree that failed lint")
		return Verdict{Verdict: VerdictApprove}, nil
	}
	_, err := red.lander().Land(context.Background(), red.work)
	red.assertRejected(t, err, RejectGate, LabelRework)
}

// TestLandStageTimeoutRejects: a test stage killed by its own timeout rejects
// the landing as a timeout naming the stage, to a human (the author cannot
// tell a hang from a loaded host by editing), and om never runs (gt-b5ugw).
func TestLandStageTimeoutRejects(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.gate.fn = func(string) GateResult {
		return GateResult{Steps: []StepResult{{Name: "lint"}, {Name: "gate", Command: "make gate-test", ExitCode: -1, TimedOut: true, Timeout: 6 * time.Minute}}}
	}
	f.review.fn = func(string) (Verdict, error) {
		t.Error("om ran after a timed-out stage")
		return Verdict{Verdict: VerdictApprove}, nil
	}
	_, err := f.lander().Land(context.Background(), f.work)
	rej := f.assertRejected(t, err, RejectTimeout, LabelNeedsHuman)
	if !strings.Contains(rej.Reason, "make gate-test") || !strings.Contains(rej.Reason, "6m0s") {
		t.Errorf("reason = %q, want the stage and its timeout named", rej.Reason)
	}
}

// TestLandOverseerReviewedHeadSkipsOM: a bead whose exact head the overseer
// reviewed lands after the gate without om, recording the overseer's verdict;
// a review of another head runs om as usual (gt-g8t3m).
func TestLandOverseerReviewedHeadSkipsOM(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	if err := f.bd.AppendNotes("gt-abc", OverseerReviewedMarker+" "+f.work.Head); err != nil {
		t.Fatal(err)
	}
	if err := f.bd.Update("gt-abc", beads.UpdateOptions{AddLabels: []string{LabelOverseerReviewed}}); err != nil {
		t.Fatal(err)
	}
	gated := false
	f.gate.fn = func(string) GateResult {
		gated = true
		return GateResult{Passed: true, Steps: []StepResult{{Name: "gate"}}}
	}
	f.review.fn = func(string) (Verdict, error) {
		t.Error("om ran on a head the overseer reviewed")
		return Verdict{}, errors.New("unreachable")
	}
	res, err := f.lander().Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	if !gated {
		t.Error("the gate did not run: an overseer review skips om only")
	}
	if !strings.HasPrefix(res.Verdict.Verdict, VerdictOverseerPrefix) || f.originMain() != res.LandedCommit {
		t.Fatalf("verdict %q landed=%v, want overseer:<sha> and a landing", res.Verdict.Verdict, f.originMain() == res.LandedCommit)
	}

	other := newLandFixture(t)
	if err := other.bd.AppendNotes("gt-abc", OverseerReviewedMarker+" 1111111111111111111111111111111111111111"); err != nil {
		t.Fatal(err)
	}
	if err := other.bd.Update("gt-abc", beads.UpdateOptions{AddLabels: []string{LabelOverseerReviewed}}); err != nil {
		t.Fatal(err)
	}
	reviewed := false
	other.review.fn = func(string) (Verdict, error) { reviewed = true; return Verdict{Verdict: VerdictApprove}, nil }
	if _, err := other.lander().Land(context.Background(), other.work); err != nil {
		t.Fatalf("Land: %v", err)
	}
	if !reviewed {
		t.Error("om did not run although the overseer reviewed a different head")
	}
}

// TestLandLintTimeoutIsInfra: a lint stage over its timeout is waiting on
// the lint lock, not judging the tree: nothing is written to the bead and the
// next pass retries (gt-b5ugw review).
func TestLandLintTimeoutIsInfra(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.gate.fn = func(string) GateResult {
		return GateResult{Steps: []StepResult{{Name: "lint", Command: "make gate-lint", ExitCode: -1, TimedOut: true, Timeout: 2 * time.Minute}}}
	}
	_, err := f.lander().Land(context.Background(), f.work)
	var infra *InfraError
	if !errors.As(err, &infra) || infra.Stage != "gate" {
		t.Fatalf("Land error = %T %v, want *InfraError at gate", err, err)
	}
	if !errors.Is(err, ErrLintTimeout) {
		t.Errorf("Land error = %v; want it to wrap ErrLintTimeout so the worker can count it", err)
	}
	f.assertUntouched(t)
}

// TestLandRevertLandsWhenOMHasNoVerdict: a revert of a red main lands a green
// tree with om_verdict error rather than wait on om (gt-b5ugw review).
func TestLandRevertLandsWhenOMHasNoVerdict(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	if err := f.bd.Update("gt-abc", beads.UpdateOptions{AddLabels: []string{"gt:revert"}}); err != nil {
		t.Fatal(err)
	}
	f.review.fn = func(string) (Verdict, error) { return Verdict{}, ErrOMTimeout }
	l := f.lander()
	l.ReviewErrorRejects = true
	l.ReviewErrorLandsLabels = []string{"gt:revert"}
	res, err := l.Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land: %v; a revert must not wait on om", err)
	}
	if !strings.HasPrefix(res.Verdict.Verdict, VerdictErrorPrefix) || f.originMain() != res.LandedCommit {
		t.Fatalf("verdict %q, want error:<reason> and a landing", res.Verdict.Verdict)
	}
	var rec LandingRecord
	if err := json.Unmarshal([]byte(f.landingLines()[0]), &rec); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(rec.OMVerdict, VerdictErrorPrefix) {
		t.Fatalf("landings file om_verdict = %q; want error:<reason>", rec.OMVerdict)
	}
}

// TestLandOMBypassIsScopedToTheLabel: ReviewErrorLandsLabels frees only the
// beads that carry the label. A bead without gt:revert whose om returned no
// verdict still goes to gt:needs-human and never lands unreviewed (gt-j8ade).
func TestLandOMBypassIsScopedToTheLabel(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.review.fn = func(string) (Verdict, error) { return Verdict{}, ErrOMTimeout }
	l := f.lander()
	l.ReviewErrorRejects = true
	l.ReviewErrorLandsLabels = []string{"gt:revert"}
	_, err := l.Land(context.Background(), f.work)
	rej := f.assertRejected(t, err, RejectReview, LabelNeedsHuman)
	if rej.Rework {
		t.Errorf("rejection = %+v; an om error is not rework", rej)
	}
}

// TestLandReviewErrorRejectsWhenItMayNotLand: with ReviewErrorRejects, a
// green tree whose om produced no verdict is rejected to a human rather than
// landed unreviewed or retried by the next pass (gt-b5ugw).
func TestLandReviewErrorRejectsWhenItMayNotLand(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.review.fn = func(string) (Verdict, error) {
		return Verdict{}, errors.New("attempt 1: timed out; attempt 2: timed out")
	}
	l := f.lander()
	l.ReviewErrorRejects = true
	_, err := l.Land(context.Background(), f.work)
	rej := f.assertRejected(t, err, RejectReview, LabelNeedsHuman)
	if rej.Rework || !strings.Contains(rej.Reason, "timed out") {
		t.Errorf("rejection = %+v", rej)
	}
	if f.originMain() != f.base {
		t.Error("origin/main moved: the tree landed unreviewed")
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

// redShellGate is a merged tree whose gate stages passed and whose shell step
// failed, naming the scripts tier-sweep reported (gt-vsct7.8).
func redShellGate(string) GateResult {
	return GateResult{Steps: []StepResult{
		{Name: "lint", Command: "make gate-lint"},
		{Name: "gate", Command: "make gate-test"},
		{Name: ShellStepName, Command: ShellStepCommand, ExitCode: 1,
			ShellFailures: []string{"scripts/a_test.sh", "plugins/b_test.sh"},
			Tail:          "tier-sweep: shell RED passed=8 failed=2 skipped=0 failed: scripts/a_test.sh plugins/b_test.sh (logs /tmp/tier-sweep.aB12)\n"},
	}}
}

// TestLandShellTierFailureRejectsAndNamesTheScripts: a red shell step rejects
// the landing with the scripts the tier named, the flake policy reruns nothing
// (the step named no Go package), and om never runs (gt-vsct7.8).
func TestLandShellTierFailureRejectsAndNamesTheScripts(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.gate.fn = redShellGate
	f.review.fn = func(string) (Verdict, error) {
		t.Error("om ran after a red shell step")
		return Verdict{Verdict: VerdictApprove}, nil
	}
	reran := false
	l := f.lander()
	l.Rerun = func(context.Context, string, []string) GateResult {
		reran = true
		return GateResult{Passed: true}
	}
	_, err := l.Land(context.Background(), f.work)
	rej := f.assertRejected(t, err, RejectGate, LabelRework)
	if reran {
		t.Error("the flake policy reran a step that named no Go package")
	}
	for _, want := range []string{"the shell tier failed:", "scripts/a_test.sh", "plugins/b_test.sh"} {
		if !strings.Contains(rej.Reason, want) {
			t.Errorf("reason = %q, want it to name %q", rej.Reason, want)
		}
	}
}

// TestLandShellTierTimeoutRejects: the shell step killed by its own timeout
// takes the existing timeout path - a rejection naming the step, to a human,
// with om never running (gt-vsct7.8).
func TestLandShellTierTimeoutRejects(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.gate.fn = func(string) GateResult {
		return GateResult{Steps: []StepResult{
			{Name: "lint", Command: "make gate-lint"},
			{Name: "gate", Command: "make gate-test"},
			{Name: ShellStepName, Command: ShellStepCommand, ExitCode: -1, TimedOut: true, Timeout: 3 * time.Minute},
		}}
	}
	f.review.fn = func(string) (Verdict, error) {
		t.Error("om ran after a timed-out shell step")
		return Verdict{Verdict: VerdictApprove}, nil
	}
	_, err := f.lander().Land(context.Background(), f.work)
	rej := f.assertRejected(t, err, RejectTimeout, LabelNeedsHuman)
	if !strings.Contains(rej.Reason, ShellStepCommand) || !strings.Contains(rej.Reason, "3m0s") {
		t.Errorf("reason = %q, want the shell step and its timeout named", rej.Reason)
	}
}

// TestStageTimesReportsTheShellStep: a landing that ran the shell tier says
// how long it took, beside the other stages (gt-vsct7.8).
func TestStageTimesReportsTheShellStep(t *testing.T) {
	t.Parallel()
	got := stageTimes(GateResult{Steps: []StepResult{
		{Name: "lint", Elapsed: 18 * time.Second},
		{Name: "gate", Elapsed: 92 * time.Second},
		{Name: ShellStepName, Elapsed: 12 * time.Second},
	}}, 0, false)
	if !strings.Contains(got, "shell 12s") {
		t.Errorf("stageTimes = %q, want the shell step's wall time", got)
	}
}

// TestStageTimesMarksARetriedOMRun: the stages line carries the retry, so a
// flaky reviewer is visible in daemon.log without reading the error beside it
// (gt-q241r).
func TestStageTimesMarksARetriedOMRun(t *testing.T) {
	t.Parallel()
	got := stageTimes(GateResult{Steps: []StepResult{{Name: "gate", Elapsed: 33 * time.Second}}}, 90*time.Second, true)
	if !strings.Contains(got, "om 1m30s (retried)") {
		t.Errorf("stageTimes = %q, want the retried om stage marked", got)
	}
	if plain := stageTimes(GateResult{}, 90*time.Second, false); strings.Contains(plain, "retried") {
		t.Errorf("stageTimes = %q; an unretried om run must not say retried", plain)
	}
}

// TestStageTimesEndsWithTheHostLoad1: the stages line carries the load its
// stages ran under, so a slow gate is read against its host from daemon.log
// alone; a host that reports no load leaves the old line whole (gt-a025o).
func TestStageTimesEndsWithTheHostLoad1(t *testing.T) {
	t.Parallel()
	g := GateResult{Steps: []StepResult{{Name: "gate", Elapsed: 29 * time.Second}}}
	readable := func() (float64, bool) { return 7.44, true }
	unreadable := func() (float64, bool) { return 0, false }

	if got, want := stageTimesUnderLoad(g, 95*time.Second, false, readable), "stages: gate 29s, om 1m35s (load1 7.4)"; got != want {
		t.Errorf("stageTimesUnderLoad = %q, want %q", got, want)
	}
	if got, want := stageTimesUnderLoad(g, 95*time.Second, false, unreadable), "stages: gate 29s, om 1m35s"; got != want {
		t.Errorf("stageTimesUnderLoad = %q, want the line unchanged when the load is unreadable", got)
	}
}
