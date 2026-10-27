package land

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/forgejo"
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
	merger   *localMerger

	realPushMain   func(name, body string) string
	realOriginMain func() string
	realParents    func(commit string) []string
	realSetMain    func(commit string)
	realRef        func(dir, ref string) string
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

// setMain points origin/main at commit: the write the fixture's merger makes
// where the real one asks Forgejo to fast-forward the target.
func (f *landFixture) setMain(commit string) {
	f.t.Helper()
	if f.git == nil {
		f.realSetMain(commit)
		return
	}
	f.git.SetRef(f.t, f.origin, "refs/heads/main", commit)
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
	if f.merger == nil {
		f.merger = &localMerger{f: f}
	}
	l := &Lander{Repo: f.repo, WorkRoot: f.workRoot, Candidate: &gateCandidate{f: f}, Merger: f.merger,
		Reviewer: f.review, Beads: f.bd, Landings: lf,
		Now: func() time.Time { return time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC) }}
	if f.git != nil {
		l.openRepo = func(dir string) Repo { return f.git.Open(dir) }
	}
	return l
}

// gateCandidate is the fixture's stand-in for the Forgejo gate: it runs the
// fixture's fakeGate on the worktree the candidate was cut in and renders its
// verdict the way a CI run would — passed, failed, or nothing reported.
type gateCandidate struct {
	f *landFixture
}

func (c *gateCandidate) Run(ctx context.Context, _ Repo, dir string, w Work, head string) CandidateResult {
	res := c.f.gate.Run(ctx, dir)
	out := CandidateResult{State: CandidatePassed, Context: "ci / gate (push)", SHA: head, Branch: w.Candidate(), Pushed: true}
	if res.Err != nil {
		out.State, out.Err = CandidateFailed, res.Err
		return out
	}
	if !res.Passed {
		out.State, out.Tail = CandidateFailed, res.FailureTail()
	}
	return out
}

func (c *gateCandidate) Discard(_ context.Context, _ Work, _ CandidateResult) {}

// localMerger is the fixture's stand-in for the Forgejo PR merge: it performs
// the fast-forward the real merger asks Forgejo for — the target's tip becomes
// the candidate — and reports the race a target that moved under the candidate
// produces. A test replaces fn to drive a refusal or a read-back failure.
type localMerger struct {
	f     *landFixture
	fn    func(MergeRequest) error
	calls []MergeRequest
}

