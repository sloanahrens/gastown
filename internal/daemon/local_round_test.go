package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/land"
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

// fakeLocalRoundGit is the rig repository a round's fetch reads and writes.
// Each error field makes its call fail with that error.
type fakeLocalRoundGit struct {
	rev, base                          string
	revErr, updateErr, addErr, baseErr error
	removeErr                          error
	refs                               map[string]string
	removed                            []string
}

func (f *fakeLocalRoundGit) Rev(string) (string, error) { return f.rev, f.revErr }

func (f *fakeLocalRoundGit) UpdateRef(ref, commit string) error {
	if f.updateErr != nil {
		return f.updateErr
	}
	f.refs[ref] = commit
	return nil
}

func (f *fakeLocalRoundGit) WorktreeAddDetached(string, string) error { return f.addErr }

func (f *fakeLocalRoundGit) WorktreeRemove(path string, _ bool) error {
	f.removed = append(f.removed, path)
	return f.removeErr
}

func (f *fakeLocalRoundGit) WorktreePrune() error { return f.removeErr }

func (f *fakeLocalRoundGit) MergeBase(string, string) (string, error) { return f.base, f.baseErr }

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
	if len(logged) != 2 {
		t.Fatalf("cleanup logged %d lines; want the remove and the prune named", len(logged))
	}
	for _, line := range logged {
		if strings.Contains(line, "SECRET-OUTPUT") {
			t.Fatalf("cleanup logs git's output: %q", line)
		}
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

func TestRunLocalRoundPatrolStopsWhenTheRigIsRefused(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	d := newLocalRoundRigDaemon(t, townRoot, false)
	d.ctx = context.Background()

	d.runLocalRoundPatrol("gastown")
}

func TestLocalRoundNeverTouchesVerdictStores(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeOf(LocalRoundPatrol{})
	for i := range typ.NumField() {
		field := typ.Field(i)
		name := field.Type.String()
		for _, store := range []string{"Verdict", "RoundsLedger", "ReviewLog", "Landings"} {
			if strings.Contains(name, store) {
				t.Fatalf("LocalRoundPatrol.%s has type %s; the patrol must not depend on a %s", field.Name, name, store)
			}
		}
	}
}
