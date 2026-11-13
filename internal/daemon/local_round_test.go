package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
)

// roundClock is the fixed time every block and bound day is taken from.
var roundClock = time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)

// fakeRoundBeads holds the open beads and the notes appended to them. An
// appended note lands on the bead's Notes, so the next tick reads it back.
type fakeRoundBeads struct {
	issues   []*beads.Issue
	comments []beads.Comment
	cleaned  int
	appended int
}

func (f *fakeRoundBeads) list() ([]*beads.Issue, error) { return f.issues, nil }

func (f *fakeRoundBeads) commentsOf(string) ([]beads.Comment, error) { return f.comments, nil }

func (f *fakeRoundBeads) appendNote(bead, note string) error {
	f.appended++
	for _, issue := range f.issues {
		if issue.ID == bead {
			issue.Notes += "\n" + note
			return nil
		}
	}
	return errors.New("no such bead " + bead)
}

// fakeRoundReviewer returns its findings, or its error, and keeps each input.
type fakeRoundReviewer struct {
	findings []land.Finding
	err      error
	inputs   []land.ReviewInput
}

func (r *fakeRoundReviewer) ReviewBead(_ context.Context, in land.ReviewInput) (land.Verdict, error) {
	r.inputs = append(r.inputs, in)
	if r.err != nil {
		return land.Verdict{}, r.err
	}
	return land.Verdict{Findings: r.findings}, nil
}

// roundConfig returns a config reader for one switch and timeout setting.
func roundConfig(enabled bool, timeoutSeconds int) func() *config.DaemonPatrolConfig {
	c := &config.DaemonPatrolConfig{LocalRoundSetting: &config.LocalRoundConfig{Enabled: enabled, TimeoutSeconds: timeoutSeconds}}
	return func() *config.DaemonPatrolConfig { return c }
}

// newRoundPatrol builds a patrol over the fakes, with the switch on, the om
// not paused and a fresh bound directory.
func newRoundPatrol(t *testing.T, beadStore *fakeRoundBeads, reviewer *fakeRoundReviewer) *LocalRoundPatrol {
	t.Helper()
	return &LocalRoundPatrol{
		ListRequests: beadStore.list,
		Comments:     beadStore.commentsOf,
		AppendNote:   beadStore.appendNote,
		Fetch: func(string, string) (localRoundTree, error) {
			return localRoundTree{Dir: "/tree", Base: "base", Cleanup: func() { beadStore.cleaned++ }}, nil
		},
		Reviewer: reviewer,
		Bound:    land.Bound{Dir: t.TempDir(), Now: func() time.Time { return roundClock }},
		Config:   roundConfig(true, 0),
		Operator: func() (*config.OperatorConfig, error) { return nil, nil },
		Paused:   func() string { return "" },
		Now:      func() time.Time { return roundClock },
		Logf:     func(string, ...any) {},
	}
}

// oneFinding is the finding a round raises in these tests.
var oneFinding = land.Finding{ID: "f1", Severity: "major", Path: "a.go", Line: 3, Title: "nil dereference"}

func TestLocalRoundServesARequestOnce(t *testing.T) {
	t.Parallel()
	beadStore := &fakeRoundBeads{issues: []*beads.Issue{{ID: "gt-ryp1ff", Notes: "LOCAL ROUND REQUEST head=abcdef1"}}}
	reviewer := &fakeRoundReviewer{findings: []land.Finding{oneFinding}}
	p := newRoundPatrol(t, beadStore, reviewer)

	p.Tick(context.Background())
	p.Tick(context.Background())

	if len(reviewer.inputs) != 1 {
		t.Fatalf("reviewer ran %d times over two ticks; want once", len(reviewer.inputs))
	}
	want := land.FormatLocalRound(1, "abcdef1", []land.Finding{oneFinding}, roundClock)
	if !strings.HasSuffix(beadStore.issues[0].Notes, "\n"+want) {
		t.Fatalf("notes after serving:\n%s\nwant the block:\n%s", beadStore.issues[0].Notes, want)
	}
	if got := strings.Count(beadStore.issues[0].Notes, "LOCAL ROUND (1):"); got != 1 {
		t.Fatalf("notes carry %d round-1 blocks; want exactly one", got)
	}
	if beadStore.cleaned != 1 {
		t.Fatalf("worktree cleaned %d times; want once", beadStore.cleaned)
	}
}

