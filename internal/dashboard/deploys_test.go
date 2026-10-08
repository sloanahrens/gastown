package dashboard

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// deployNow is the instant the deploy tests read at: an hour past the base
// instant the shared stamp() builds from, so a run created at stamp(n) is
// deployNow's minute minus n old.
var deployNow = time.Date(2026, 10, 5, 11, 0, 0, 0, time.UTC)

// deployStub is a stand-in Forgejo that answers the three calls the deploy
// reader makes, recording what it was asked for.
type deployStub struct {
	repos    []string
	reposErr error
	runs     map[string][]forgejo.ActionRun
	runsErr  map[string]error
	jobs     map[int64][]forgejo.ActionRunJob
	jobsErr  map[int64]error

	runsAsked []string
	jobsAsked []int64
}

func (s *deployStub) ListUserRepos(context.Context) ([]forgejo.Repository, error) {
	if s.reposErr != nil {
		return nil, s.reposErr
	}
	out := make([]forgejo.Repository, 0, len(s.repos))
	for _, name := range s.repos {
		out = append(out, forgejo.Repository{FullName: name})
	}
	return out, nil
}

func (s *deployStub) ListRuns(_ context.Context, owner, repo string, _ forgejo.RunFilter) (*forgejo.RunList, error) {
	name := owner + "/" + repo
	s.runsAsked = append(s.runsAsked, name)
	if err := s.runsErr[name]; err != nil {
		return nil, err
	}
	return &forgejo.RunList{Runs: s.runs[name]}, nil
}

func (s *deployStub) ListRunJobs(_ context.Context, _, _ string, runID int64) ([]forgejo.ActionRunJob, error) {
	s.jobsAsked = append(s.jobsAsked, runID)
	if err := s.jobsErr[runID]; err != nil {
		return nil, err
	}
	return s.jobs[runID], nil
}

// newDeployTestReader wires a reader to a stub with the tests' clock.
func newDeployTestReader(stub *deployStub, repos []string) *DeployReader {
	r := NewDeployReader(stub, repos)
	r.now = func() time.Time { return deployNow }
	return r
}

// testDeployRun builds a run of the deploy workflow: the fields the block
// draws, plus the workflow id it is picked out by.
func testDeployRun(id int64, ref, status string, created int) forgejo.ActionRun {
	return forgejo.ActionRun{
		ID:         id,
		WorkflowID: ".forgejo/workflows/deploy.yml",
		PrettyRef:  ref,
		CommitSHA:  "645edaf6a1b2c3d4e5f60718293a4b5c6d7e8f90",
		Status:     status,
		Event:      "push",
		Created:    stamp(created),
		HTMLURL:    "https://forgejo.test/runs/" + strconv.FormatInt(id, 10),
	}
}

// testJob is one job of a run, with the needs that order it.
func testJob(name, status string, needs ...string) forgejo.ActionRunJob {
	return forgejo.ActionRunJob{Name: name, Status: status, Needs: needs}
}

// stageNames is the row's stages as the page reads them.
func stageNames(stages []DeployStage) []string {
	out := make([]string, 0, len(stages))
	for _, s := range stages {
		out = append(out, s.Name+" "+s.Status)
	}
	return out
}

// A deploy run reaches the page as one row: its repo, ref, short commit, state
// and age, with the jobs as stages in the order their needs give — the file's
// jobs are not in that order in the API, and the operator reads the release
// left to right.
func TestDeployReaderDrawsARunAsStagesInNeedsOrder(t *testing.T) {
	t.Parallel()

	stub := &deployStub{
		repos: []string{"sloan/fractals-nextjs"},
		runs: map[string][]forgejo.ActionRun{
			"sloan/fractals-nextjs": {testDeployRun(7, "v0.1.0", "success", 30)},
		},
		jobs: map[int64][]forgejo.ActionRunJob{
			7: {
				testJob("prod", "success", "staging"),
				testJob("build", "success"),
				testJob("staging", "success", "staging-preview"),
				testJob("staging-preview", "success", "build"),
			},
		},
	}
	d := newDeployTestReader(stub, nil).Read()

	require.Empty(t, d.Error)
	require.Empty(t, d.Missing)
	require.Len(t, d.Runs, 1)
	row := d.Runs[0]
	assert.Equal(t, "sloan/fractals-nextjs", row.Repo)
	assert.Equal(t, "v0.1.0", row.Ref, "the ref is a tag like v0.1.0 or a branch")
	assert.Equal(t, "645edaf6", row.Hash, "the commit is cut to the eight characters the Actions page shows")
	assert.Equal(t, "success", row.Status)
	assert.Equal(t, "https://forgejo.test/runs/7", row.URL)
	assert.True(t, row.At.Equal(time.Date(2026, 10, 5, 10, 30, 0, 0, time.UTC)), "the row carries the run's own creation time")
	assert.False(t, row.StagesUnread)
	assert.Empty(t, row.Warn)
	assert.Equal(t, []string{
		"build success", "staging-preview success", "staging success", "prod success",
	}, stageNames(row.Stages), "the needs order, not the API's")
}