func (m *localMerger) Merge(_ context.Context, req MergeRequest) error {
	m.calls = append(m.calls, req)
	if m.fn != nil {
		return m.fn(req)
	}
	// The candidate was merged onto the target's tip, so a target that is no
	// longer that tip moved under the candidate: the 409 the API reports for
	// a stale candidate.
	if got := m.f.originMain(); got != m.f.base {
		return &RaceError{Target: req.Work.Target, Expected: m.f.base, Actual: got}
	}
	m.f.setMain(req.Head)
	return nil
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

// refAt is what ref points at in the repository at dir, "" when it is unset.
func (f *landFixture) refAt(dir, ref string) string {
	f.t.Helper()
	if f.git == nil {
		if f.realRef == nil {
			f.t.Fatal("refAt: the fixture has no gitfake and no realRef")
		}
		return f.realRef(dir, ref)
	}
	return f.git.Ref(dir, ref)
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

// TestRejectKeepsTheHolderOfAHumanOnlyBead: the branch of a human-only bead
// comes from a person, so the crew that asked for it is the only holder that
// can act on the refusal. Clearing the assignee instead is what put one back on
// the dispatcher's board, and from there onto a polecat seat that cannot land
// it (gt-me800). Everything else about the rejection is unchanged. The label is
// typed by hand, and the dispatch hold folds its case, so a Human-Only bead is
// held the same way (gt-r5uwc).
func TestRejectKeepsTheHolderOfAHumanOnlyBead(t *testing.T) {
	t.Parallel()
	for _, label := range []string{dispatch.HumanOnlyLabel, "Human-Only", " HUMAN-ONLY "} {
		t.Run(label, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			const crew = "fractals/crew/ops"
			f.bd.Seed(beads.Issue{ID: "gt-abc", Title: "land the release workflows", Status: "hooked", Type: "task",
				Assignee: crew, Labels: []string{LabelReadyToLand, label}, Notes: FormatReadyNote(f.work)})
			f.gate.fn = func(string) GateResult {
				return GateResult{Passed: false, Steps: []StepResult{{Name: "test", ExitCode: 1}}}
			}

			_, err := f.lander().Land(context.Background(), f.work)
			var rej *Rejection
			if !errors.As(err, &rej) || rej.Kind != RejectGate || rej.RecordErr != nil {
				t.Fatalf("Land = %T %v, want a recorded gate rejection", err, err)
			}
			b := f.bead()
			if b.Status != "open" || b.Assignee != crew || beads.HasLabel(b, LabelReadyToLand) || !beads.HasLabel(b, LabelRework) {
				t.Errorf("bead after rejection: status=%s assignee=%q labels=%v", b.Status, b.Assignee, b.Labels)
			}
			if CountRejections(b.Notes) != 1 {
				t.Errorf("rejection not recorded once:\n%s", b.Notes)
			}
		})
	}
}

// TestRejectKeepsABirdsClaim: a bird claimed the bead it submitted, so a
// rejection leaves the claim and the status alone, still notes the refusal,
// and reaches the owner as one attention item keyed by the bead (gt-h4cvi).
func TestRejectKeepsABirdsClaim(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	const owner = "gastown/crew/sloan"
	f.bd.Seed(beads.Issue{ID: "gt-abc", Title: "add b", Status: "in_progress", Type: "task",
		Assignee: owner, Labels: []string{LabelReadyToLand}, Notes: FormatReadyNote(f.work)})
	f.gate.fn = func(string) GateResult {
		return GateResult{Passed: false, Steps: []StepResult{{Name: "test", ExitCode: 1}}}
	}
	var alerts []string
	l := f.lander()
	l.BirdOwned = func(assignee string) bool { return assignee == owner }
	l.RejectionAlert = func(key, message string) error {
		alerts = append(alerts, key+": "+message)
		return nil
	}

	_, err := l.Land(context.Background(), f.work)
	var rej *Rejection
	if !errors.As(err, &rej) || rej.Kind != RejectGate || rej.RecordErr != nil {
		t.Fatalf("Land = %T %v, want a recorded gate rejection", err, err)
	}
	b := f.bead()
	if b.Status != "in_progress" || b.Assignee != owner {
		t.Errorf("bead after rejection: status=%s assignee=%q, want the bird's claim kept", b.Status, b.Assignee)
	}
	if beads.HasLabel(b, LabelReadyToLand) || !beads.HasLabel(b, LabelRework) {
		t.Errorf("labels after rejection = %v, want the ready label swapped for rework", b.Labels)
	}
	if CountRejections(b.Notes) != 1 {
		t.Errorf("rejection not recorded once:\n%s", b.Notes)
	}
	if len(alerts) != 1 {
		t.Fatalf("raised %d attention item(s), want one: %v", len(alerts), alerts)
	}
	if !strings.HasPrefix(alerts[0], "rejected:gt-abc:") || !strings.Contains(alerts[0], owner) {
		t.Errorf("attention item %q; want it keyed rejected:gt-abc and naming %s", alerts[0], owner)
	}
}

// TestRejectReleasesAPolecatsBead: a polecat's claim ends at gt done, so its
// rejected bead is reopened and unassigned for dispatch exactly as before, and
// nothing is raised for an owner who is not a bird (gt-h4cvi).
func TestRejectReleasesAPolecatsBead(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	const owner = "gastown/polecats/opal"
	f.bd.Seed(beads.Issue{ID: "gt-abc", Title: "add b", Status: "in_progress", Type: "task",
		Assignee: owner, Labels: []string{LabelReadyToLand}, Notes: FormatReadyNote(f.work)})
	f.gate.fn = func(string) GateResult {
		return GateResult{Passed: false, Steps: []StepResult{{Name: "test", ExitCode: 1}}}
	}
	var alerts []string
	l := f.lander()
	l.BirdOwned = func(assignee string) bool { return assignee == "gastown/crew/sloan" }
	l.RejectionAlert = func(key, message string) error {
		alerts = append(alerts, key)
		return nil
	}

	_, err := l.Land(context.Background(), f.work)
	var rej *Rejection
	if !errors.As(err, &rej) || rej.Kind != RejectGate || rej.RecordErr != nil {
		t.Fatalf("Land = %T %v, want a recorded gate rejection", err, err)
	}
	b := f.bead()
	if b.Status != "open" || b.Assignee != "" {
		t.Errorf("bead after rejection: status=%s assignee=%q, want it released to dispatch", b.Status, b.Assignee)
	}
	if CountRejections(b.Notes) != 1 {
		t.Errorf("rejection not recorded once:\n%s", b.Notes)
	}
	if len(alerts) != 0 {
		t.Errorf("raised %v for a polecat-owned bead, want nothing", alerts)
	}
}

// TestBirdRejectionAlertIsRaisedBeforeTheWrite: the item is raised while the
// bead still carries ready-to-land, because the write that takes the bead out
// of the queue is the last thing that ever runs the rejection — an item raised
// after it can be lost for good. A pass whose write failed raises again on the
// next pass, and the two deliveries are one item because both carry the bead's
// key (gt-h4cvi).
func TestBirdRejectionAlertIsRaisedBeforeTheWrite(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	const owner = "town/crew/robin"
	f.bd.Seed(beads.Issue{ID: "gt-abc", Title: "add b", Status: "in_progress", Type: "task",
		Assignee: owner, Labels: []string{LabelReadyToLand}, Notes: FormatReadyNote(f.work)})
	f.gate.fn = func(string) GateResult {
		return GateResult{Passed: false, Steps: []StepResult{{Name: "test", ExitCode: 1}}}
	}
	// deliveries counts every raise, and items stands in for the notify layer,
	// which keys an attention item by its fingerprint: two deliveries of one
	// key are one item. A map alone would hide a duplicate raise, so both are
	// recorded.
	var deliveries []string
	var readyAtDelivery []bool
	items := map[string]string{}
	l := f.lander()
	l.Beads = &failFirstUpdate{Fake: f.bd}
	l.BirdOwned = func(assignee string) bool { return assignee == owner }
	l.RejectionAlert = func(key, message string) error {
		deliveries = append(deliveries, key)
		readyAtDelivery = append(readyAtDelivery, beads.HasLabel(f.bead(), LabelReadyToLand))
		items[key] = message
		return nil
	}

	_, err := l.Land(context.Background(), f.work)
	var rej *Rejection
	if !errors.As(err, &rej) || rej.RecordErr == nil || !strings.Contains(rej.RecordErr.Error(), "reopening the bead") {
		t.Fatalf("first Land = %T %v, want a rejection whose bead write failed", err, err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("deliveries = %v, want the item raised before the failed write", deliveries)
	}
	if !readyAtDelivery[0] {
		t.Error("the item was raised after the bead left ready-to-land; want it raised before the write")
	}
	b := f.bead()
	if !beads.HasLabel(b, LabelReadyToLand) || b.Status != "in_progress" || b.Assignee != owner {
		t.Errorf("bead after the failed write: status=%s assignee=%q labels=%v, want it ready with the bird's claim kept",
			b.Status, b.Assignee, b.Labels)
	}
	if CountRejections(b.Notes) != 1 {
		t.Fatalf("rejection recorded %d time(s) after the failed write, want once:\n%s", CountRejections(b.Notes), b.Notes)
	}

	_, err = l.Land(context.Background(), f.work)
	if !errors.As(err, &rej) || rej.RecordErr != nil {
		t.Fatalf("retry Land = %T %v, want the completed rejection", err, err)
	}
	if len(deliveries) != 2 {
		t.Errorf("deliveries = %v, want the retry to raise again", deliveries)
	}
	if len(items) != 1 || items["rejected:gt-abc"] == "" {
		t.Errorf("items = %v, want one rejected:gt-abc however many times it is delivered", items)
	}
	b = f.bead()
	if b.Status != "in_progress" || b.Assignee != owner {
		t.Errorf("bead after rejection: status=%s assignee=%q, want the bird's claim kept", b.Status, b.Assignee)
	}
	if beads.HasLabel(b, LabelReadyToLand) || !beads.HasLabel(b, LabelRework) {
		t.Errorf("labels after rejection = %v, want the ready label swapped for rework", b.Labels)
	}
	if CountRejections(b.Notes) != 1 {
		t.Errorf("rejections recorded = %d, want the one block:\n%s", CountRejections(b.Notes), b.Notes)
	}
}

// TestBirdRejectionAlertFailureIsSurfaced: the attention item is how the owner
// hears of the refusal, so a sink that refuses it is not just logged. The pass
// reports the failed delivery as RecordErr and leaves the bead ready, keeping
// the claim, so the worker's next pass delivers the item and finishes the
// rejection (gt-h4cvi).
func TestBirdRejectionAlertFailureIsSurfaced(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	const owner = "gastown/crew/sloan"
	f.bd.Seed(beads.Issue{ID: "gt-abc", Title: "add b", Status: "in_progress", Type: "task",
		Assignee: owner, Labels: []string{LabelReadyToLand}, Notes: FormatReadyNote(f.work)})
	f.gate.fn = func(string) GateResult {
		return GateResult{Passed: false, Steps: []StepResult{{Name: "test", ExitCode: 1}}}
	}
	var attempts int
	items := map[string]string{}
	l := f.lander()
	l.BirdOwned = func(assignee string) bool { return assignee == owner }
	l.RejectionAlert = func(key, message string) error {
		attempts++
		if attempts == 1 {
			return errors.New("bd: database is locked")
		}
		items[key] = message
		return nil
	}

	_, err := l.Land(context.Background(), f.work)
	var rej *Rejection
	if !errors.As(err, &rej) || rej.RecordErr == nil || !strings.Contains(rej.RecordErr.Error(), "raising the rejection attention item") {
		t.Fatalf("first Land = %T %v, want the failed delivery surfaced as RecordErr", err, err)
	}
	b := f.bead()
	if b.Status != "in_progress" || b.Assignee != owner || !beads.HasLabel(b, LabelReadyToLand) {
		t.Errorf("bead after the failed delivery: status=%s assignee=%q labels=%v, want the claim kept and the bead still ready to retry",
			b.Status, b.Assignee, b.Labels)
	}
	if CountRejections(b.Notes) != 1 {
		t.Fatalf("rejection recorded %d time(s), want the note to stand for the retry:\n%s", CountRejections(b.Notes), b.Notes)
	}

	_, err = l.Land(context.Background(), f.work)
	if !errors.As(err, &rej) || rej.RecordErr != nil {
		t.Fatalf("retry Land = %T %v, want the completed rejection", err, err)
	}
	if attempts != 2 {
		t.Errorf("delivery attempts = %d, want the retry", attempts)
	}
	if len(items) != 1 || items["rejected:gt-abc"] == "" {
		t.Errorf("items = %v, want the item delivered on the retry", items)
	}
	b = f.bead()
	if b.Status != "in_progress" || b.Assignee != owner || !beads.HasLabel(b, LabelRework) {
		t.Errorf("bead after the retry: status=%s assignee=%q labels=%v, want the bird's claim kept and rework recorded",
			b.Status, b.Assignee, b.Labels)
	}
	if CountRejections(b.Notes) != 1 {
		t.Errorf("rejection recorded %d time(s), want once:\n%s", CountRejections(b.Notes), b.Notes)
	}
}

// failFirstUpdate fails the first bead write so a test sees the pass that
// finishes an interrupted rejection.
type failFirstUpdate struct {
	*beadsfake.Fake
	failed bool
}

func (b *failFirstUpdate) Update(id string, opts beads.UpdateOptions) error {
	if !b.failed {
		b.failed = true
		return errors.New("database is locked")
	}
	return b.Fake.Update(id, opts)
}

// TestBirdRejectionAlertBoundsTheReason: om's summary is free text of any
// length, and the item is a headline the owner reads rather than the rejected
// work's whole review, so the text is flattened and cut while the bead and the
// owner still name themselves (gt-h4cvi).
func TestBirdRejectionAlertBoundsTheReason(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	const owner = "gastown/crew/sloan"
	f.bd.Seed(beads.Issue{ID: "gt-abc", Title: "add b", Status: "in_progress", Type: "task",
		Assignee: owner, Labels: []string{LabelReadyToLand}, Notes: FormatReadyNote(f.work)})
	f.review.fn = func(string) (Verdict, error) {
		return Verdict{Verdict: VerdictRequestChanges, Score: 0.4,
			Summary: strings.Repeat("the summary keeps going ", 500) + "\na second line"}, nil
	}
	var alerts []string
	l := f.lander()
	l.BirdOwned = func(assignee string) bool { return assignee == owner }
	l.RejectionAlert = func(key, message string) error {
		alerts = append(alerts, message)
		return nil
	}

	_, err := l.Land(context.Background(), f.work)
	var rej *Rejection
	if !errors.As(err, &rej) || rej.Kind != RejectReview {
		t.Fatalf("Land = %T %v, want a review rejection", err, err)
	}
	if len(alerts) != 1 {
		t.Fatalf("raised %d item(s), want one: %v", len(alerts), alerts)
	}
	msg := alerts[0]
	if strings.Contains(msg, "\n") {
		t.Errorf("item carries a newline, want one line: %q", msg)
	}
	if len(msg) > 600 || !strings.Contains(msg, " …") {
		t.Errorf("item is %d bytes and %q; want the summary cut short", len(msg), msg)
	}
	if !strings.Contains(msg, "gt-abc") || !strings.Contains(msg, owner) {
		t.Errorf("item = %q, want it naming the bead and its owner", msg)
	}
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
	if want := []string{StageCI, StageOM}; !slices.Equal(stages, want) {
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
	// gt-as9v8: the head the record names stays reachable at
	// refs/landed/<bead>, in the rig's repository and on origin, so the
	// landing worker may delete the branch without losing the history a
	// worktree still holding it answers its own landing with.
	ref := LandedRef("gt-abc")
	for _, dir := range []string{f.repo, f.origin} {
		if got := f.refAt(dir, ref); got != f.work.Head {
			t.Errorf("%s in %s = %q, want the landed head %q", ref, dir, got, f.work.Head)
		}
	}
}

// A rework of a bead that landed before (after a revert) lands a new head,
// and refs/landed/<bead> moves to it: the ref answers "is this worktree's
// work landed?" for the newest landing of the bead, not the first (gt-as9v8).
func TestLandMovesTheLandedRefOnAReLanding(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	first := f.work.Head
	if _, err := f.lander().Land(context.Background(), f.work); err != nil {
		t.Fatalf("first Land: %v", err)
	}
	ref := LandedRef("gt-abc")

	// The revert put main back where the first branch forked, and the rework
	// adds its own file on top of what landed.
	f.base = f.originMain()
	f.git.SetRef(t, f.origin, "refs/heads/"+fixtureBranch, f.base)
	second := f.git.Commit(t, f.origin, fixtureBranch, "feat: add c", map[string]string{"c.txt": "more\n"})
	f.work.Head = second
	f.ready(second)
	if _, err := f.lander().Land(context.Background(), f.work); err != nil {
		t.Fatalf("second Land: %v", err)
	}
	for _, dir := range []string{f.repo, f.origin} {
		if got := f.refAt(dir, ref); got != second {
			t.Errorf("%s in %s = %q, want the re-landed head %q (first head %q)", ref, dir, got, second, first)
		}
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

// TestConflictRejectionLogsItsTargetTip: a conflicted merge writes no merge
// line, so the rejection line has to name the tip the conflict was built on for
// the conflict scorecard to attribute the pair (gt-0nvhe).
func TestConflictRejectionLogsItsTargetTip(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.base = f.pushMain("b.txt", "main's b\n")
	var landLog strings.Builder
	l := f.lander()
	l.Out = &landLog
	_, err := l.Land(context.Background(), f.work)
	rej := f.assertRejected(t, err, RejectConflict, LabelRework)
	if rej.TargetTip != f.base {
		t.Errorf("TargetTip = %q, want the target tip %q", rej.TargetTip, f.base)
	}
	if want := "(target tip " + f.base + ")"; !strings.Contains(landLog.String(), want) {
		t.Errorf("landing log lacks %q:\n%s", want, landLog.String())
	}
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

// TestLandRejectionAfterTheCeilingRecordsTheCeiling is gt-ern0y: a bead can be
// resubmitted after it has been escalated, and its notes keep accumulating, so
// the raw rejection count would record the next refusal as "attempt 4 of 3".
// The recorded attempt, and the escalation text, stop at MaxReworkAttempts.
func TestLandRejectionAfterTheCeilingRecordsTheCeiling(t *testing.T) {
	t.Parallel()
	for _, prior := range []int{0, 1, MaxReworkAttempts - 1, MaxReworkAttempts, MaxReworkAttempts + 2} {
		prior := prior
		t.Run(fmt.Sprintf("%d_prior_rejections", prior), func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			for i := 1; i <= prior; i++ {
				// Another head, so this block is never read as an unfinished
				// repeat of the rejection the landing below records.
				block := FormatRejectionNote(RejectionNote{
					Attempt: i, Kind: string(RejectGate), Reason: "earlier refusal",
					Branch: f.work.Branch, Target: f.work.Target, MR: f.work.BeadID,
					Head: fmt.Sprintf("%040d", i),
				})
				if err := f.bd.Update(f.work.BeadID, beads.UpdateOptions{AppendNotes: block}); err != nil {
					t.Fatalf("%d: seeding the notes: %v", i, err)
				}
			}
			f.gate.fn = func(string) GateResult {
				return GateResult{Steps: []StepResult{{Name: "test", ExitCode: 2}}}
			}
			_, err := f.lander().Land(context.Background(), f.work)
			var rej *Rejection
			if !errors.As(err, &rej) || rej.Kind != RejectGate || rej.RecordErr != nil {
				t.Fatalf("Land = %T %v, want a recorded gate rejection", err, err)
			}
			want := min(prior+1, MaxReworkAttempts)
			notes := f.bead().Notes
			if got := CountRejections(notes); got != prior+1 {
				t.Fatalf("%d rejection block(s) recorded, want %d", got, prior+1)
			}
			last, ok := ParseRejectionNote(notes)
			if !ok || last.Attempt != want {
				t.Fatalf("last block attempt = %d (ok=%v), want the ceiling %d", last.Attempt, ok, want)
			}
			if prior+1 > MaxReworkAttempts && strings.Contains(notes, fmt.Sprintf("attempt %d of", prior+1)) {
				t.Errorf("the escalation text names attempt %d, above the ceiling %d:\n%s", prior+1, MaxReworkAttempts, notes)
			}
			capped := prior+1 >= MaxReworkAttempts
			if !capped {
				if !rej.Rework || strings.Contains(notes, "the loop is escalated") {
					t.Errorf("rejection %d of %d was escalated early: rework=%v\n%s", prior+1, MaxReworkAttempts, rej.Rework, notes)
				}
				return
			}
			escalation := fmt.Sprintf("attempt %d of %d: the loop is escalated", want, MaxReworkAttempts)
			if rej.Rework || !strings.Contains(notes, escalation) {
				t.Errorf("capped rejection records rework=%v, want false and %q:\n%s", rej.Rework, escalation, notes)
			}
		})
	}
}

// requeue applies gt land requeue to the fixture's bead the way the command
// does: it restores the ready label, drops the rework label, and records the
// REQUEUED comment naming the rejection attempt it undid.
func (f *landFixture) requeue(attempt int) {
	f.t.Helper()
	if err := f.bd.Update(f.work.BeadID, beads.UpdateOptions{
		AddLabels: []string{LabelReadyToLand}, RemoveLabels: []string{LabelRework},
	}); err != nil {
		f.t.Fatalf("requeue: %v", err)
	}
	if err := f.bd.AddCommentAs(f.work.BeadID, "operator",
		FormatRequeueComment("operator", f.work.Branch, f.work.Head, attempt, "runner fault, not the diff")); err != nil {
		f.t.Fatalf("requeue comment: %v", err)
	}
}

// TestLandRejectionAfterRequeueRecordsTheNextAttempt: a landing rejected,
// re-queued unchanged and rejected again for the same kind is the next attempt,
// not the completion of an unfinished pass. A requeue leaves the bead in the
// same shape an interrupted pass does — ready, refusal label gone — so the
// requeue comment naming the attempt it undid is what tells them apart; without
// it the second block collapses into the first and the new gate tail and
// attempt number are lost (gt-en9gs).
func TestLandRejectionAfterRequeueRecordsTheNextAttempt(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.gate.fn = func(string) GateResult {
		return GateResult{Steps: []StepResult{{Name: "test", ExitCode: 1, Tail: "--- FAIL: TestFirst\n"}}}
	}
	l := f.lander()
	if _, err := l.Land(context.Background(), f.work); err == nil {
		t.Fatal("the first landing passed; want a gate rejection")
	}
	if got, ok := ParseRejectionNote(f.bead().Notes); !ok || got.Attempt != 1 {
		t.Fatalf("first rejection attempt = %d, %v; want 1", got.Attempt, ok)
	}
	f.requeue(1)

	f.gate.fn = func(string) GateResult {
		return GateResult{Steps: []StepResult{{Name: "test", ExitCode: 1, Tail: "--- FAIL: TestSecond\n"}}}
	}
	_, err := l.Land(context.Background(), f.work)
	var rej *Rejection
	if !errors.As(err, &rej) || rej.Kind != RejectGate || rej.RecordErr != nil {
		t.Fatalf("second Land = %T %v, want a recorded gate rejection", err, err)
	}
	notes := f.bead().Notes
	if got := CountRejections(notes); got != 2 {
		t.Fatalf("%d rejection block(s) after the requeue, want 2:\n%s", got, notes)
	}
	last, ok := ParseRejectionNote(notes)
	if !ok || last.Attempt != 2 || last.Head != f.work.Head {
		t.Errorf("last block = attempt %d head %s, want attempt 2 of %s", last.Attempt, last.Head, f.work.Head)
	}
	if !strings.Contains(notes, "TestSecond") || !strings.Contains(notes, "TestFirst") {
		t.Errorf("the requeued rejection did not carry the new gate tail:\n%s", notes)
	}
}

// TestLandRequeueLoopStillHitsTheAttemptCap: a requeue loop is rejections like
// any other, so MaxReworkAttempts ends it. Before the requeue comment named the
// attempt it undid, every round after the first collapsed into the first block,
// the count never advanced and the cap could not trip (gt-en9gs).
func TestLandRequeueLoopStillHitsTheAttemptCap(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.gate.fn = func(string) GateResult { return GateResult{Steps: []StepResult{{Name: "test", ExitCode: 2}}} }
	l := f.lander()
	for attempt := 1; attempt <= MaxReworkAttempts; attempt++ {
		_, err := l.Land(context.Background(), f.work)
		var rej *Rejection
		if !errors.As(err, &rej) || rej.Kind != RejectGate {
			t.Fatalf("attempt %d: Land = %T %v, want a gate rejection", attempt, err, err)
		}
		capped := attempt >= MaxReworkAttempts
		if rej.Rework == capped {
			t.Errorf("attempt %d: rej.Rework = %v, want %v", attempt, rej.Rework, !capped)
		}
		if got := CountRejections(f.bead().Notes); got != attempt {
			t.Fatalf("attempt %d: %d rejection block(s), want %d", attempt, got, attempt)
		}
		if capped {
			if b := f.bead(); !beads.HasLabel(b, LabelNeedsHuman) || beads.HasLabel(b, LabelReadyToLand) {
				t.Errorf("the cap did not escalate: labels %v", b.Labels)
			}
			return
		}
		f.requeue(attempt)
	}
	t.Errorf("the requeue loop never hit MaxReworkAttempts = %d", MaxReworkAttempts)
}

// TestLandInterruptedPassAfterRequeueIsFinishedNotRepeated: a pass that writes
// its block and then fails to reopen the bead is finished on the retry. The
// requeue marker must not confuse that with a requeued block: the newest
// REQUEUED comment names the earlier attempt, so the block it left unfinished
// is still completed in place rather than appended again (gt-zqqcr, gt-en9gs).
func TestLandInterruptedPassAfterRequeueIsFinishedNotRepeated(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.gate.fn = func(string) GateResult {
		return GateResult{Steps: []StepResult{{Name: "test", ExitCode: 1, Tail: "--- FAIL: TestFirst\n"}}}
	}
	l := f.lander()
	if _, err := l.Land(context.Background(), f.work); err == nil {
		t.Fatal("the first landing passed; want a gate rejection")
	}
	f.requeue(1)

	f.gate.fn = func(string) GateResult {
		return GateResult{Steps: []StepResult{{Name: "test", ExitCode: 1, Tail: "--- FAIL: TestSecond\n"}}}
	}
	l.Beads = &failingBeads{Fake: f.bd, failUpdate: true}
	for round := 1; round <= 2; round++ {
		_, err := l.Land(context.Background(), f.work)
		var rej *Rejection
		if !errors.As(err, &rej) || rej.RecordErr == nil || !strings.Contains(rej.RecordErr.Error(), "reopening the bead") {
			t.Fatalf("round %d: Land = %T %v, want a rejection whose reopen failed", round, err, err)
		}
		notes := f.bead().Notes
		if got := CountRejections(notes); got != 2 {
			t.Fatalf("round %d: %d rejection block(s), want 2 — the interrupted block completed, not repeated:\n%s", round, got, notes)
		}
		last, ok := ParseRejectionNote(notes)
		if !ok || last.Attempt != 2 || !strings.Contains(notes, "TestSecond") {
			t.Fatalf("round %d: unfinished block = attempt %d, %v; want attempt 2 carrying the new tail", round, last.Attempt, ok)
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
	// The merge reports success but the target's tip is not the merged commit:
	// a merge that did not take, which the read-back is there to catch.
	f.merger.fn = func(MergeRequest) error { return nil }
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

func TestGateInfraErrorIsNotARejection(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.gate.fn = func(string) GateResult { return GateResult{Err: errors.New("container slot unavailable")} }
	_, err := f.lander().Land(context.Background(), f.work)
	var infra *InfraError
	if !errors.As(err, &infra) || infra.Stage != StageCI {
		t.Fatalf("Land error = %T %v, want *InfraError at the CI stage", err, err)
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

// TestLandCompletesALandingWhoseRecordWasNeverWritten: the merge took on the
// target but the pass never recorded it (the daemon died between the merge and
// the record). The next pass finishes that landing — record written, bead
// closed — instead of reading the head on the target as an operator push and
// sending the bead to a human.
func TestLandCompletesALandingWhoseRecordWasNeverWritten(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	var merged string
	f.merger.fn = func(req MergeRequest) error {
		merged = req.Head
		f.setMain(req.Head)
		return errors.New("the pass ended after the merge, before the record")
	}
	if _, err := l.Land(context.Background(), f.work); err == nil {
		t.Fatal("the first pass returned a landing it never recorded")
	}
	if f.bead().Status == "closed" || len(f.landingLines()) != 0 {
		t.Fatalf("the first pass left a record: status=%s lines=%v", f.bead().Status, f.landingLines())
	}
	if got := f.originMain(); got != merged {
		t.Fatalf("origin/main = %s, want the merge %s", got, merged)
	}

	f.merger.fn = nil
	res, err := f.lander().Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("the second pass did not finish the landing: %v", err)
	}
	if res.LandedCommit != merged {
		t.Errorf("LandedCommit = %s, want the merge %s", res.LandedCommit, merged)
	}
	if len(f.merger.calls) != 1 {
		t.Errorf("the merger ran %d times, want the one merge", len(f.merger.calls))
	}
	if len(f.gate.dirs) != 1 || len(f.review.calls) != 1 {
		t.Errorf("the second pass gated and reviewed again: %d gate runs, %d reviews", len(f.gate.dirs), len(f.review.calls))
	}
	b := f.bead()
	if b.Status != "closed" || !strings.Contains(b.Notes, LandingNoteMarker+"\nlanded_commit: "+merged) {
		t.Errorf("bead after the finished landing: status=%s notes=%q", b.Status, b.Notes)
	}
	if CountRejections(b.Notes) != 0 {
		t.Errorf("the finished landing carries a rejection:\n%s", b.Notes)
	}
	lines := f.landingLines()
	if len(lines) != 1 {
		t.Fatalf("landings lines = %d, want 1", len(lines))
	}
	// The record says what it does not know rather than reading a gate that
	// never ran here as a failure.
	if !strings.Contains(lines[0], `"om_verdict":"`+VerdictUnrecorded+`"`) || !strings.Contains(lines[0], "not recorded") {
		t.Errorf("landing record = %s", lines[0])
	}
}

// squashRange adds an auto-save commit to the work branch, which is what makes
// mergeWork squash: the range it lands then holds a checkpoint commit.
func (f *landFixture) squashRange(t *testing.T) {
	t.Helper()
	f.work.Head = f.git.Commit(t, f.origin, fixtureBranch, checkpoint.WIPCommitPrefix+" 2026-09-30", map[string]string{"c.txt": "wip\n"})
}

// TestLandRecoversUnrecordedSquashLanding: a range with auto-save commits is
// squash-merged, so the commit the target gets has one parent and never
// contains the head. Its record was never written, and the next pass — whose
// ancestry check cannot see a squash at all — read the identical trees as an
// empty merge and sent the author back to rework a landing that had already
// taken (gt-064oh).
func TestLandRecoversUnrecordedSquashLanding(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.squashRange(t)
	merged := recoversUnrecordedSquashLanding(t, f)
	// The fake can answer the premise directly: the target holds a squash of
	// the head, not the head.
	if contains, err := f.git.Open(f.repo).IsAncestor(f.work.Head, merged); err != nil || contains {
		t.Fatalf("the head is reachable from the landed %s (%v); the test proves nothing about a squash", shortSHA(merged), err)
	}
}

// recoversUnrecordedSquashLanding takes a fixture whose range has an
// auto-save commit through a landing that squashes and then loses its record,
// lands the same head again and returns the squashed commit the second pass
// must finish. The unit tier runs it over gitfake; the integration tier over
// real git, whose patch ids are what the recovery matches a squash by.
func recoversUnrecordedSquashLanding(t *testing.T, f *landFixture) string {
	t.Helper()
	l := f.lander()
	var merged string
	f.merger.fn = func(req MergeRequest) error {
		merged = req.Head
		f.setMain(req.Head)
		return errors.New("the pass ended after the merge, before the record")
	}
	if _, err := l.Land(context.Background(), f.work); err == nil {
		t.Fatal("the first pass returned a landing it never recorded")
	}
	if parents := f.parents(merged); len(parents) != 1 {
		t.Fatalf("landed commit parents = %v, want the squash's single parent", parents)
	}
	if f.bead().Status == "closed" || len(f.landingLines()) != 0 {
		t.Fatalf("the first pass left a record: status=%s lines=%v", f.bead().Status, f.landingLines())
	}

	f.merger.fn = nil
	res, err := f.lander().Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("the second pass did not finish the squash landing: %v", err)
	}
	if res.LandedCommit != merged {
		t.Errorf("LandedCommit = %s, want the squash %s", res.LandedCommit, merged)
	}
	if len(f.merger.calls) != 1 {
		t.Errorf("the merger ran %d times, want the one merge", len(f.merger.calls))
	}
	if len(f.gate.dirs) != 1 || len(f.review.calls) != 1 {
		t.Errorf("the second pass gated and reviewed again: %d gate runs, %d reviews", len(f.gate.dirs), len(f.review.calls))
	}
	b := f.bead()
	if b.Status != "closed" || !strings.Contains(b.Notes, LandingNoteMarker+"\nlanded_commit: "+merged) {
		t.Errorf("bead after the finished landing: status=%s notes=%q", b.Status, b.Notes)
	}
	if CountRejections(b.Notes) != 0 {
		t.Errorf("the finished landing carries a rejection:\n%s", b.Notes)
	}
	lines := f.landingLines()
	if len(lines) != 1 {
		t.Fatalf("landings lines = %d, want 1", len(lines))
	}
	if !strings.Contains(lines[0], `"om_verdict":"`+VerdictUnrecorded+`"`) {
		t.Errorf("landing record = %s", lines[0])
	}
	return merged
}

// TestLandRecoversUnrecordedSquashLandingUnderALaterLanding: another landing
// reached the target between the unrecorded squash and the retry, so the
// retry's base is no longer tree-identical to the head and nothing rejects
// before the merge: MergeSquash stages nothing, git commit fails, and every
// pass reports an InfraError over a landing that already took (gt-064oh).
func TestLandRecoversUnrecordedSquashLandingUnderALaterLanding(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.squashRange(t)
	l := f.lander()
	var merged string
	f.merger.fn = func(req MergeRequest) error {
		merged = req.Head
		f.setMain(req.Head)
		return errors.New("the pass ended after the merge, before the record")
	}
	if _, err := l.Land(context.Background(), f.work); err == nil {
		t.Fatal("the first pass returned a landing it never recorded")
	}
	f.merger.fn = nil
	f.base = f.pushMain("d.txt", "someone else landed\n")
	if f.git.Tree(f.work.Head)["d.txt"] != "" || f.git.Tree(f.originMain())["d.txt"] == "" {
		t.Fatal("the retry's base is still tree-identical to the head; the test proves nothing")
	}

	res, err := f.lander().Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("the second pass did not finish the squash landing: %v", err)
	}
	if res.LandedCommit != merged || f.originMain() != f.base {
		t.Errorf("LandedCommit = %s, origin/main = %s; want the squash %s under the later landing %s",
			res.LandedCommit, f.originMain(), merged, f.base)
	}
	if len(f.merger.calls) != 1 {
		t.Errorf("the merger ran %d times, want the one merge", len(f.merger.calls))
	}
	if b := f.bead(); b.Status != "closed" || CountRejections(b.Notes) != 0 {
		t.Errorf("bead after the finished landing: status=%s notes=%q", b.Status, b.Notes)
	}
	if len(f.landingLines()) != 1 {
		t.Fatalf("landings lines = %d, want 1", len(f.landingLines()))
	}
}

// TestLandRecoversUnrecordedSquashLandingWithNearbyMainEdit: the target edited a
// line within three of the head's hunk between the head's fork point and the
// squash. A patch-id hashes context lines, so the squash's diff (against the
// edited base) and the head's own diff (against the fork point) hash
// differently though they change the same lines, and the recovery read the
// landing as another head's and rejected it with the work on the target
// (gt-r5uwc).
//
// gitfake merges by path, so a retry's squash conflicts with the target's own
// edit of the file, which is the shape gt-01yt6 recovers from: the squash is
// written onto the edited main by hand rather than by a first pass.
func TestLandRecoversUnrecordedSquashLandingWithNearbyMainEdit(t *testing.T) {
	t.Parallel()
	lines := func(edits map[int]string) string {
		var b strings.Builder
		for i := 1; i <= 30; i++ {
			if s, ok := edits[i]; ok {
				b.WriteString(s)
			} else {
				fmt.Fprintf(&b, "line %d", i)
			}
			b.WriteString("\n")
		}
		return b.String()
	}
	f := newLandFixtureAt(t)
	f.git = gitfake.New()
	f.git.InitBare(t, f.origin)
	seed := f.git.Commit(t, f.origin, "main", "main: seed", map[string]string{"n.txt": lines(nil)})
	f.git.SetRef(t, f.origin, "refs/heads/"+fixtureBranch, seed)
	f.git.Commit(t, f.origin, fixtureBranch, "feat: edit line 20", map[string]string{"n.txt": lines(map[int]string{20: "line 20 by the head"})})
	head := f.git.Commit(t, f.origin, fixtureBranch, checkpoint.WIPCommitPrefix+" 2026-09-30", map[string]string{"c.txt": "wip\n"})
	f.base = f.git.Commit(t, f.origin, "main", "main: edit line 18", map[string]string{"n.txt": lines(map[int]string{18: "line 18 by main"})})
	squash := f.git.Commit(t, f.origin, "main", landMergeMarker(Work{Branch: fixtureBranch, Head: head, Target: "main"})+"cafe1234)",
		map[string]string{"n.txt": lines(map[int]string{18: "line 18 by main", 20: "line 20 by the head"}), "c.txt": "wip\n"})
	f.git.Clone(t, f.origin, f.repo)
	f.ready(head)

	res, err := f.lander().Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("the retry did not finish the squash landing: %v", err)
	}
	if res.LandedCommit != squash || len(f.merger.calls) != 0 {
		t.Errorf("LandedCommit = %s after %d merges, want the squash %s found on the target", res.LandedCommit, len(f.merger.calls), squash)
	}
	if b := f.bead(); b.Status != "closed" || CountRejections(b.Notes) != 0 {
		t.Errorf("bead after the finished landing: status=%s notes=%q", b.Status, b.Notes)
	}
	if len(f.landingLines()) != 1 {
		t.Errorf("landings lines = %d, want 1", len(f.landingLines()))
	}
}

// lookupFailsRepo is a Repo whose search of the target for a land merge fails,
// the transient git error completeUnrecordedLanding reports as an InfraError.
type lookupFailsRepo struct {
	Repo
}

func (lookupFailsRepo) FindCommitMatchingSubject(ref, prefix string) (string, error) {
	return "", errors.New("git log: transient failure")
}

// TestLandUnrecordedSquashLookupFailureIsInfraNotRejection: a squash landing
// whose record was lost is retried, and the search of the target for the
// landing fails on a transient git error. The pass cannot tell a landing it
// already made from a head that arrived by another route, so it ends as an
// InfraError the worker retries, never as a rejection that sends the author
// to rework work that landed (gt-xqpmc). The recovery is consulted at three
// dead ends, one per shape the retry's base can take after the lost record,
// and each must end the same way (gt-z0qzo).
func TestLandUnrecordedSquashLookupFailureIsInfraNotRejection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// later is what another landing does to the target between the lost
		// record and the retry; nil leaves the target at the squash.
		later func(f *landFixture)
	}{
		{name: "identical trees before the merge"},
		{name: "squash under a later landing stages nothing", later: func(f *landFixture) {
			f.base = f.pushMain("d.txt", "someone else landed\n")
		}},
		{name: "squash conflicts with a later landing", later: func(f *landFixture) {
			f.base = f.pushMain("b.txt", "a later landing's b\n")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			f.squashRange(t)
			first := f.lander()
			f.merger.fn = func(req MergeRequest) error {
				f.setMain(req.Head)
				return errors.New("the pass ended after the merge, before the record")
			}
			if _, err := first.Land(context.Background(), f.work); err == nil {
				t.Fatal("the first pass returned a landing it never recorded")
			}
			f.merger.fn = nil
			if tc.later != nil {
				tc.later(f)
			}
			before := f.bead()
			labelsBefore := slices.Clone(before.Labels)
			notesBefore := before.Notes

			l := f.lander()
			l.openRepo = func(dir string) Repo { return lookupFailsRepo{Repo: f.git.Open(dir)} }
			_, err := l.Land(context.Background(), f.work)
			var rej *Rejection
			if errors.As(err, &rej) {
				t.Fatalf("Land rejected the retry as %s (rework=%v): %s", rej.Kind, rej.Rework, rej.Reason)
			}
			var infra *InfraError
			if !errors.As(err, &infra) || infra.Stage != "find the landing on the target" {
				t.Fatalf("Land error = %T %v, want an *InfraError at the find-the-landing stage", err, err)
			}
			b := f.bead()
			if beads.HasLabel(b, LabelRework) || !slices.Equal(b.Labels, labelsBefore) || b.Notes != notesBefore {
				t.Errorf("the failed lookup changed the bead: labels %v -> %v, notes:\n%s", labelsBefore, b.Labels, b.Notes)
			}
			if b.Status == "closed" || len(f.landingLines()) != 0 {
				t.Errorf("the failed lookup recorded a landing: status=%s lines=%v", b.Status, f.landingLines())
			}
		})
	}
}

// TestAsInfraError: an error that already is an *InfraError, at any depth of
// wrapping, passes through untouched; any other is wrapped at the stage; nil
// stays nil.
func TestAsInfraError(t *testing.T) {
	t.Parallel()
	infra := &InfraError{Stage: "inner", Err: errors.New("boom")}
	for _, tc := range []struct {
		name      string
		err       error
		wantSame  bool
		wantStage string
	}{
		{name: "infra", err: infra, wantSame: true, wantStage: "inner"},
		{name: "wrapped infra", err: fmt.Errorf("context: %w", infra), wantSame: true, wantStage: "inner"},
		{name: "plain", err: errors.New("plain"), wantStage: "outer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := asInfraError("outer", tc.err)
			if tc.wantSame && got != tc.err {
				t.Errorf("asInfraError changed %v to %v", tc.err, got)
			}
			var ie *InfraError
			if !errors.As(got, &ie) || ie.Stage != tc.wantStage {
				t.Fatalf("asInfraError(%v) = %T %v, want an *InfraError at %q", tc.err, got, got, tc.wantStage)
			}
			if !errors.Is(got, tc.err) {
				t.Errorf("the result lost its cause: %v", got)
			}
		})
	}
	if got := asInfraError("outer", nil); got != nil {
		t.Errorf("asInfraError(nil) = %v, want nil", got)
	}
}

// TestLandRecoversUnrecordedSquashLandingOverAConflictingLaterLanding: another
// landing edited the lines the head's own change wrote, between the unrecorded
// squash and the retry. The retry's squash of the head conflicts with the
// target's later version of those lines, and the pass rejected it as a
// conflict, sending the author to rework work that already landed. The target
// holds a land merge naming this head, so the conflict is the landing meeting
// its own work and the pass finishes it (gt-01yt6).
func TestLandRecoversUnrecordedSquashLandingOverAConflictingLaterLanding(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.squashRange(t)
	l := f.lander()
	var merged string
	f.merger.fn = func(req MergeRequest) error {
		merged = req.Head
		f.setMain(req.Head)
		return errors.New("the pass ended after the merge, before the record")
	}
	if _, err := l.Land(context.Background(), f.work); err == nil {
		t.Fatal("the first pass returned a landing it never recorded")
	}
	f.merger.fn = nil
	// The later landing rewrites b.txt, which the head's change added.
	f.base = f.pushMain("b.txt", "a later landing's b\n")
	if f.git.Tree(f.originMain())["b.txt"] == f.git.Tree(f.work.Head)["b.txt"] {
		t.Fatal("the target still carries the head's b.txt; the retry's merge cannot conflict")
	}

	res, err := f.lander().Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("the second pass did not finish the squash landing: %v", err)
	}
	if res.LandedCommit != merged || f.originMain() != f.base {
		t.Errorf("LandedCommit = %s, origin/main = %s; want the squash %s under the later landing %s",
			res.LandedCommit, f.originMain(), merged, f.base)
	}
	if len(f.merger.calls) != 1 {
		t.Errorf("the merger ran %d times, want the one merge", len(f.merger.calls))
	}
	if len(f.gate.dirs) != 1 || len(f.review.calls) != 1 {
		t.Errorf("the second pass gated and reviewed again: %d gate runs, %d reviews", len(f.gate.dirs), len(f.review.calls))
	}
	if b := f.bead(); b.Status != "closed" || CountRejections(b.Notes) != 0 {
		t.Errorf("bead after the finished landing: status=%s notes=%q", b.Status, b.Notes)
	}
	if len(f.landingLines()) != 1 {
		t.Fatalf("landings lines = %d, want 1", len(f.landingLines()))
	}
}

// TestLandDeclinesRecoveryOfAConflictWithNoLandMergeForTheHead: a squash of
// the head conflicts with a landing that edited its lines, and the target holds
// no land merge naming this head, so the conflict is the author's to resolve.
// The recovery declines and the pass rejects with rework instead of recording a
// landing (gt-01yt6).
func TestLandDeclinesRecoveryOfAConflictWithNoLandMergeForTheHead(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.squashRange(t)
	f.base = f.pushMain("b.txt", "main's b\n")

	_, err := f.lander().Land(context.Background(), f.work)
	rej := f.assertRejected(t, err, RejectConflict, LabelRework)
	if len(rej.Conflicting) != 1 || rej.Conflicting[0] != "b.txt" {
		t.Errorf("conflicting = %v, want b.txt", rej.Conflicting)
	}
	if len(f.merger.calls) != 0 || len(f.gate.dirs) != 0 {
		t.Errorf("a declined recovery merged or gated: %d merges, %d gate runs", len(f.merger.calls), len(f.gate.dirs))
	}
}

// TestLandRecoversUnrecordedSquashLandingForkedBeforeItsBase: the branch
// forked before the base the squash merged onto — another landing reached the
// target in between — so the head's own change is not the diff between the two
// tips. The recovery still has to match the landing's patch to that change:
// read off the landing's base instead, the target's own work between the fork
// and the landing is folded in reversed, no patch matches, and every retry
// over a head that already landed fails (gt-064oh).
func TestLandRecoversUnrecordedSquashLandingForkedBeforeItsBase(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.squashRange(t)
	f.base = f.pushMain("d.txt", "someone else landed\n")
	if f.git.Tree(f.work.Head)["d.txt"] != "" {
		t.Fatal("the head carries the later landing; the branch did not fork before the base it merges onto")
	}
	merged := recoversUnrecordedSquashLanding(t, f)
	if got := f.parents(merged); len(got) != 1 || got[0] != f.base {
		t.Fatalf("the squash's parents = %v, want the later landing %s it merged onto", got, f.base)
	}
}

// TestLandRefusesAHeadWhoseContentIsOnAMovedTarget: the head's changes reached
// the target by commits of someone else's, no land merge names the head, and
// the target has moved on since, so the two trees differ and the head is not
// an ancestor. Nothing before the merge sees the head as empty; the squash
// stages nothing and its commit fails. That is the head's work already being
// there, a RejectEmpty, and not an InfraError at the merge stage that the
// worker would retry and escalate as a commit problem (gt-sq2oe).
func TestLandRefusesAHeadWhoseContentIsOnAMovedTarget(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.squashRange(t)
	f.pushMain("b.txt", "work\n")
	f.pushMain("c.txt", "wip\n")
	f.base = f.pushMain("d.txt", "someone else landed\n")
	if same, _ := f.git.Open(f.origin).TreesIdentical(f.base, f.work.Head); same {
		t.Fatal("the target's tree is the head's; the identical-trees check would reject, so the test proves nothing")
	}

	_, err := f.lander().Land(context.Background(), f.work)
	var infra *InfraError
	if errors.As(err, &infra) {
		t.Fatalf("Land ended as an InfraError at the %s stage: %v", infra.Stage, err)
	}
	rej := f.assertRejected(t, err, RejectEmpty, LabelRework)
	if !strings.Contains(rej.Reason, "empty merge") || !strings.Contains(rej.Reason, "already there") {
		t.Errorf("reason = %q, want it to name that the head's changes are already on the target", rej.Reason)
	}
}

// TestLandDoesNotFinishALandingWhosePatchIsNotTheHeads: the target carries a
// commit whose subject begins with the land merge's marker for this branch,
// head and target, but which did not merge this head. The marker alone is not
// evidence enough to write a record: the recovery reads the commit's patch
// against the head's own change and declines when they differ, rather than put
// a landing the target never saw on the bead (gt-064oh).
func TestLandDoesNotFinishALandingWhosePatchIsNotTheHeads(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.squashRange(t)
	l := f.lander()
	var merged string
	f.merger.fn = func(req MergeRequest) error {
		merged = req.Head
		f.setMain(req.Head)
		return errors.New("the pass ended after the merge, before the record")
	}
	if _, err := l.Land(context.Background(), f.work); err == nil {
		t.Fatal("the first pass returned a landing it never recorded")
	}
	f.merger.fn = nil
	// A later commit carrying the marker's subject — a second landing of the
	// same branch and target, or an operator's commit — whose patch is not the
	// head's change.
	planted := f.git.Commit(t, f.origin, "main", landMergeMarker(f.work)+shortSHA(f.base)+")",
		map[string]string{"z.txt": "not this head's work\n"})
	if planted == merged {
		t.Fatal("the planted commit is the landing itself; the test proves nothing")
	}

	_, err := f.lander().Land(context.Background(), f.work)
	// The squash of the head stages nothing — the target carries its work —
	// and the marker's commit is not its landing, so the pass ends as an
	// empty merge, never as a record of the planted commit (gt-sq2oe).
	var rej *Rejection
	if !errors.As(err, &rej) || rej.Kind != RejectEmpty {
		t.Fatalf("the second pass = %v, want a RejectEmpty: the marker's commit is not this head's landing", err)
	}
	if b := f.bead(); b.Status == "closed" || strings.Contains(b.Notes, planted) {
		t.Errorf("the pass recorded the planted commit %s as the landing: status=%s notes=%q", shortSHA(planted), b.Status, b.Notes)
	}
	if lines := f.landingLines(); len(lines) != 0 {
		t.Errorf("landings lines = %v, want none", lines)
	}
}

// subjectLookups counts the land merge lookups the landing's search of the
// target makes, so a test can pin that a pass with nothing to recover never
// walks the target's history (gt-064oh).
type subjectLookups struct {
	n int
}

// countedRepo is a Repo whose unrecorded-landing lookups are counted.
type countedRepo struct {
	Repo
	s *subjectLookups
}

func (r countedRepo) FindCommitMatchingSubject(ref, prefix string) (string, error) {
	r.s.n++
	return r.Repo.FindCommitMatchingSubject(ref, prefix)
}

// TestLandDoesNotSearchTheTargetForAFreshLanding: a landing with nothing to
// recover reads the target's history zero times. The search is what finishes a
// landing the target already carries, so a pass that carries none — the
// ordinary landing — must not pay for it (gt-064oh).
func TestLandDoesNotSearchTheTargetForAFreshLanding(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	moved := f.pushMain("c.txt", "other\n")
	f.base = moved
	lookups := &subjectLookups{}
	l := f.lander()
	l.openRepo = func(dir string) Repo { return countedRepo{Repo: f.git.Open(dir), s: lookups} }
	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land: %v", err)
	}
	if lines := f.landingLines(); len(lines) != 1 || f.bead().Status != "closed" {
		t.Fatalf("the fixture's landing left %d landings lines and the bead %s; the test proves nothing", len(lines), f.bead().Status)
	}
	if lookups.n != 0 {
		t.Errorf("a fresh landing searched the target's history %d time(s) for a land merge naming this head", lookups.n)
	}
}

// TestLandFinishesTheUnrecordedLandingARevertQuotes: an operator reverts the
// landing whose record was never written, and the revert's message quotes the
// land merge's subject, so a search of whole commit messages finds the revert
// first. The pass still finishes the merge this lander made: the revert is not
// the landing, and the record names the landing (gt-h7trd).
func TestLandFinishesTheUnrecordedLandingARevertQuotes(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	var merged string
	f.merger.fn = func(req MergeRequest) error {
		merged = req.Head
		f.setMain(req.Head)
		return errors.New("the pass ended after the merge, before the record")
	}
	if _, err := l.Land(context.Background(), f.work); err == nil {
		t.Fatal("the first pass returned a landing it never recorded")
	}
	subject := landMergeMarker(f.work) + shortSHA(f.base) + ")"
	revert := f.git.Commit(f.t, f.origin, "main", "Revert \""+subject+"\"\n\nThis reverts commit "+merged+".", map[string]string{"a.txt": "reverted\n"})
	if got := f.originMain(); got != revert {
		t.Fatalf("origin/main = %s, want the revert %s", got, revert)
	}

	f.merger.fn = nil
	res, err := f.lander().Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("the second pass did not finish the landing: %v", err)
	}
	if res.LandedCommit != merged {
		t.Errorf("LandedCommit = %s, want the merge %s (the revert is %s)", res.LandedCommit, merged, revert)
	}
	if len(f.merger.calls) != 1 {
		t.Errorf("the merger ran %d times, want the one merge", len(f.merger.calls))
	}
	b := f.bead()
	if b.Status != "closed" || !strings.Contains(b.Notes, LandingNoteMarker+"\nlanded_commit: "+merged) {
		t.Errorf("bead after the finished landing: status=%s notes=%q", b.Status, b.Notes)
	}
	if strings.Contains(b.Notes, revert) {
		t.Errorf("the record names the revert as the landing:\n%s", b.Notes)
	}
	if lines := f.landingLines(); len(lines) != 1 {
		t.Fatalf("landings lines = %d, want 1", len(lines))
	}
}