func TestLocalRoundFailuresAreRecordedAsUnavailable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		setup  func(p *LocalRoundPatrol, t *testing.T)
		reason string
		// notes is the bead's notes when it is not the usual valid request.
		notes string
		// hidden is text the reviewer's error carries, which the answer must not echo.
		hidden string
	}{
		{
			name:   "switch off",
			setup:  func(p *LocalRoundPatrol, _ *testing.T) { p.Config = roundConfig(false, 0) },
			reason: "daemon.local_round is off",
		},
		{
			name:   "timeout refused",
			setup:  func(p *LocalRoundPatrol, _ *testing.T) { p.Config = roundConfig(true, -1) },
			reason: "daemon.local_round.timeout_seconds",
		},
		{
			name:   "om paused",
			setup:  func(p *LocalRoundPatrol, _ *testing.T) { p.Paused = func() string { return "a hold" } },
			reason: "the landing worker's om is paused: a hold",
		},
		{
			name: "bound refused",
			setup: func(p *LocalRoundPatrol, t *testing.T) {
				for range 2 {
					if _, err := p.Bound.Allow("gt-ryp1ff"); err != nil {
						t.Fatalf("priming the bound: %v", err)
					}
				}
			},
			reason: "submit with gt done",
		},
		{
			name:   "fetch failure",
			setup:  func(p *LocalRoundPatrol, _ *testing.T) { p.Fetch = failingFetch },
			reason: "could not fetch the head",
			hidden: "no such commit",
		},
		{
			name: "reviewer error",
			setup: func(p *LocalRoundPatrol, _ *testing.T) {
				p.Reviewer = &fakeRoundReviewer{err: errors.New("om printed secrets")}
			},
			reason: "the om review failed",
			hidden: "om printed secrets",
		},
		{
			name:   "comments unreadable",
			setup:  func(p *LocalRoundPatrol, _ *testing.T) { p.Comments = failingComments },
			reason: "could not read the bead's comments",
			hidden: "bd unreachable",
		},
		{
			name:   "operator config unreadable",
			setup:  func(p *LocalRoundPatrol, _ *testing.T) { p.Operator = failingOperator },
			reason: "could not read the operator identities",
			hidden: "settings unreadable",
		},
		{
			name:   "request names no head",
			setup:  func(p *LocalRoundPatrol, _ *testing.T) {},
			reason: "the request names no valid head",
			notes:  "LOCAL ROUND REQUEST head=--upload-pack",
		},
		{
			name: "review timed out",
			setup: func(p *LocalRoundPatrol, _ *testing.T) {
				p.Reviewer = &fakeRoundReviewer{err: context.DeadlineExceeded}
			},
			reason: "did not finish within",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			request := tc.notes
			if request == "" {
				request = "LOCAL ROUND REQUEST head=abcdef1"
			}
			beadStore := &fakeRoundBeads{issues: []*beads.Issue{{ID: "gt-ryp1ff", Notes: request}}}
			p := newRoundPatrol(t, beadStore, &fakeRoundReviewer{findings: []land.Finding{oneFinding}})
			tc.setup(p, t)

			p.Tick(context.Background())
			p.Tick(context.Background())

			notes := beadStore.issues[0].Notes
			if n := strings.Count(notes, "LOCAL ROUND ("); n != 1 {
				t.Fatalf("notes carry %d LOCAL ROUND lines after two ticks; the answered request must not be retried", n)
			}
			line := notes[strings.LastIndex(notes, "\n")+1:]
			if !strings.HasPrefix(line, "LOCAL ROUND (1): unavailable - ") {
				t.Fatalf("answer %q; want an unavailable line for round 1", line)
			}
			if !strings.Contains(line, tc.reason) {
				t.Fatalf("answer %q does not name the reason %q", line, tc.reason)
			}
			if strings.Contains(notes, "LOCAL ROUND FINDING") || strings.Contains(notes, "head=abcdef1 at=") {
				t.Fatalf("a failed round wrote a block with findings or a head:\n%s", notes)
			}
			if tc.hidden != "" && strings.Contains(notes, tc.hidden) {
				t.Fatalf("the answer echoes the reviewer's error text %q:\n%s", tc.hidden, notes)
			}
		})
	}
}

func failingFetch(string, string) (localRoundTree, error) {
	return localRoundTree{}, errors.New("no such commit")
}

// boundCount reads the stored round count for bead, or 0 when the bound holds
// no state file for it yet.
func boundCount(t *testing.T, bound land.Bound, bead string) int {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(bound.Dir, bead+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatalf("reading the bound state for %s: %v", bead, err)
	}
	var st struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(body, &st); err != nil {
		t.Fatalf("parsing the bound state for %s: %v", bead, err)
	}
	return st.Count
}

// A request whose head cannot be fetched never reaches the reviewer, so it
// spends none of the bead's daily rounds: three failures leave the bound at
// zero, and the next fetchable request is the day's first round (gt-u9r6ey).
func TestLocalRoundFetchFailureDoesNotCountTowardTheBound(t *testing.T) {
	t.Parallel()
	beadStore := &fakeRoundBeads{issues: []*beads.Issue{{ID: "gt-ryp1ff", Notes: "LOCAL ROUND REQUEST head=abcdef1"}}}
	p := newRoundPatrol(t, beadStore, &fakeRoundReviewer{findings: []land.Finding{oneFinding}})
	p.Fetch = failingFetch

	for i := 1; i <= 3; i++ {
		p.Tick(context.Background())
		if got := strings.Count(beadStore.issues[0].Notes, "unavailable - could not fetch the head"); got != i {
			t.Fatalf("after %d unanswered fetches the notes carry %d fetch answers; want %d", i, got, i)
		}
		if got := boundCount(t, p.Bound, "gt-ryp1ff"); got != 0 {
			t.Fatalf("after %d requests whose fetch failed the bound count = %d; want 0", i, got)
		}
		beadStore.issues[0].Notes += "\nLOCAL ROUND REQUEST head=abcdef1"
	}
	p.Fetch = func(string, string) (localRoundTree, error) {
		return localRoundTree{Dir: "/tree", Base: "base", Cleanup: func() { beadStore.cleaned++ }}, nil
	}

	p.Tick(context.Background())

	if got := boundCount(t, p.Bound, "gt-ryp1ff"); got != 1 {
		t.Fatalf("the fetchable request left the bound at %d; want the day's first round", got)
	}
	if want := land.FormatLocalRound(4, "abcdef1", []land.Finding{oneFinding}, roundClock); !strings.HasSuffix(beadStore.issues[0].Notes, "\n"+want) {
		t.Fatalf("notes after the fetchable request:\n%s\nwant round 4's block:\n%s", beadStore.issues[0].Notes, want)
	}
}

// A fetch that succeeds and is then refused by the bound still cleans its tree
// up, and the answer is the bound's own reason (gt-u9r6ey).
func TestLocalRoundBoundRefusalAfterFetchCleansUp(t *testing.T) {
	t.Parallel()
	beadStore := &fakeRoundBeads{issues: []*beads.Issue{{ID: "gt-ryp1ff", Notes: "LOCAL ROUND REQUEST head=abcdef1"}}}
	p := newRoundPatrol(t, beadStore, &fakeRoundReviewer{findings: []land.Finding{oneFinding}})
	for range 2 {
		if _, err := p.Bound.Allow("gt-ryp1ff"); err != nil {
			t.Fatalf("priming the bound: %v", err)
		}
	}

	p.Tick(context.Background())

	notes := beadStore.issues[0].Notes
	line := notes[strings.LastIndex(notes, "\n")+1:]
	if !strings.HasPrefix(line, "LOCAL ROUND (1): unavailable - ") || !strings.Contains(line, "submit with gt done") {
		t.Fatalf("answer %q; want an unavailable line naming the bound", line)
	}
	if beadStore.cleaned != 1 {
		t.Fatalf("the tree was cleaned %d times; want the bound's refusal to clean it once", beadStore.cleaned)
	}
}