// A stage that failed is drawn as it is, and the stages it held up are drawn
// as the API reports them — blocked, which is a stage that did not run.
func TestDeployReaderDrawsAFailedStageAndTheStagesItHeldUp(t *testing.T) {
	t.Parallel()

	stub := &deployStub{
		repos: []string{"sloan/fractals-nextjs"},
		runs: map[string][]forgejo.ActionRun{
			"sloan/fractals-nextjs": {testDeployRun(7, "v0.1.0", "failure", 30)},
		},
		jobs: map[int64][]forgejo.ActionRunJob{
			7: {
				testJob("build", "success"),
				testJob("staging-preview", "failure", "build"),
				testJob("staging", "blocked", "staging-preview"),
				testJob("prod", "blocked", "staging"),
			},
		},
	}
	d := newDeployTestReader(stub, nil).Read()

	require.Len(t, d.Runs, 1)
	assert.Equal(t, []string{
		"build success", "staging-preview failure", "staging blocked", "prod blocked",
	}, stageNames(d.Runs[0].Stages))
}

// A deploy run nothing has picked up is the one runner reading the block can
// make: the runners endpoint is owner-only, so a run still waiting well past
// its creation says so in the page's warning style, and one waiting only a
// little does not.
func TestDeployReaderSaysNoRunnerPickedUpARunThatWaited(t *testing.T) {
	t.Parallel()

	stub := &deployStub{
		repos: []string{"sloan/fractals-nextjs"},
		runs: map[string][]forgejo.ActionRun{
			// Six minutes waiting, against the five-minute threshold.
			"sloan/fractals-nextjs": {testDeployRun(7, "v0.1.0", "waiting", 54)},
		},
		jobs: map[int64][]forgejo.ActionRunJob{7: {testJob("build", "waiting")}},
	}
	d := newDeployTestReader(stub, nil).Read()

	require.Len(t, d.Runs, 1)
	assert.Equal(t, "no runner picked this up for 6 min", d.Runs[0].Warn)
	assert.Equal(t, []string{"build waiting"}, stageNames(d.Runs[0].Stages))

	// Exactly at the threshold is not past it.
	stub.runs["sloan/fractals-nextjs"] = []forgejo.ActionRun{testDeployRun(8, "v0.1.0", "waiting", 55)}
	d = newDeployTestReader(stub, nil).Read()
	require.Len(t, d.Runs, 1)
	assert.Empty(t, d.Runs[0].Warn, "five minutes of waiting is not yet trouble")
}