// TestLandRecordsALandingAnotherPushMovedTheTargetPast: the target advanced
// between the merge and the read-back. The commit is in the target's history,
// so the landing is recorded rather than failed.
func TestLandRecordsALandingAnotherPushMovedTheTargetPast(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	var merged string
	var askedTarget func(string) (bool, error)
	f.merger.fn = func(req MergeRequest) error {
		merged, askedTarget = req.Head, req.TargetHasCommit
		f.setMain(req.Head)
		f.pushMain("c.txt", "someone else landed\n")
		return nil
	}
	res, err := l.Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	if res.LandedCommit != merged {
		t.Errorf("LandedCommit = %s, want the merge %s", res.LandedCommit, merged)
	}
	if f.originMain() == merged {
		t.Fatal("the target did not advance; the test proves nothing")
	}
	if askedTarget == nil {
		t.Fatal("the merger got no target check")
	}
	if ok, err := askedTarget(merged); err != nil || !ok {
		t.Errorf("TargetHasCommit(%s) = %v, %v; want true for a commit on the target", merged, ok, err)
	}
	b := f.bead()
	if b.Status != "closed" || !strings.Contains(b.Notes, "landed_commit: "+merged) {
		t.Errorf("bead = status %s, notes %q", b.Status, b.Notes)
	}
	if n := len(f.landingLines()); n != 1 {
		t.Errorf("landings lines = %d, want 1", n)
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

// failingBeads fails AppendNotes, Update, or Show after the first call.
type failingBeads struct {
	*beadsfake.Fake
	failNotes  bool
	failUpdate bool
	shows      int
	failShow   bool
	// failShowNow fails the first Show too, for a caller whose only read it
	// is: the post-landing queue runs after every other read of the bead
	// (gt-qtj72).
	failShowNow bool
}

func (b *failingBeads) AppendNotes(id, note string) error {
	if b.failNotes {
		return errors.New("database is locked")
	}
	return b.Fake.AppendNotes(id, note)
}

func (b *failingBeads) Update(id string, opts beads.UpdateOptions) error {
	if b.failUpdate {
		return errors.New("database is locked")
	}
	return b.Fake.Update(id, opts)
}

func (b *failingBeads) Show(id string) (*beads.Issue, error) {
	b.shows++
	if b.failShowNow || (b.failShow && b.shows > 1) {
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

// TestLandRejectionRetryDoesNotBurnAttempts: a rejection whose block was
// written but whose bead update failed is finished on the retry, not recorded
// again. Otherwise each retry appends another block, the count reaches
// MaxReworkAttempts and the bead is escalated having never told the author why
// (gt-zqqcr).
func TestLandRejectionRetryDoesNotBurnAttempts(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	f.gate.fn = func(string) GateResult { return GateResult{Steps: []StepResult{{Name: "test", ExitCode: 1}}} }
	l := f.lander()
	l.Beads = &failingBeads{Fake: f.bd, failUpdate: true}
	for round := 1; round <= MaxReworkAttempts; round++ {
		_, err := l.Land(context.Background(), f.work)
		var rej *Rejection
		if !errors.As(err, &rej) {
			t.Fatalf("round %d: Land = %T %v, want a rejection", round, err, err)
		}
		if rej.RecordErr == nil || !strings.Contains(rej.RecordErr.Error(), "reopening the bead") {
			t.Fatalf("round %d: RecordErr = %v, want the failed reopen", round, rej.RecordErr)
		}
		// The refusal never reached the author, so no round is a new attempt.
		if got := CountRejections(f.bead().Notes); got != 1 {
			t.Fatalf("round %d: %d rejection block(s), want 1", round, got)
		}
	}
	b := f.bead()
	if beads.HasLabel(b, LabelNeedsHuman) || !beads.HasLabel(b, LabelReadyToLand) {
		t.Errorf("labels after %d failed records = %v; want the bead still ready and not escalated", MaxReworkAttempts, b.Labels)
	}
	if strings.Contains(b.Notes, "the loop is escalated") {
		t.Errorf("notes claim the loop escalated: %s", b.Notes)
	}
}

// TestStageTimesReportsEveryGateStep: a landing whose gate ran several stages
// says how long each took, beside om (gt-vsct7.8).
func TestStageTimesReportsEveryGateStep(t *testing.T) {
	t.Parallel()
	got := stageTimes(GateResult{Steps: []StepResult{
		{Name: "lint", Elapsed: 18 * time.Second},
		{Name: "gate", Elapsed: 92 * time.Second},
		{Name: "test", Elapsed: 12 * time.Second},
	}}, 0, false)
	if !strings.Contains(got, "test 12s") {
		t.Errorf("stageTimes = %q, want every step's wall time", got)
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

// The creator check, slice 7 of the Forgejo landing design: any write-access
// user can post a commit status, so before it merges, the worker asserts the
// gate status carries no user creator and om / review was posted by the
// landing bot. A status failing either check is escalated, never merged.

// TestForgejoMergerAcceptsACleanCandidate is the check's pass arm: the gate
// status came from the workflow run and om / review from the landing bot, so
// the merge goes through.
func TestForgejoMergerAcceptsACleanCandidate(t *testing.T) {
	t.Parallel()
	client := &fakePulls{}
	if err := testMerger(client).Merge(context.Background(), mergeRequestForTest(Verdict{Verdict: VerdictApprove})); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(client.merges) != 1 {
		t.Fatalf("merged %d time(s), want once", len(client.merges))
	}
}

// TestForgejoMergerRefusesAUserPostedGateStatus: a gate status posted by an
// account rather than by the run is a forged gate, so the landing is refused
// and nothing is merged.
func TestForgejoMergerRefusesAUserPostedGateStatus(t *testing.T) {
	t.Parallel()
	client := &fakePulls{combined: &forgejo.CombinedStatus{Statuses: []forgejo.CommitStatus{
		gateStatus(&forgejo.User{Login: "mallory"}),
		reviewStatus(testBotLogin),
	}}}
	err := testMerger(client).Merge(context.Background(), mergeRequestForTest(Verdict{Verdict: VerdictApprove}))
	var forged *ForgedStatusError
	if !errors.As(err, &forged) {
		t.Fatalf("Merge error = %T %v, want a *ForgedStatusError", err, err)
	}
	if !strings.Contains(forged.Reason, "mallory") || !strings.Contains(forged.Reason, testGateContext) {
		t.Fatalf("reason %q; want it to name the posting account and the %s context", forged.Reason, testGateContext)
	}
	if len(client.merges) != 0 {
		t.Fatal("a candidate whose gate status a user posted was merged")
	}
	if len(client.created) != 0 {
		t.Fatal("a land pull request was opened for a candidate that failed the creator check")
	}
}

// TestForgejoMergerRefusesAReviewNotFromTheLandingBot: om / review posted by
// any other account is not the review the worker posted, so the merge pinned
// to it is refused.
func TestForgejoMergerRefusesAReviewNotFromTheLandingBot(t *testing.T) {
	t.Parallel()
	client := &fakePulls{combined: &forgejo.CombinedStatus{Statuses: []forgejo.CommitStatus{
		gateStatus(nil),
		reviewStatus("mallory"),
	}}}
	err := testMerger(client).Merge(context.Background(), mergeRequestForTest(Verdict{Verdict: VerdictApprove}))
	var forged *ForgedStatusError
	if !errors.As(err, &forged) {
		t.Fatalf("Merge error = %T %v, want a *ForgedStatusError", err, err)
	}
	if !strings.Contains(forged.Reason, "mallory") || !strings.Contains(forged.Reason, testBotLogin) {
		t.Fatalf("reason %q; want it to name the posting account and the landing bot", forged.Reason)
	}
	if len(client.merges) != 0 {
		t.Fatal("a candidate whose review another account posted was merged")
	}
}

// TestForgejoMergerRefusesAReviewWithNoCreator: a review status a workflow run
// posted is not the landing bot's either, so it fails the check rather than
// passing it for want of a creator to compare.
func TestForgejoMergerRefusesAReviewWithNoCreator(t *testing.T) {
	t.Parallel()
	client := &fakePulls{combined: &forgejo.CombinedStatus{Statuses: []forgejo.CommitStatus{
		gateStatus(nil),
		reviewStatus(""),
	}}}
	err := testMerger(client).Merge(context.Background(), mergeRequestForTest(Verdict{Verdict: VerdictApprove}))
	var forged *ForgedStatusError
	if !errors.As(err, &forged) {
		t.Fatalf("Merge error = %T %v, want a *ForgedStatusError", err, err)
	}
	if len(client.merges) != 0 {
		t.Fatal("a candidate with no landing bot review was merged")
	}
}

// TestForgejoMergerChecksEveryStatusOfAContext: the workflow's own status does
// not vouch for a user-posted one beside it. Reading only the first status the
// API returns would let a forged one hide behind it.
func TestForgejoMergerChecksEveryStatusOfAContext(t *testing.T) {
	t.Parallel()
	client := &fakePulls{combined: &forgejo.CombinedStatus{Statuses: []forgejo.CommitStatus{
		gateStatus(nil),
		gateStatus(&forgejo.User{Login: "mallory"}),
		reviewStatus(testBotLogin),
	}}}
	err := testMerger(client).Merge(context.Background(), mergeRequestForTest(Verdict{Verdict: VerdictApprove}))
	var forged *ForgedStatusError
	if !errors.As(err, &forged) {
		t.Fatalf("Merge error = %T %v, want a *ForgedStatusError", err, err)
	}
	if len(client.merges) != 0 {
		t.Fatal("a user-posted gate status was overlooked because a workflow's status came first")
	}
}

// TestForgejoMergerMissingGateStatusIsInfrastructure: the candidate gate read
// this context as a success moments ago, so its absence now is an anomaly
// rather than a verdict, and the landing is retried rather than rejected or
// merged.
func TestForgejoMergerMissingGateStatusIsInfrastructure(t *testing.T) {
	t.Parallel()
	client := &fakePulls{combined: &forgejo.CombinedStatus{Statuses: []forgejo.CommitStatus{
		reviewStatus(testBotLogin),
	}}}
	err := testMerger(client).Merge(context.Background(), mergeRequestForTest(Verdict{Verdict: VerdictApprove}))
	var infra *InfraError
	if !errors.As(err, &infra) || infra.Stage != "read the candidate's statuses" {
		t.Fatalf("Merge error = %T %v, want an *InfraError at the status read", err, err)
	}
	if len(client.merges) != 0 {
		t.Fatal("a candidate with no gate status was merged")
	}
}

// TestForgejoMergerStatusReadFailureIsInfrastructure: a status list the API
// will not return says nothing about the work, so the landing is retried.
func TestForgejoMergerStatusReadFailureIsInfrastructure(t *testing.T) {
	t.Parallel()
	client := &fakePulls{combinedErr: errors.New("connection refused")}
	err := testMerger(client).Merge(context.Background(), mergeRequestForTest(Verdict{Verdict: VerdictApprove}))
	var infra *InfraError
	if !errors.As(err, &infra) {
		t.Fatalf("Merge error = %T %v, want an *InfraError", err, err)
	}
	if len(client.merges) != 0 {
		t.Fatal("a candidate was merged without its statuses being read")
	}
}

// TestLandForgedStatusIsAHumanRejection: the forgery check's failure reaches
// the bead as a rejection no author can rework and no retry lifts, so the
// landing worker escalates it (outRejectedHuman) instead of merging.
func TestLandForgedStatusIsAHumanRejection(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	l.Candidate = &fakeCandidate{res: CandidateResult{State: CandidatePassed, Context: "ci / gate (push)"}}
	l.Merger = &fakeMerger{fn: func(MergeRequest) error {
		return &ForgedStatusError{Reason: `the ci / gate (push) status on the candidate abc1234 was posted by user "mallory"`}
	}}

	_, err := l.Land(context.Background(), f.work)
	rej := f.assertRejected(t, err, RejectForgedStatus, LabelNeedsHuman)
	if !strings.Contains(rej.Reason, "mallory") {
		t.Fatalf("reason %q; want it to name the account that posted the status", rej.Reason)
	}
	if rej.Rework {
		t.Fatal("a forged status was left to the author to rework; only a human can settle it")
	}
}

// writeCountingBeads counts the writes made on the work bead; each is a Dolt
// commit.
type writeCountingBeads struct {
	*beadsfake.Fake
	notes, updates, closes int
}

func (b *writeCountingBeads) AppendNotes(id, note string) error {
	b.notes++
	return b.Fake.AppendNotes(id, note)
}

func (b *writeCountingBeads) Update(id string, opts beads.UpdateOptions) error {
	b.updates++
	return b.Fake.Update(id, opts)
}

func (b *writeCountingBeads) ForceCloseWithReason(reason string, ids ...string) error {
	b.closes++
	return b.Fake.ForceCloseWithReason(reason, ids...)
}

// TestLandRecordsTheBeadInTwoWrites: a landing writes the LANDING RECORD note
// and the ready label's removal in one update, then the close: two Dolt
// commits on the work bead, with the READY TO LAND write before them making
// three from submission to close (gt-uhdbi).
func TestLandRecordsTheBeadInTwoWrites(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	bd := &writeCountingBeads{Fake: f.bd}
	l := f.lander()
	l.Beads = bd
	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land: %v", err)
	}
	if bd.notes != 0 || bd.updates != 1 || bd.closes != 1 {
		t.Errorf("writes = %d notes, %d updates, %d closes; want 0, 1, 1", bd.notes, bd.updates, bd.closes)
	}
	b := f.bead()
	if b.Status != "closed" || beads.HasLabel(b, LabelReadyToLand) || strings.Count(b.Notes, LandingNoteMarker) != 1 {
		t.Errorf("after landing: status=%s labels=%v notes=%q", b.Status, b.Labels, b.Notes)
	}
}

// TestLandRepairWritesNoSecondRecord: a pass that finds the record and the
// label change already written (only the close failed) writes just the
// close, not the note again.
func TestLandRepairWritesNoSecondRecord(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	bd := &writeCountingBeads{Fake: f.bd}
	l := f.lander()
	l.Beads = &countingFlaky{flakyCloseBeads: flakyCloseBeads{Fake: f.bd}, count: bd}
	if _, err := l.Land(context.Background(), f.work); err == nil {
		t.Fatal("first Land: want the failed close reported")
	}
	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("repair Land: %v", err)
	}
	if bd.updates != 1 {
		t.Errorf("updates = %d, want 1: the repair pass found the record written and rewrote it", bd.updates)
	}
	if n := strings.Count(f.bead().Notes, LandingNoteMarker); n != 1 {
		t.Errorf("%d landing records after the repair, want 1", n)
	}
}

// countingFlaky fails the first close and counts the updates that reach the
// bead.
type countingFlaky struct {
	flakyCloseBeads
	count *writeCountingBeads
}

func (b *countingFlaky) Update(id string, opts beads.UpdateOptions) error {
	b.count.updates++
	return b.Fake.Update(id, opts)
}

// TestLandingRecordAgent: a landing records the agent its seat ran on, in the
// landings file and in the bead's LANDING RECORD block; a landing with no agent
// — a crew's, which has no seat, or a seat whose agent bead named none — records
// no agent at all rather than an empty line the reader would count as a seat
// (gt-mukgk.6).
func TestLandingRecordAgent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		worker, agent string
	}{
		{"seat with an agent", "opal", "claude-haiku"},
		{"seat whose agent bead names none", "opal", ""},
		{"crew landing", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			f.work.Worker = tc.worker
			f.work.Agent = tc.agent
			if _, err := f.lander().Land(context.Background(), f.work); err != nil {
				t.Fatalf("Land: %v", err)
			}
			line := f.landingLines()[0]
			var rec LandingRecord
			if err := json.Unmarshal([]byte(line), &rec); err != nil {
				t.Fatal(err)
			}
			if rec.Agent != tc.agent {
				t.Errorf("landings file agent = %q, want %q:\n%s", rec.Agent, tc.agent, line)
			}
			if got := strings.Contains(line, `"agent"`); got != (tc.agent != "") {
				t.Errorf("landings line has an agent key = %v, want %v: %s", got, tc.agent != "", line)
			}
			bead, err := f.bd.Show("gt-abc")
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Contains(bead.Notes, "\nagent:"); got != (tc.agent != "") {
				t.Errorf("LANDING RECORD block has an agent line = %v, want %v:\n%s", got, tc.agent != "", bead.Notes)
			}
			if tc.agent != "" && !strings.Contains(bead.Notes, "\nagent: "+tc.agent) {
				t.Errorf("the bead's LANDING RECORD block carries no %q agent line:\n%s", tc.agent, bead.Notes)
			}
		})
	}
}

// TestPolicyRejectionCountsDescriptionAcceptance: the spec dispatcher leaves
// the acceptance field empty and shapes the criteria as a "## Acceptance"
// section of the description, so that section is where policyRejection must
// count unchecked boxes (the same text CriteriaText hands to om).
func TestPolicyRejectionCountsDescriptionAcceptance(t *testing.T) {
	t.Parallel()
	issue := &beads.Issue{ID: "gt-abc", Title: "add b", Status: "hooked", Type: "task",
		AcceptanceCriteria: "", Description: "## Goal\nx\n\n## Acceptance\n- [ ] x"}
	rej := policyRejection(issue)
	if rej == nil || !rej.Rework || rej.Kind != RejectPolicy || !strings.Contains(rej.Reason, "1 unchecked acceptance criteria") {
		t.Fatalf("policyRejection = %+v, want a Rework policy rejection naming 1 unchecked criterion", rej)
	}
	issue.Description = "## Acceptance\n- [x] x\n\n## Notes\n- [ ] not a criterion"
	if rej := policyRejection(issue); rej != nil {
		t.Errorf("ticked section with an unchecked box in another section: rejection = %+v", rej)
	}
	issue.Description = "## Goal\nno acceptance section"
	if rej := policyRejection(issue); rej != nil {
		t.Errorf("bead with no boxes: rejection = %+v", rej)
	}
}