// A round whose reviewer runs and then fails still spends one of the day's
// rounds: it spent the review budget (gt-u9r6ey).
func TestLocalRoundReviewFailureStillCounts(t *testing.T) {
	t.Parallel()
	beadStore := &fakeRoundBeads{issues: []*beads.Issue{{ID: "gt-ryp1ff", Notes: "LOCAL ROUND REQUEST head=abcdef1"}}}
	p := newRoundPatrol(t, beadStore, &fakeRoundReviewer{err: errors.New("om failed")})

	p.Tick(context.Background())

	notes := beadStore.issues[0].Notes
	line := notes[strings.LastIndex(notes, "\n")+1:]
	if !strings.HasPrefix(line, "LOCAL ROUND (1): unavailable - ") || !strings.Contains(line, "the om review failed") {
		t.Fatalf("answer %q; want an unavailable line naming the review failure", line)
	}
	if got := boundCount(t, p.Bound, "gt-ryp1ff"); got != 1 {
		t.Fatalf("a failed review left the bound at %d; want 1", got)
	}
}

func failingComments(string) ([]beads.Comment, error) {
	return nil, errors.New("bd unreachable")
}

func failingOperator() (*config.OperatorConfig, error) {
	return nil, errors.New("settings unreadable")
}

func TestLocalRoundTickSkipsWhenTheBeadsCannotBeListed(t *testing.T) {
	t.Parallel()
	reviewer := &fakeRoundReviewer{}
	beadStore := &fakeRoundBeads{}
	p := newRoundPatrol(t, beadStore, reviewer)
	p.ListRequests = func() ([]*beads.Issue, error) { return nil, errors.New("bd unreachable") }

	p.Tick(context.Background())

	if len(reviewer.inputs) != 0 || beadStore.appended != 0 {
		t.Fatalf("a failed listing ran %d rounds and wrote %d notes; want neither", len(reviewer.inputs), beadStore.appended)
	}
}

func TestLocalRoundTickStopsWhenCancelled(t *testing.T) {
	t.Parallel()
	beadStore := &fakeRoundBeads{issues: []*beads.Issue{{ID: "gt-ryp1ff", Notes: "LOCAL ROUND REQUEST head=abcdef1"}}}
	reviewer := &fakeRoundReviewer{}
	p := newRoundPatrol(t, beadStore, reviewer)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p.Tick(ctx)

	if len(reviewer.inputs) != 0 {
		t.Fatalf("a cancelled tick ran %d rounds; want none", len(reviewer.inputs))
	}
}

func TestLocalRoundIgnoresBeadsWithoutARequest(t *testing.T) {
	t.Parallel()
	beadStore := &fakeRoundBeads{issues: []*beads.Issue{{ID: "gt-ryp1ff", Notes: "a note with no request"}}}
	reviewer := &fakeRoundReviewer{}
	p := newRoundPatrol(t, beadStore, reviewer)

	p.Tick(context.Background())

	if len(reviewer.inputs) != 0 || beadStore.appended != 0 {
		t.Fatalf("a bead with no request ran %d rounds; want none", len(reviewer.inputs))
	}
}

func TestLocalRoundAnswerFailureIsLoggedAndNotRetriedInThisTick(t *testing.T) {
	t.Parallel()
	beadStore := &fakeRoundBeads{issues: []*beads.Issue{{ID: "gt-ryp1ff", Notes: "LOCAL ROUND REQUEST head=abcdef1"}}}
	reviewer := &fakeRoundReviewer{findings: []land.Finding{oneFinding}}
	p := newRoundPatrol(t, beadStore, reviewer)
	var logged []string
	p.Logf = func(format string, args ...any) { logged = append(logged, format) }
	p.AppendNote = func(string, string) error { return errors.New("bd unreachable") }

	p.Tick(context.Background())

	if len(reviewer.inputs) != 1 {
		t.Fatalf("reviewer ran %d times; want one round", len(reviewer.inputs))
	}
	if len(logged) != 1 {
		t.Fatalf("logged %d lines for a failed append; want the one line naming it", len(logged))
	}
}

func TestLocalRoundServesOldestRequestFirst(t *testing.T) {
	t.Parallel()
	beadStore := &fakeRoundBeads{issues: []*beads.Issue{
		{ID: "gt-newer", CreatedAt: "2026-11-13T08:00:00Z", Notes: "LOCAL ROUND REQUEST head=abcdef1"},
		{ID: "gt-older", CreatedAt: "2026-11-12T08:00:00Z", Notes: "LOCAL ROUND REQUEST head=1234567"},
	}}
	reviewer := &fakeRoundReviewer{}
	p := newRoundPatrol(t, beadStore, reviewer)

	p.Tick(context.Background())

	if len(reviewer.inputs) != 2 {
		t.Fatalf("reviewer ran %d times; want one round per bead", len(reviewer.inputs))
	}
	if got := reviewer.inputs[0].BeadID; got != "gt-older" {
		t.Fatalf("first round served %s; want the oldest request, gt-older", got)
	}
}

func TestLocalRoundServesABeadWithoutATimeFirst(t *testing.T) {
	t.Parallel()
	beadStore := &fakeRoundBeads{issues: []*beads.Issue{
		{ID: "gt-dated", CreatedAt: "2026-11-12T08:00:00Z", Notes: "LOCAL ROUND REQUEST head=abcdef1"},
		{ID: "gt-undated", CreatedAt: "not a time", Notes: "LOCAL ROUND REQUEST head=1234567"},
	}}
	reviewer := &fakeRoundReviewer{}
	p := newRoundPatrol(t, beadStore, reviewer)

	p.Tick(context.Background())

	if got := reviewer.inputs[0].BeadID; got != "gt-undated" {
		t.Fatalf("first round served %s; want the bead whose time does not parse, sorted first", got)
	}
}