// A running run holding the same stages is stuck, and the reader is the only
// thing that can say so: no stamp the API sends carries the moment a stage last
// changed, so it compares each read against the one before it.
func TestDeployReaderSaysARunningRunThatHasNotMoved(t *testing.T) {
	t.Parallel()

	stub := &deployStub{
		repos: []string{"sloan/fractals-nextjs"},
		runs: map[string][]forgejo.ActionRun{
			"sloan/fractals-nextjs": {testDeployRun(7, "v0.1.0", "running", 40)},
		},
		jobs: map[int64][]forgejo.ActionRunJob{
			7: {testJob("build", "running"), testJob("staging-preview", "waiting", "build")},
		},
	}
	reader := newDeployTestReader(stub, nil)
	at := time.Date(2026, 10, 5, 11, 0, 0, 0, time.UTC)

	readAt := func(d time.Duration) *DeployRun {
		reader.now = func() time.Time { return at.Add(d) }
		dpl := reader.Read()
		require.Len(t, dpl.Runs, 1)
		return &dpl.Runs[0]
	}
	assert.Empty(t, readAt(0).Warn, "the first read has nothing to compare against")
	assert.Empty(t, readAt(29*time.Minute).Warn, "29 minutes of the same stages is not yet stuck")
	assert.Equal(t, deployStuck, readAt(30*time.Minute).Warn, "30 minutes with no stage change is")

	// A stage moves, so the run is moving again: the clock starts over.
	stub.jobs[7] = []forgejo.ActionRunJob{testJob("build", "success"), testJob("staging-preview", "running", "build")}
	assert.Empty(t, readAt(31*time.Minute).Warn, "a stage changed, so the run is not stuck")
	assert.Empty(t, readAt(59*time.Minute).Warn, "and the new stages are only 28 minutes old")
	assert.Equal(t, deployStuck, readAt(61*time.Minute).Warn)
}

// The two inferences are the block's whole judgement about a runner it cannot
// see, so their thresholds are pinned here: a change to either is a change to
// what the panel claims, and should be deliberate.
func TestDeployThresholds(t *testing.T) {
	t.Parallel()

	assert.Equal(t, 5*time.Minute, deployNoRunnerAfter, "a run waiting longer than this says nothing picked it up")
	assert.Equal(t, 30*time.Minute, deployStuckAfter, "a run holding the same stages this long reads as stuck")
	assert.Equal(t, 5, deployRunsKept, "runs listed per repo")
	assert.Equal(t, 3, deployJobsKept, "runs whose jobs one refresh fetches, across every repo")
	assert.Equal(t, 40, deployTextMax, "runes of a stage name or a ref the page shows")
}

// A repo with no deploy run is a repo with no deploy run: the block says so
// rather than failing, and a run of another workflow is not a deploy.
func TestDeployReaderWithNoDeployRuns(t *testing.T) {
	t.Parallel()

	gate := testDeployRun(1, "main", "success", 30)
	gate.WorkflowID = ".forgejo/workflows/gate.yml"
	nameless := testDeployRun(2, "main", "success", 20)
	nameless.WorkflowID = ""

	stub := &deployStub{
		repos: []string{"sloan/fractals-nextjs", "sloan/beads"},
		runs: map[string][]forgejo.ActionRun{
			"sloan/fractals-nextjs": {gate, nameless},
		},
	}
	d := newDeployTestReader(stub, nil).Read()

	assert.Empty(t, d.Error)
	assert.Empty(t, d.Missing)
	assert.Empty(t, d.Runs)
	assert.NotNil(t, d.Runs, "an empty list, never nil: the page draws its own line for it")
	assert.Equal(t, []string{"sloan/fractals-nextjs", "sloan/beads"}, stub.runsAsked, "both repos were asked")
}

// A viewer that cannot answer leaves the block saying so, and the Cloud
// section's own findings are not part of this reader at all.
func TestDeployReaderNamesAViewerThatCannotAnswer(t *testing.T) {
	t.Parallel()

	stub := &deployStub{reposErr: &forgejo.APIError{StatusCode: http.StatusInternalServerError}}
	d := newDeployTestReader(stub, nil).Read()
	assert.Equal(t, "http 500", d.Error)
	assert.Empty(t, d.Runs)
	assert.Empty(t, d.Missing)
}

// Every named repo failing is the viewer not answering: there is no fresh
// value to show, so the note carries the failure rather than an empty list that
// reads as a town with no deploys.
func TestDeployReaderNamesEveryRepoItCouldNotRead(t *testing.T) {
	t.Parallel()

	stub := &deployStub{
		reposErr: io.EOF, // the explicit list skips ListUserRepos
		runsErr: map[string]error{
			"sloan/beads":           &forgejo.APIError{StatusCode: http.StatusForbidden},
			"sloan/fractals-nextjs": &forgejo.APIError{StatusCode: http.StatusNotFound},
		},
	}
	d := newDeployTestReader(stub, []string{"sloan/beads", "sloan/fractals-nextjs"}).Read()
	assert.NotEmpty(t, d.Error, "no repo answered, so the note names the failure")
	assert.Empty(t, d.Runs)
}