func TestLocalRoundUsesPriorBlockAsPriorFindings(t *testing.T) {
	t.Parallel()
	beadStore := &fakeRoundBeads{issues: []*beads.Issue{{ID: "gt-ryp1ff", Notes: "LOCAL ROUND REQUEST head=abcdef1"}}}
	reviewer := &fakeRoundReviewer{findings: []land.Finding{oneFinding}}
	p := newRoundPatrol(t, beadStore, reviewer)
	p.Tick(context.Background())

	beadStore.issues[0].Notes += "\nLOCAL ROUND REQUEST head=1234567"
	reviewer.findings = nil
	p.Tick(context.Background())

	if len(reviewer.inputs) != 2 {
		t.Fatalf("reviewer ran %d times; want 2", len(reviewer.inputs))
	}
	got := reviewer.inputs[1].PriorFindings
	if len(got) != 1 || got[0].ID != oneFinding.ID {
		t.Fatalf("second round's PriorFindings = %+v; want the first block's finding %s", got, oneFinding.ID)
	}
}

func TestLocalRoundPriorBlockSurvivesAnUnavailableLine(t *testing.T) {
	t.Parallel()
	first := land.FormatLocalRound(1, "abcdef1", []land.Finding{oneFinding}, roundClock)
	beadStore := &fakeRoundBeads{issues: []*beads.Issue{{
		ID:    "gt-ryp1ff",
		Notes: first + "\nLOCAL ROUND (2): unavailable - the om review failed\nLOCAL ROUND REQUEST head=1234567",
	}}}
	reviewer := &fakeRoundReviewer{}
	p := newRoundPatrol(t, beadStore, reviewer)

	p.Tick(context.Background())

	if len(reviewer.inputs) != 1 {
		t.Fatalf("reviewer ran %d times; want 1", len(reviewer.inputs))
	}
	if got := reviewer.inputs[0].PriorFindings; len(got) != 1 || got[0].ID != oneFinding.ID {
		t.Fatalf("PriorFindings = %+v; want the round-1 block's finding past the unavailable line", got)
	}
	if !strings.Contains(beadStore.issues[0].Notes, "LOCAL ROUND (3): head=1234567 at=") {
		t.Fatalf("round after an unavailable line is not numbered 3:\n%s", beadStore.issues[0].Notes)
	}
}

func TestLocalRoundPriorFindingsCarryRejectionAndLocalRound(t *testing.T) {
	t.Parallel()
	rejected := []land.Finding{
		{ID: "r1", Severity: "major", Path: "b.go", Line: 7, Title: "race"},
		{ID: "r2", Severity: "minor", Path: "c.go", Line: 9, Title: "typo"},
	}
	rejection := land.FormatRejectionNote(land.RejectionNote{
		Attempt: 1, Kind: string(land.RejectReview), Reason: "om requested changes",
		Branch: "polecat/x", Target: "main", MR: "gt-ryp1ff", Findings: rejected,
	})
	block := land.FormatLocalRound(1, "abcdef1", []land.Finding{oneFinding}, roundClock)
	const request = "\nLOCAL ROUND REQUEST head=1234567"
	cases := []struct {
		name  string
		notes string
		want  []string
	}{
		{name: "rejection then a local round", notes: rejection + "\n" + block + request, want: []string{"r1", "r2", oneFinding.ID}},
		{name: "rejection only", notes: rejection + request, want: []string{"r1", "r2"}},
		{name: "local round only", notes: block + request, want: []string{oneFinding.ID}},
		{name: "neither", notes: strings.TrimPrefix(request, "\n"), want: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			beadStore := &fakeRoundBeads{issues: []*beads.Issue{{ID: "gt-ryp1ff", Notes: tc.notes}}}
			reviewer := &fakeRoundReviewer{}
			p := newRoundPatrol(t, beadStore, reviewer)

			p.Tick(context.Background())

			if len(reviewer.inputs) != 1 {
				t.Fatalf("reviewer ran %d times; want 1", len(reviewer.inputs))
			}
			var got []string
			for _, f := range reviewer.inputs[0].PriorFindings {
				got = append(got, f.ID)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("PriorFindings = %v; want %v (rejection findings first, then the local round's)", got, tc.want)
			}
		})
	}
}

// fakeLocalRoundGit is the rig repository a round's fetch reads and writes.
// Each error field makes its call fail with that error.
type fakeLocalRoundGit struct {
	rev, base                          string
	revErr, updateErr, addErr, baseErr error
	removeErr                          error
	refs                               map[string]string
	removed                            []string
	// calls counts every git call, so a test can assert none was made.
	calls int
}

func (f *fakeLocalRoundGit) Rev(string) (string, error) {
	f.calls++
	return f.rev, f.revErr
}

func (f *fakeLocalRoundGit) UpdateRef(ref, commit string) error {
	f.calls++
	if f.updateErr != nil {
		return f.updateErr
	}
	f.refs[ref] = commit
	return nil
}

func (f *fakeLocalRoundGit) WorktreeAddDetached(string, string) error {
	f.calls++
	return f.addErr
}

func (f *fakeLocalRoundGit) WorktreeRemove(path string, _ bool) error {
	f.calls++
	f.removed = append(f.removed, path)
	return f.removeErr
}

func (f *fakeLocalRoundGit) MergeBase(string, string) (string, error) {
	f.calls++
	return f.base, f.baseErr
}

// gitOutput stands for what a failing git call carries; no error may echo it.
var gitOutput = errors.New("fatal: SECRET-OUTPUT")

func TestLocalRoundFetchServesTheHeadInAWorktree(t *testing.T) {
	t.Parallel()
	workRoot := t.TempDir()
	g := &fakeLocalRoundGit{rev: "abcdef1", base: "base1", refs: map[string]string{}}
	fetch := localRoundFetcher(g, nil, workRoot, "origin", "main", func(string, ...any) {})

	tree, err := fetch("gt-ryp1ff", "abcdef1")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got := g.refs["refs/local-round/gt-ryp1ff"]; got != "abcdef1" {
		t.Fatalf("private ref = %q; want the head", got)
	}
	if tree.Base != "base1" || !strings.HasPrefix(tree.Dir, workRoot) {
		t.Fatalf("tree = %+v; want the merge base and a worktree under the work root", tree)
	}
	tree.Cleanup()
	if len(g.removed) != 1 || g.removed[0] != tree.Dir {
		t.Fatalf("cleanup removed %v; want the worktree %s", g.removed, tree.Dir)
	}
}

func TestLocalRoundFetchFailuresRefuseTheRound(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		g           *fakeLocalRoundGit
		missingRoot bool
		wantCleanup bool
	}{
		{name: "head is no commit", g: &fakeLocalRoundGit{revErr: gitOutput}},
		{name: "private ref not written", g: &fakeLocalRoundGit{rev: "abcdef1", updateErr: gitOutput}},
		{name: "work root missing", g: &fakeLocalRoundGit{rev: "abcdef1"}, missingRoot: true},
		{name: "worktree add fails", g: &fakeLocalRoundGit{rev: "abcdef1", addErr: gitOutput}, wantCleanup: true},
		{name: "merge base fails", g: &fakeLocalRoundGit{rev: "abcdef1", baseErr: gitOutput}, wantCleanup: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.g.refs = map[string]string{}
			root := t.TempDir()
			if tc.missingRoot {
				root = filepath.Join(t.TempDir(), "absent")
			}
			fetch := localRoundFetcher(tc.g, nil, root, "origin", "main", func(string, ...any) {})

			_, err := fetch("gt-ryp1ff", "abcdef1")
			if err == nil {
				t.Fatal("fetch returned no error; the round must be refused")
			}
			if strings.Contains(err.Error(), "SECRET-OUTPUT") {
				t.Fatalf("the refusal echoes git's output: %v", err)
			}
			if got := len(tc.g.removed) == 1; got != tc.wantCleanup {
				t.Fatalf("worktree cleanup ran %d times; want it run: %v", len(tc.g.removed), tc.wantCleanup)
			}
		})
	}
}

func TestLocalRoundFetchCleanupLogsWithoutGitOutput(t *testing.T) {
	t.Parallel()
	g := &fakeLocalRoundGit{rev: "abcdef1", base: "base1", refs: map[string]string{}, removeErr: gitOutput}
	var logged []string
	logf := func(format string, args ...any) { logged = append(logged, format) }
	fetch := localRoundFetcher(g, nil, t.TempDir(), "origin", "main", logf)

	tree, err := fetch("gt-ryp1ff", "abcdef1")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	tree.Cleanup()
	if len(logged) != 1 {
		t.Fatalf("cleanup logged %d lines; want the one failed remove named", len(logged))
	}
	for _, line := range logged {
		if strings.Contains(line, "SECRET-OUTPUT") {
			t.Fatalf("cleanup logs git's output: %q", line)
		}
	}
}

func TestLocalRoundFetchRefusesABeadIdThatIsNoRefName(t *testing.T) {
	t.Parallel()
	// The id is written into refs/local-round/<bead>, so each of these would name
	// another ref or a path, and the 65-character id is past the ref bound.
	for _, bead := range []string{"..", "gt x", "gt-@{x", "-gt-abc", "gt-a/b", `gt-a\b`, "gt-abc..def", "gt-" + strings.Repeat("a", 62)} {
		t.Run(bead, func(t *testing.T) {
			t.Parallel()
			g := &fakeLocalRoundGit{rev: "abcdef1", base: "base1", refs: map[string]string{}}
			fetch := localRoundFetcher(g, nil, t.TempDir(), "origin", "main", func(string, ...any) {})

			_, err := fetch(bead, "abcdef1")
			if err == nil || !strings.Contains(err.Error(), "is not valid for a ref name") {
				t.Fatalf("fetch error = %v; want the bead id refused as no ref name", err)
			}
			if g.calls != 0 || len(g.refs) != 0 {
				t.Fatalf("the refused id made %d git calls and wrote %d refs; want neither", g.calls, len(g.refs))
			}
		})
	}
}

func TestLocalRoundFetchAcceptsTheBeadIdShapes(t *testing.T) {
	t.Parallel()
	for _, bead := range []string{"gt-ryp1ff", "gt-3lvcjz.24", "gt-wisp-abc"} {
		t.Run(bead, func(t *testing.T) {
			t.Parallel()
			g := &fakeLocalRoundGit{rev: "abcdef1", base: "base1", refs: map[string]string{}}
			fetch := localRoundFetcher(g, nil, t.TempDir(), "origin", "main", func(string, ...any) {})

			tree, err := fetch(bead, "abcdef1")
			if err != nil {
				t.Fatalf("fetch: %v", err)
			}
			tree.Cleanup()
			if got := g.refs["refs/local-round/"+bead]; got != "abcdef1" {
				t.Fatalf("private ref = %q; want the head", got)
			}
		})
	}
}

func TestLocalRoundCleanupRemovesOnlyItsOwnWorktree(t *testing.T) {
	t.Parallel()
	g := &fakeLocalRoundGit{rev: "abcdef1", base: "base1", refs: map[string]string{}}
	fetch := localRoundFetcher(g, nil, t.TempDir(), "origin", "main", func(string, ...any) {})

	first, err := fetch("gt-aaa", "abcdef1")
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	second, err := fetch("gt-bbb", "abcdef1")
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	if first.Dir == second.Dir {
		t.Fatalf("two rounds share the worktree %s", first.Dir)
	}

	first.Cleanup()
	if len(g.removed) != 1 || g.removed[0] != first.Dir {
		t.Fatalf("first cleanup removed %v; want exactly its own worktree %s", g.removed, first.Dir)
	}
	second.Cleanup()
	if len(g.removed) != 2 || g.removed[1] != second.Dir {
		t.Fatalf("second cleanup removed %v; want exactly its own worktree %s after the first", g.removed, second.Dir)
	}
}