// One repo the viewer cannot read costs the block that repo and nothing else,
// and the repo is named rather than reading as a repo with no deploys.
func TestDeployReaderNamesTheRepoItCouldNotRead(t *testing.T) {
	t.Parallel()

	stub := &deployStub{
		runs: map[string][]forgejo.ActionRun{
			"sloan/beads": {testDeployRun(1, "v0.2.0", "success", 10)},
		},
		runsErr: map[string]error{
			"sloan/fractals-nextjs": &forgejo.APIError{StatusCode: http.StatusNotFound},
		},
	}
	d := newDeployTestReader(stub, []string{"sloan/beads", "sloan/fractals-nextjs"}).Read()

	assert.Empty(t, d.Error, "one readable repo is an answer")
	assert.Equal(t, []string{"sloan/fractals-nextjs"}, d.Missing)
	require.Len(t, d.Runs, 1)
	assert.Equal(t, "sloan/beads", d.Runs[0].Repo)
}

// A stage name and a ref are a repository's own text: the characters that carry
// no visible glyph are gone and a long one is capped, so neither can reorder
// what the operator reads or spend the cell.
func TestDeployReaderStripsAndCapsHostileStageNames(t *testing.T) {
	t.Parallel()

	const long = "staging-preview-that-goes-on-and-on-and-on-and-does-not-stop"
	run := testDeployRun(7, "v0.1.0‮\u0007", "success", 30)
	stub := &deployStub{
		repos: []string{"sloan/fractals-nextjs"},
		runs:  map[string][]forgejo.ActionRun{"sloan/fractals-nextjs": {run}},
		jobs: map[int64][]forgejo.ActionRunJob{
			7: {testJob("build\u001b[31m\non two lines", "success"), testJob(long, "success", "build\u001b[31m\non two lines")},
		},
	}
	d := newDeployTestReader(stub, nil).Read()

	require.Len(t, d.Runs, 1)
	require.Len(t, d.Runs[0].Stages, 2)
	for _, s := range d.Runs[0].Stages {
		assert.LessOrEqual(t, len([]rune(s.Name)), deployTextMax+1, "a name is capped, with the cut marked")
		assert.Equal(t, strings.TrimSpace(s.Name), s.Name)
		for _, r := range s.Name {
			assert.False(t, unicode.IsControl(r) || unicode.Is(unicode.Cf, r), "no invisible glyph survives %q", s.Name)
		}
	}
	assert.Equal(t, "build[31m on two lines", d.Runs[0].Stages[0].Name, "a control character is dropped, a newline becomes a space")
	assert.True(t, strings.HasSuffix(d.Runs[0].Stages[1].Name, "…"), "a capped name says it was cut: %q", d.Runs[0].Stages[1].Name)
	assert.Equal(t, "v0.1.0", d.Runs[0].Ref, "the ref is stripped too")
}

// Runs cost one call per repo and jobs cost one each, so the jobs are capped:
// the newest runs get them and the rest carry their ref, commit, state and age
// with the page saying their stages were not read.
func TestDeployReaderFetchesJobsForTheNewestRunsOnly(t *testing.T) {
	t.Parallel()

	var runs []forgejo.ActionRun
	for i := 0; i < 8; i++ {
		runs = append(runs, testDeployRun(int64(i+1), "v0."+strconv.Itoa(i), "success", i))
	}
	stub := &deployStub{
		repos: []string{"sloan/fractals-nextjs"},
		runs:  map[string][]forgejo.ActionRun{"sloan/fractals-nextjs": runs},
	}
	d := newDeployTestReader(stub, nil).Read()

	require.Len(t, d.Runs, deployRunsKept, "the newest five of the repo's runs, and no more")
	assert.Equal(t, []string{"v0.7", "v0.6", "v0.5", "v0.4", "v0.3"}, refsOf(d.Runs), "newest first")
	assert.Equal(t, []int64{8, 7, 6}, stub.jobsAsked, "jobs for the newest three, across every repo")
	for i, row := range d.Runs {
		assert.Equal(t, i >= deployJobsKept, row.StagesUnread, "a run past the cap says its stages were not read")
	}

	// A jobs call that fails leaves that run saying the same, rather than an
	// empty stage list the page would draw as a run with no stages.
	stub.jobsErr = map[int64]error{8: io.EOF}
	d = newDeployTestReader(stub, nil).Read()
	require.Len(t, d.Runs, deployRunsKept)
	assert.True(t, d.Runs[0].StagesUnread)
}