func TestLocalRoundListsOpenAndInProgressOnly(t *testing.T) {
	t.Parallel()
	store := []*beads.Issue{
		{ID: "gt-open", Status: string(beads.StatusOpen)},
		{ID: "gt-busy", Status: string(beads.StatusInProgress)},
		{ID: "gt-done", Status: "closed"},
	}
	var asked []beads.IssueStatus
	// bd answers a status query with the beads of those statuses, as the real
	// lister does.
	bdList := func(statuses ...beads.IssueStatus) ([]*beads.Issue, error) {
		asked = statuses
		var out []*beads.Issue
		for _, issue := range store {
			if slices.Contains(statuses, beads.IssueStatus(issue.Status)) {
				out = append(out, issue)
			}
		}
		return out, nil
	}

	got, err := listLocalRoundRequests(bdList)()
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	var ids []string
	for _, issue := range got {
		ids = append(ids, issue.ID)
	}
	if !slices.Equal(ids, []string{"gt-open", "gt-busy"}) {
		t.Fatalf("listed %v; want the open and in_progress beads, never the closed one", ids)
	}
	if !slices.Equal(asked, []beads.IssueStatus{beads.StatusOpen, beads.StatusInProgress}) {
		t.Fatalf("asked bd for %v; want open and in_progress", asked)
	}
}

// newLocalRoundRigDaemon is a daemon whose git is a fake. With forgejo set, the
// rig's landing remote is the Forgejo one the fake lists. The work root is a
// fresh directory outside the town.
func newLocalRoundRigDaemon(t *testing.T, townRoot string, forgejo bool) *Daemon {
	t.Helper()
	d := &Daemon{
		logger:       discardLogger,
		config:       &Config{TownRoot: townRoot},
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{LandingWorker: &LandingWorkerConfig{WorkRoot: t.TempDir()}}},
	}
	f := useGitfake(t, d)
	if forgejo {
		forgejoLandingRemote(t, f, townRoot, "gastown")
	}
	return d
}

// localRoundRigKeyFile is the promote key path the rig config names.
const localRoundRigKeyFile = "promote-gastown.key"

// localRoundRigConfigBadBranch is a rig config whose Forgejo block still
// resolves, so the remote step passes, while default_branch cannot be read.
const localRoundRigConfigBadBranch = `{"type":"rig","version":1,"name":"gastown","default_branch":5,
	"merge_queue":{"forgejo":{"remote_url":"https://forgejo.example/gastown/gastown.git","gate_workflow":"gate",
	"bots":{"landing":"gt-landing"},"promote_target":"git@github.com:example/gastown.git","promote_key_file":"` + localRoundRigKeyFile + `"}}}`