// refsOf names the rows by ref, which is what the tests address them by.
func refsOf(rows []DeployRun) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Ref)
	}
	return out
}

// The block follows the deploy workflow, whatever event produced the run: a tag
// push, a manual run and a teardown run are all deploys, and they are picked
// out by the workflow the run names rather than by the event.
func TestDeployReaderTakesTheRunsOfTheDeployWorkflowWhateverTheirEvent(t *testing.T) {
	t.Parallel()

	tagPush := testDeployRun(1, "v0.1.0", "success", 30)
	manual := testDeployRun(2, "main", "running", 20)
	manual.Event = "workflow_dispatch"
	teardown := testDeployRun(3, "main", "success", 10)
	teardown.Event = "delete"
	// The same workflow named without its directory, and one that merely ends
	// in the string, are both what the file's base decides.
	bare := testDeployRun(4, "main", "success", 5)
	bare.WorkflowID = "deploy.yml"
	impostor := testDeployRun(5, "main", "success", 4)
	impostor.WorkflowID = ".forgejo/workflows/not-deploy.yml"

	stub := &deployStub{
		repos: []string{"sloan/fractals-nextjs"},
		runs:  map[string][]forgejo.ActionRun{"sloan/fractals-nextjs": {tagPush, manual, teardown, bare, impostor}},
	}
	d := newDeployTestReader(stub, nil).Read()

	require.Len(t, d.Runs, 4)
	assert.Equal(t, []string{"v0.1.0", "main", "main", "main"}, refsOf(d.Runs), "newest first, and the impostor is not a deploy")
}

// deployURL is the link the row gets: an http(s) one, and nothing for a URL the
// API did not mean as a place to send the operator.
func TestDeployURLLinksOnlyHTTP(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, in, want string }{
		{"https is kept", "https://forgejo.test/runs/7", "https://forgejo.test/runs/7"},
		{"http is kept", "http://forgejo.test/runs/7", "http://forgejo.test/runs/7"},
		{"a script URL is not a link target", "javascript:alert(1)", ""},
		{"another scheme is not one either", "ftp://forgejo.test/x", ""},
		{"no URL stays none", "", ""},
		{"a path is not a URL", "/runs/7", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, deployURL(tc.in))
		})
	}
}

// The stage order is the workflow's needs read out: a job after every job it
// needs, the API's order kept among jobs that do not order each other, and a
// need the API did not send a job for cannot hold anything up.
func TestOrderJobs(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   []forgejo.ActionRunJob
		want []string
	}{
		{
			"a chain given backwards",
			[]forgejo.ActionRunJob{
				testJob("prod", "waiting", "staging"),
				testJob("staging", "waiting", "staging-preview"),
				testJob("staging-preview", "waiting", "build"),
				testJob("build", "running"),
			},
			[]string{"build", "staging-preview", "staging", "prod"},
		},
		{
			"jobs that do not order each other keep the API's order",
			[]forgejo.ActionRunJob{testJob("test", "waiting"), testJob("lint", "waiting")},
			[]string{"test", "lint"},
		},
		{
			"a need with no job behind it cannot hold one up",
			[]forgejo.ActionRunJob{testJob("deploy", "waiting", "gone")},
			[]string{"deploy"},
		},
		{
			"no jobs",
			nil,
			nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got []string
			for _, j := range orderJobs(tc.in) {
				got = append(got, j.Name)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// A needs cycle is not something a workflow file can express, so it cannot be
// the reason a read hangs: the walk ends on it and keeps the API's order.
func TestOrderJobsEndsOnACycle(t *testing.T) {
	t.Parallel()

	got := orderJobs([]forgejo.ActionRunJob{
		testJob("a", "waiting", "b"),
		testJob("b", "waiting", "a"),
	})
	var names []string
	for _, j := range got {
		names = append(names, j.Name)
	}
	assert.Equal(t, []string{"a", "b"}, names)
}