func TestNewRigLocalRoundPatrolRefusesABrokenRig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		// build makes the town and the daemon for one broken rig.
		build func(t *testing.T, townRoot string) *Daemon
	}{
		{name: "rig repository missing", build: func(t *testing.T, townRoot string) *Daemon {
			return newLocalRoundRigDaemon(t, townRoot, false)
		}},
		{name: "remote matches no remote", build: func(t *testing.T, townRoot string) *Daemon {
			promoteRigConfig(t, townRoot, "gastown", localRoundRigKeyFile)
			return newLocalRoundRigDaemon(t, townRoot, false)
		}},
		{name: "default branch unreadable", build: func(t *testing.T, townRoot string) *Daemon {
			promoteRigConfig(t, townRoot, "gastown", localRoundRigKeyFile)
			d := newLocalRoundRigDaemon(t, townRoot, true)
			writeDaemonRigConfigFile(t, filepath.Join(townRoot, "gastown"), localRoundRigConfigBadBranch)
			return d
		}},
		{name: "work root relative", build: func(t *testing.T, townRoot string) *Daemon {
			promoteRigConfig(t, townRoot, "gastown", localRoundRigKeyFile)
			d := newLocalRoundRigDaemon(t, townRoot, true)
			d.patrolConfig.Patrols.LandingWorker.WorkRoot = "relative/work"
			return d
		}},
		{name: "work root under the town", build: func(t *testing.T, townRoot string) *Daemon {
			promoteRigConfig(t, townRoot, "gastown", localRoundRigKeyFile)
			d := newLocalRoundRigDaemon(t, townRoot, true)
			d.patrolConfig.Patrols.LandingWorker.WorkRoot = filepath.Join(townRoot, "work")
			return d
		}},
		{name: "work root is a file", build: func(t *testing.T, townRoot string) *Daemon {
			promoteRigConfig(t, townRoot, "gastown", localRoundRigKeyFile)
			d := newLocalRoundRigDaemon(t, townRoot, true)
			file := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			d.patrolConfig.Patrols.LandingWorker.WorkRoot = file
			return d
		}},
		{name: "om directory blocked", build: func(t *testing.T, townRoot string) *Daemon {
			promoteRigConfig(t, townRoot, "gastown", localRoundRigKeyFile)
			d := newLocalRoundRigDaemon(t, townRoot, true)
			if err := os.MkdirAll(filepath.Join(townRoot, ".runtime"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(townRoot, ".runtime", "local-round-om"), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			return d
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			d := tc.build(t, townRoot)

			if _, err := d.newRigLocalRoundPatrol("gastown"); err == nil {
				t.Fatal("a broken rig built a local round patrol; it must be refused")
			}
		})
	}
}

func TestNewRigLocalRoundPatrolWritesNoVerdicts(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	promoteRigConfig(t, townRoot, "gastown", localRoundRigKeyFile)
	d := newLocalRoundRigDaemon(t, townRoot, true)

	p, err := d.newRigLocalRoundPatrol("gastown")
	if err != nil {
		t.Fatalf("newRigLocalRoundPatrol: %v", err)
	}
	om, ok := p.Reviewer.(land.OMReviewer)
	if !ok {
		t.Fatalf("reviewer is %T; want the om reviewer", p.Reviewer)
	}
	if om.Verdicts != nil {
		t.Fatal("the local round's om reviewer keeps a verdict store; a round must write no verdict")
	}
}

func TestLocalRoundPausedWhileAHoldStands(t *testing.T) {
	t.Parallel()
	d := &Daemon{logger: discardLogger}
	if got := d.localRoundPaused(); got != "" {
		t.Fatalf("no hold: paused = %q; want not paused", got)
	}
	d.landingHolds.hold("rebuild_om")
	if got := d.localRoundPaused(); got == "" {
		t.Fatal("a hold stands: paused = \"\"; want the round paused")
	}
}

func TestStartLocalRoundPatrolsNeedsADaemonConfig(t *testing.T) {
	t.Parallel()
	d := &Daemon{logger: discardLogger}
	d.startLocalRoundPatrols()
}

// cancelOnEscalate records each escalation as the recorder does, then cancels
// the context: the start alert is the last thing a refused rig's first tick does,
// so the loop's next look at the context ends it.
type cancelOnEscalate struct {
	*notifyfake.Recorder
	cancel context.CancelFunc
}

func (c cancelOnEscalate) Escalate(ctx context.Context, e notify.Escalation) error {
	err := c.Recorder.Escalate(ctx, e)
	c.cancel()
	return err
}

// TestRunLocalRoundPatrolEndsWithTheDaemonWhileARigIsRefused bounds the loop: a
// refused rig keeps its patrol goroutine until the daemon's context ends, and
// that end raises the rig's start alert first. A loop that ignores the context
// fails at the 5 s bound rather than hanging the gate.
func TestRunLocalRoundPatrolEndsWithTheDaemonWhileARigIsRefused(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := notifyfake.New()
	d := &Daemon{
		logger:   discardLogger,
		config:   &Config{TownRoot: t.TempDir()},
		notifier: cancelOnEscalate{Recorder: rec, cancel: cancel},
		ctx:      ctx,
	}
	bound, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	done := make(chan struct{})
	go func() {
		d.runLocalRoundPatrol("gastown")
		close(done)
	}()

	select {
	case <-done:
	case <-bound.Done():
		t.Fatal("the patrol kept running after the daemon's context ended")
	}
	escalations := rec.Escalations()
	if len(escalations) != 1 || escalations[0].Escalation.Fingerprint != localRoundRigKey("gastown") {
		t.Fatalf("escalations = %+v; want one start alert under the rig's key", escalations)
	}
}

func TestLocalRoundStartFailureRaisesAnAlert(t *testing.T) {
	t.Parallel()
	rec := notifyfake.New()
	d := &Daemon{logger: discardLogger, config: &Config{TownRoot: t.TempDir()}, notifier: rec}
	beadStore := &fakeRoundBeads{issues: []*beads.Issue{{ID: "gt-ryp1ff", Notes: "LOCAL ROUND REQUEST head=abcdef1"}}}
	reviewer := &fakeRoundReviewer{findings: []land.Finding{oneFinding}}
	refused := errors.New("rig repository /town/a/.repo.git: no such file")
	repaired := false
	a := &localRoundRigPatrol{d: d, rig: "a", build: func(string) (*LocalRoundPatrol, error) {
		if !repaired {
			return nil, refused
		}
		return newRoundPatrol(t, beadStore, reviewer), nil
	}}
	// Rig b has its own bead, so only rig a's request is served by a's patrol.
	bStore := &fakeRoundBeads{issues: []*beads.Issue{{ID: "gt-b00000", Notes: "LOCAL ROUND REQUEST head=abcdef1"}}}
	b := &localRoundRigPatrol{d: d, rig: "b", build: func(string) (*LocalRoundPatrol, error) {
		return newRoundPatrol(t, bStore, reviewer), nil
	}}
	ctx := context.Background()

	a.tick(ctx)
	a.tick(ctx)
	b.tick(ctx)

	escalations := rec.Escalations()
	if len(escalations) != 1 {
		t.Fatalf("escalations = %d; want one start alert, for rig a only", len(escalations))
	}
	got := escalations[0].Escalation
	if got.Fingerprint != localRoundRigKey("a") {
		t.Fatalf("alert key = %q; want %q", got.Fingerprint, localRoundRigKey("a"))
	}
	if !strings.Contains(got.Reason, refused.Error()) {
		t.Fatalf("alert reason %q does not name the refusal %q", got.Reason, refused.Error())
	}
	if clears := rec.Clears(); len(clears) != 0 {
		t.Fatalf("clears = %+v before a build landed; want none", clears)
	}

	repaired = true
	a.tick(ctx)

	clears := rec.Clears()
	if len(clears) != 1 || !slices.Equal(clears[0].Fingerprints, []string{localRoundRigKey("a")}) {
		t.Fatalf("clears = %+v; want one clear of rig a's start alert once its build landed", clears)
	}
	if !strings.Contains(beadStore.issues[0].Notes, "LOCAL ROUND (1): head=abcdef1 at=") {
		t.Fatalf("the rebuilt patrol did not serve the request; notes:\n%s", beadStore.issues[0].Notes)
	}
}

// failFirstEscalates refuses its first fail sends and records the rest, as a
// notifier that is down for a while and then answers does.
type failFirstEscalates struct {
	*notifyfake.Recorder
	fail  int
	calls int
}

func (f *failFirstEscalates) Escalate(ctx context.Context, e notify.Escalation) error {
	f.calls++
	if f.calls <= f.fail {
		return errors.New("notifier down")
	}
	return f.Recorder.Escalate(ctx, e)
}

// The alarm's own failure path: a start alert whose send fails is not taken for
// an open one. The next tick, which is refused again, sends it again; once it has
// landed no tick sends a second one; and the later build that lands clears it
// (gt-ryp1ff).
func TestLocalRoundStartAlertSendFailureIsRetriedNextTick(t *testing.T) {
	t.Parallel()
	clk := &diskMaintNoSleepClock{FakeClock: newFixedClock()}
	rec := notifyfake.New()
	// One tick's send makes maxEscalationRetries attempts: refuse all of them.
	notifier := &failFirstEscalates{Recorder: rec, fail: maxEscalationRetries}
	d := &Daemon{logger: discardLogger, config: &Config{TownRoot: t.TempDir()}, notifier: notifier, clock: clk}
	refused := errors.New("rig repository /town/a/.repo.git: no such file")
	repaired := false
	beadStore := &fakeRoundBeads{}
	reviewer := &fakeRoundReviewer{findings: []land.Finding{oneFinding}}
	a := &localRoundRigPatrol{d: d, rig: "a", build: func(string) (*LocalRoundPatrol, error) {
		if !repaired {
			return nil, refused
		}
		return newRoundPatrol(t, beadStore, reviewer), nil
	}}
	ctx := context.Background()

	a.tick(ctx)
	if got := rec.Escalations(); len(got) != 0 {
		t.Fatalf("escalations after a refused send = %+v; want none recorded", got)
	}
	if a.alertOpen {
		t.Fatal("the alert is marked open though its send failed: the next tick would never send it")
	}

	a.tick(ctx)
	escalations := rec.Escalations()
	if len(escalations) != 1 || escalations[0].Escalation.Fingerprint != localRoundRigKey("a") {
		t.Fatalf("escalations after the retry = %+v; want one start alert under rig a's key", escalations)
	}

	a.tick(ctx)
	if got := rec.Escalations(); len(got) != 1 {
		t.Fatalf("escalations after the alert landed = %d; want it sent once, not again each tick", len(got))
	}

	repaired = true
	a.tick(ctx)
	clears := rec.Clears()
	if len(clears) != 1 || !slices.Equal(clears[0].Fingerprints, []string{localRoundRigKey("a")}) {
		t.Fatalf("clears = %+v; want one clear of rig a's start alert once its build landed", clears)
	}
}

// allowedPatrolFields are every dependency the patrol holds. A new one fails
// TestLocalRoundNeverTouchesVerdictStores until someone decides it is not a
// verdict store, the review log or the rounds ledger.
var allowedPatrolFields = []string{"ListRequests", "Comments", "AppendNote", "Fetch", "Reviewer", "Bound", "Config", "Operator", "Paused", "Now", "Logf"}

func TestLocalRoundNeverTouchesVerdictStores(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeOf(LocalRoundPatrol{})
	for i := range typ.NumField() {
		if name := typ.Field(i).Name; !slices.Contains(allowedPatrolFields, name) {
			t.Fatalf("LocalRoundPatrol.%s is not an allowed dependency; decide whether it is a verdict store, review log or rounds ledger first", name)
		}
	}
}

// roundTreeState maps each file under root to its size and mtime, so a test
// can tell what a tick wrote.
func roundTreeState(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[rel] = fmt.Sprintf("%d %d", info.Size(), info.ModTime().UnixNano())
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return files
}

func TestLocalRoundTickWritesNoVerdictOrLedger(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	promoteRigConfig(t, townRoot, "gastown", localRoundRigKeyFile)
	d := newLocalRoundRigDaemon(t, townRoot, true)
	d.patrolConfig.LocalRoundSetting = &config.LocalRoundConfig{Enabled: true}
	p, err := d.newRigLocalRoundPatrol("gastown")
	if err != nil {
		t.Fatalf("newRigLocalRoundPatrol: %v", err)
	}
	// The bead reads and writes, the fetch and the reviewer are fakes. The fetch
	// writes the rig's git refs, which this check does not cover. The bound,
	// operator and pause reads are the production ones over the town.
	beadStore := &fakeRoundBeads{issues: []*beads.Issue{{ID: "gt-ryp1ff", Notes: "LOCAL ROUND REQUEST head=abcdef1"}}}
	p.ListRequests = beadStore.list
	p.Comments = beadStore.commentsOf
	p.AppendNote = beadStore.appendNote
	p.Fetch = func(string, string) (localRoundTree, error) {
		return localRoundTree{Dir: t.TempDir(), Base: "base", Cleanup: func() {}}, nil
	}
	p.Reviewer = &fakeRoundReviewer{findings: []land.Finding{oneFinding}}
	before := roundTreeState(t, townRoot)

	p.Tick(context.Background())

	if !strings.Contains(beadStore.issues[0].Notes, "LOCAL ROUND (1): head=abcdef1 at=") {
		t.Fatalf("the tick did not serve the request with a block; notes:\n%s", beadStore.issues[0].Notes)
	}
	// The bound is the one file a round may write under the town.
	boundDir := filepath.Join(".runtime", "local-round-bound")
	after := roundTreeState(t, townRoot)
	for rel, state := range after {
		if strings.HasPrefix(rel, boundDir) {
			continue
		}
		if before[rel] != state {
			t.Fatalf("the tick wrote %s; a round writes no verdict, review log or rounds ledger", rel)
		}
	}
	for rel := range before {
		if _, ok := after[rel]; !ok {
			t.Fatalf("the tick removed %s", rel)
		}
	}
}

func TestRunStartsLocalRoundPatrolsThroughItsSeam(t *testing.T) {
	t.Parallel()
	d := &Daemon{logger: discardLogger}
	var started []*Daemon
	d.seams.startLocalRoundPatrols = func(got *Daemon) { started = append(started, got) }

	d.seams.runStartLocalRoundPatrols(d)

	if len(started) != 1 || started[0] != d {
		t.Fatalf("the seam ran %d times; want once with the daemon", len(started))
	}
}
