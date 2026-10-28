package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path"
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
	repos     []string
	reposErr  error
	runs      map[string][]forgejo.ActionRun
	runsErr   map[string]error
	healthErr map[string]error // only the health read fails: the repo's deploy runs answered
	jobs      map[int64][]forgejo.ActionRunJob
	jobsErr   map[int64]error

	// runsAsked is the repo and workflow each deploy query named, healthAsked
	// the repos asked for the health workflow's runs, and jobsAsked the runs
	// asked for their jobs.
	runsAsked   []string
	healthAsked []string
	jobsAsked   []int64

	// healthDeadline and jobsDeadline are the deadlines the checks' read and the
	// deploy runs' jobs were called under: the checks run last on a budget of
	// their own (gt-t9w60).
	healthDeadline time.Time
	jobsDeadline   time.Time

	// spendTimeout makes a deploy query wait out the refresh's deadline before it
	// answers, and healthCtxErr is the error the checks' read began under.
	spendTimeout bool
	healthCtxErr error
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

func (s *deployStub) ListRuns(ctx context.Context, owner, repo string, f forgejo.RunFilter) (*forgejo.RunList, error) {
	name := owner + "/" + repo
	if f.WorkflowID == healthWorkflow {
		s.healthAsked = append(s.healthAsked, name)
		s.healthDeadline, _ = ctx.Deadline()
		s.healthCtxErr = ctx.Err()
		if s.healthCtxErr != nil {
			return nil, s.healthCtxErr
		}
	} else {
		s.runsAsked = append(s.runsAsked, name+" "+f.WorkflowID)
		if s.spendTimeout {
			<-ctx.Done()
		}
	}
	if err := s.runsErr[name]; err != nil {
		return nil, err
	}
	if f.WorkflowID == healthWorkflow {
		if err := s.healthErr[name]; err != nil {
			return nil, err
		}
	}
	return &forgejo.RunList{Runs: s.workflowRuns(name, f.WorkflowID)}, nil
}

// workflowRuns answers the way the instance does: the repo's runs, narrowed to
// the workflow the query names when it names one.
func (s *deployStub) workflowRuns(name, workflow string) []forgejo.ActionRun {
	runs := s.runs[name]
	if workflow == "" {
		return runs
	}
	out := make([]forgejo.ActionRun, 0, len(runs))
	for _, run := range runs {
		if path.Base(run.WorkflowID) == workflow {
			out = append(out, run)
		}
	}
	return out
}

func (s *deployStub) ListRunJobs(ctx context.Context, _, _ string, runID int64) ([]forgejo.ActionRunJob, error) {
	s.jobsAsked = append(s.jobsAsked, runID)
	if s.jobsDeadline.IsZero() {
		// The deploys' own jobs are the first the refresh asks for.
		s.jobsDeadline, _ = ctx.Deadline()
	}
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

// testStagingRun builds a run of the staging workflow: the landing that
// deploys main to staging after every merge, whose ref is the branch it
// deployed where a release's is a tag.
func testStagingRun(id int64, ref, status string, created int) forgejo.ActionRun {
	run := testDeployRun(id, ref, status, created)
	run.WorkflowID = ".forgejo/workflows/staging.yml"
	return run
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
	assert.False(t, row.StagesSkipped, "a run inside the cap had its jobs asked for")
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
	assert.Equal(t, 24, healthRunWindow, "health runs a row is counted over, and the jobs calls one repo costs: a day of the hourly check")
}

// A landing's staging deploy is a deploy the operator has to see: the block
// follows staging.yml beside deploy.yml, names each row's workflow, and orders
// the two together, so a landing that moved staging and a release that shipped
// read as the one list of deploys they are (gt-2h2lx).
func TestDeployReaderTakesTheRunsOfBothWorkflows(t *testing.T) {
	t.Parallel()

	staging := testStagingRun(2, "main", "running", 55)
	release := testDeployRun(1, "v0.1.0", "success", 30)

	stub := &deployStub{
		repos: []string{"sloan/fractals-nextjs"},
		runs:  map[string][]forgejo.ActionRun{"sloan/fractals-nextjs": {release, staging}},
		jobs: map[int64][]forgejo.ActionRunJob{
			1: {testJob("build", "success"), testJob("staging-preview", "success", "build"), testJob("prod", "success", "staging-preview")},
			2: {testJob("build", "success"), testJob("staging", "running", "build")},
		},
	}
	d := newDeployTestReader(stub, nil).Read()

	require.Empty(t, d.Error)
	require.Len(t, d.Runs, 2)
	assert.Equal(t, []string{"main", "v0.1.0"}, refsOf(d.Runs), "newest first, across both workflows")
	assert.Equal(t, []string{"staging.yml", "deploy.yml"}, workflowsOf(d.Runs), "the row names the workflow it came from")
	assert.Equal(t, []string{"build success", "staging running"}, stageNames(d.Runs[0].Stages), "the staging run's own jobs")
	assert.Equal(t, []string{"build success", "staging-preview success", "prod success"}, stageNames(d.Runs[1].Stages), "and the release's")
	assert.Equal(t, []int64{2, 1}, stub.jobsAsked)
}

// The runner-trouble inference reads the run, not the workflow it came from: a
// staging run nothing has picked up says so on the clock a release's does, so
// a landing whose staging deploy never starts reaches the operator the same
// way (gt-2h2lx).
func TestDeployReaderSaysNoRunnerPickedUpAStagingRunThatWaited(t *testing.T) {
	t.Parallel()

	stub := &deployStub{
		repos: []string{"sloan/fractals-nextjs"},
		// Six minutes waiting, against the five-minute threshold.
		runs: map[string][]forgejo.ActionRun{"sloan/fractals-nextjs": {testStagingRun(7, "main", "waiting", 54)}},
		jobs: map[int64][]forgejo.ActionRunJob{7: {testJob("build", "waiting")}},
	}
	d := newDeployTestReader(stub, nil).Read()

	require.Len(t, d.Runs, 1)
	assert.Equal(t, stagingWorkflow, d.Runs[0].Workflow)
	assert.Equal(t, "no runner picked this up for 6 min", d.Runs[0].Warn)
}

// The two workflows share every cap: a repo's staging runs compete with its
// releases for the newest deployRunsKept rows, and every row of both competes
// for the deployJobsKept job fetches, so a busy week of landings cannot crowd
// the releases out of the block, or the other way round (gt-2h2lx).
func TestDeployReaderCapsBothWorkflowsTogether(t *testing.T) {
	t.Parallel()

	var runs []forgejo.ActionRun
	for i := 0; i < 8; i++ {
		if i%2 == 0 {
			runs = append(runs, testStagingRun(int64(i+1), "main", "success", i))
			continue
		}
		runs = append(runs, testDeployRun(int64(i+1), "v0."+strconv.Itoa(i), "success", i))
	}
	stub := &deployStub{
		repos: []string{"sloan/fractals-nextjs"},
		runs:  map[string][]forgejo.ActionRun{"sloan/fractals-nextjs": runs},
	}
	d := newDeployTestReader(stub, nil).Read()

	require.Len(t, d.Runs, deployRunsKept, "the newest five of the repo's runs, of either workflow")
	assert.Equal(t, []string{"v0.7", "main", "v0.5", "main", "v0.3"}, refsOf(d.Runs), "newest first")
	assert.Equal(t, []string{"deploy.yml", "staging.yml", "deploy.yml", "staging.yml", "deploy.yml"}, workflowsOf(d.Runs))
	assert.Equal(t, []int64{8, 7, 6}, stub.jobsAsked, "jobs for the newest three, the two workflows sharing the cap")
	for i, row := range d.Runs {
		assert.Equal(t, i >= deployJobsKept, row.StagesSkipped, "a run past the cap was never asked for its jobs")
		assert.False(t, row.StagesUnread, "and nothing failed, so the two are told apart")
	}
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
	assert.Equal(t, []string{
		"sloan/fractals-nextjs deploy.yml", "sloan/fractals-nextjs staging.yml",
		"sloan/beads deploy.yml", "sloan/beads staging.yml",
	}, stub.runsAsked, "both repos were asked, each by workflow")
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
// with no stages. A run past the cap is marked skipped, which is not the mark a
// failed jobs call leaves — the page draws the two differently (gt-egffr).
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
		assert.Equal(t, i >= deployJobsKept, row.StagesSkipped, "a run past the cap was never asked for its jobs")
		assert.False(t, row.StagesUnread, "and nothing failed, so the two are told apart")
	}

	// A jobs call that fails is a different mark on the run: its stages were
	// asked for and did not come back, rather than never asked for at all.
	stub.jobsErr = map[int64]error{8: io.EOF}
	d = newDeployTestReader(stub, nil).Read()
	require.Len(t, d.Runs, deployRunsKept)
	assert.True(t, d.Runs[0].StagesUnread, "the failed call is what the run says")
	assert.False(t, d.Runs[0].StagesSkipped, "the run was inside the cap and its jobs were asked for")
}

// refsOf names the rows by ref, which is what the tests address them by.
func refsOf(rows []DeployRun) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Ref)
	}
	return out
}

// workflowsOf names the rows by the workflow each came from, which is what the
// tests that read both workflows address them by.
func workflowsOf(rows []DeployRun) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Workflow)
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

// testHealthRun builds a run of the scheduled health workflow: the check of a
// deployed app, which runs on its own hourly clock rather than behind a
// landing or a release.
func testHealthRun(id int64, status string, created int) forgejo.ActionRun {
	run := testDeployRun(id, "main", status, created)
	run.WorkflowID = ".forgejo/workflows/health.yml"
	run.Event = "schedule"
	return run
}

// healthJobs is one health run's jobs: the off job every run carries, then the
// two environments the workflow checks, in the statuses the test gives them.
func healthJobs(off, staging, prod string) []forgejo.ActionRunJob {
	return []forgejo.ActionRunJob{
		testJob("off", off),
		testJob("staging-integration", staging, "off"),
		testJob("prod-smoke", prod, "staging-integration"),
	}
}

// healthRowsOf names the health rows by app and environment, which is what the
// tests that read more than one of them address them by.
func healthRowsOf(rows []DeployHealthRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.App+"/"+r.Env)
	}
	return out
}

// The scheduled checks reach the page as one row per app and environment, not
// as a run each: health.yml runs hourly, so a row behind every run would bury
// the deploys the block is about. Every run folded into a row is a run the
// repo ran, and the run's own jobs are what say whether the check passed.
func TestDeployReaderFoldsTheHealthRunsIntoRowsPerAppAndEnvironment(t *testing.T) {
	t.Parallel()

	stub := &deployStub{
		repos: []string{"sloan/fractals-nextjs"},
		runs: map[string][]forgejo.ActionRun{"sloan/fractals-nextjs": {
			testHealthRun(3, "success", 41),
			testHealthRun(2, "success", 21),
			testHealthRun(1, "success", 1),
		}},
		jobs: map[int64][]forgejo.ActionRunJob{
			3: healthJobs("success", "success", "success"),
			2: healthJobs("success", "success", "success"),
			1: healthJobs("success", "success", "success"),
		},
	}
	d := newDeployTestReader(stub, nil).Read()

	require.Empty(t, d.Error)
	require.Empty(t, d.Missing)
	assert.Empty(t, d.Runs, "a health run is not a deploy row")
	assert.Empty(t, d.HealthFailed)
	require.Len(t, d.Health, 2)
	assert.Equal(t, []string{"fractals-nextjs/staging-integration", "fractals-nextjs/prod-smoke"}, healthRowsOf(d.Health))

	row := d.Health[0]
	assert.Equal(t, "sloan/fractals-nextjs", row.Repo)
	assert.Equal(t, "success", row.Status)
	assert.Equal(t, "https://forgejo.test/runs/3", row.URL, "the row links the newest run it folded")
	assert.True(t, row.At.Equal(time.Date(2026, 10, 5, 10, 41, 0, 0, time.UTC)), "the result carries the newest run's own time")
	assert.True(t, row.LastGreen.Equal(row.At), "the newest run passed, so it is the last green")
	assert.Equal(t, 3, row.Passed, "all three runs of the window passed")
	assert.Equal(t, 3, row.Runs)
	assert.False(t, row.Unread)
	assert.Equal(t, 3, d.Health[1].Passed, "each environment is read from its own job")

	// Both reads are per repo: the repo's deploy runs, one query per workflow so
	// the checks cannot crowd the deploys off the page, then its health runs.
	assert.Equal(t, []string{"sloan/fractals-nextjs deploy.yml", "sloan/fractals-nextjs staging.yml"}, stub.runsAsked)
	assert.Equal(t, []string{"sloan/fractals-nextjs"}, stub.healthAsked)
	assert.Equal(t, []int64{3, 2, 1}, stub.jobsAsked, "the jobs of every run of the window, which is what tells a pass from an app that is off")
}

// A failed check is the row's own result and the strip's marker, and the last
// green stays the run that did pass: the two are read from the same window, so
// an operator can see how long a check has been failing.
func TestDeployReaderCountsThePassesAndFindsTheLastGreen(t *testing.T) {
	t.Parallel()

	stub := &deployStub{
		repos: []string{"sloan/fractals-nextjs"},
		runs: map[string][]forgejo.ActionRun{"sloan/fractals-nextjs": {
			testHealthRun(3, "failure", 41),
			testHealthRun(2, "success", 21),
			testHealthRun(1, "success", 1),
		}},
		jobs: map[int64][]forgejo.ActionRunJob{
			3: healthJobs("success", "success", "failure"),
			2: healthJobs("success", "success", "success"),
			1: healthJobs("success", "success", "success"),
		},
	}
	d := newDeployTestReader(stub, nil).Read()

	assert.Equal(t, []string{"sloan/fractals-nextjs"}, d.HealthFailed, "the newest run of a turned-on app failed, so the strip marks it")
	require.Len(t, d.Health, 2)

	staging, prod := d.Health[0], d.Health[1]
	assert.Equal(t, "success", staging.Status)
	assert.Equal(t, 3, staging.Passed, "staging's own job passed every run, whatever prod did")
	assert.Equal(t, "failure", prod.Status)
	assert.Equal(t, 2, prod.Passed, "of the window's three runs, prod passed two")
	assert.Equal(t, 3, prod.Runs)
	assert.True(t, prod.At.Equal(time.Date(2026, 10, 5, 10, 41, 0, 0, time.UTC)), "the failure is the newest run's")
	assert.True(t, prod.LastGreen.Equal(time.Date(2026, 10, 5, 10, 21, 0, 0, time.UTC)), "the last green is the newest run that passed, not the newest run")
}

// A run whose checks were skipped is the app being off on purpose: the row
// says so, nothing counts it as a pass, and the strip is not marked — a
// deployment nobody tested must not read as one that was.
func TestDeployReaderReadsASkippedHealthRunAsOffNotAsAPass(t *testing.T) {
	t.Parallel()

	stub := &deployStub{
		repos: []string{"sloan/fractals-nextjs"},
		runs: map[string][]forgejo.ActionRun{"sloan/fractals-nextjs": {
			testHealthRun(2, "success", 41),
			testHealthRun(1, "success", 21),
		}},
		jobs: map[int64][]forgejo.ActionRunJob{
			2: healthJobs("skipped", "skipped", "skipped"),
			1: healthJobs("success", "success", "success"),
		},
	}
	d := newDeployTestReader(stub, nil).Read()

	assert.Empty(t, d.HealthFailed, "an app that is off on purpose is not a failed app")
	require.Len(t, d.Health, 2)
	for _, row := range d.Health {
		assert.Equal(t, "skipped", row.Status, "the row carries the API's own word, which the page says as off on purpose")
		assert.Equal(t, 1, row.Passed, "of two runs, only the one that ran passed")
		assert.Equal(t, 2, row.Runs)
		assert.True(t, row.LastGreen.Equal(time.Date(2026, 10, 5, 10, 21, 0, 0, time.UTC)), "the last green is the run before the app was turned off")
	}
}

// The pass count and the last green are read over a window of the app's newest
// runs, not over everything the repo has ever run: the window is the day the
// workflow checks, and it is also what bounds the calls the block makes.
func TestDeployReaderCountsTheHealthWindowAndNoMore(t *testing.T) {
	t.Parallel()

	var runs []forgejo.ActionRun
	jobs := map[int64][]forgejo.ActionRunJob{}
	for i := 0; i < healthRunWindow+1; i++ {
		id := int64(i + 1)
		runs = append(runs, testHealthRun(id, "success", i))
		if i == 0 {
			// The oldest run, past the window, failed: it is history the rows
			// do not count, so it neither lowers the count nor marks the strip.
			jobs[id] = healthJobs("success", "success", "failure")
			continue
		}
		jobs[id] = healthJobs("success", "success", "success")
	}
	stub := &deployStub{
		repos: []string{"sloan/fractals-nextjs"},
		runs:  map[string][]forgejo.ActionRun{"sloan/fractals-nextjs": runs},
		jobs:  jobs,
	}
	d := newDeployTestReader(stub, nil).Read()

	require.Len(t, d.Health, 2)
	assert.Equal(t, healthRunWindow, d.Health[1].Runs, "the window, and the run past it is not counted")
	assert.Equal(t, healthRunWindow, d.Health[1].Passed, "the run past the window failed, and it is not in the count")
	assert.Empty(t, d.HealthFailed, "the newest check passed, whatever the run past the window did")
	assert.Len(t, stub.jobsAsked, healthRunWindow, "one jobs call per run of the window: the run past it is never asked for")
}

// A repo that neither deploys nor has health runs shows nothing extra: the block
// names an app only once it deploys, and the read costs that repo one list.
func TestDeployReaderWithNoDeployOrHealthRunsShowsNothingExtra(t *testing.T) {
	t.Parallel()

	stub := &deployStub{
		repos: []string{"sloan/gastown"},
	}
	d := newDeployTestReader(stub, nil).Read()

	assert.Empty(t, d.Runs)
	assert.Empty(t, d.Health)
	assert.Empty(t, d.HealthFailed)
	assert.Equal(t, []string{"sloan/gastown"}, stub.healthAsked, "the repo was asked, and answered with no health run")
	assert.Empty(t, stub.jobsAsked)
}

// An app that deploys and has no scheduled check yet is named in the block, one
// row per environment, and each row says it has no check rather than a result.
// No check ran, so the rows are neither a pass nor a failure: the strip stays
// quiet and nothing is counted.
func TestDeployReaderNamesADeployedAppWithNoScheduledChecks(t *testing.T) {
	t.Parallel()

	stub := &deployStub{
		repos: []string{"sloan/beads"},
		runs:  map[string][]forgejo.ActionRun{"sloan/beads": {testDeployRun(4, "v0.2.0", "success", 20)}},
	}
	d := newDeployTestReader(stub, nil).Read()

	require.Len(t, d.Runs, 1)
	require.Len(t, d.Health, 2)
	assert.Equal(t, []string{"beads/staging-integration", "beads/prod-smoke"}, healthRowsOf(d.Health))
	for _, row := range d.Health {
		assert.True(t, row.NoChecks, "the row says the app has no check yet")
		assert.Empty(t, row.Status, "there is no run to read a result from")
		assert.Equal(t, 0, row.Runs, "nothing ran, so nothing is counted as passed")
		assert.Equal(t, 0, row.Passed)
		assert.False(t, row.Unread)
	}
	assert.Empty(t, d.HealthFailed)
	assert.Empty(t, d.HealthUnread)
	assert.Nil(t, d.HealthMark)
}

// "No check yet" is a claim only a listing the block read can make. A deployed
// app whose checks could not be listed, or whose checks name no environment the
// block knows, has checks it could not read: it draws no no-checks rows, and
// the strip names it the way it always has.
func TestDeployReaderDoesNotCallAnUnreadAppNoChecksYet(t *testing.T) {
	t.Parallel()

	deploy := testDeployRun(4, "v0.2.0", "success", 20)

	t.Run("the listing failed", func(t *testing.T) {
		t.Parallel()

		stub := &deployStub{
			repos:     []string{"sloan/fractals-nextjs"},
			runs:      map[string][]forgejo.ActionRun{"sloan/fractals-nextjs": {deploy}},
			healthErr: map[string]error{"sloan/fractals-nextjs": io.EOF},
		}
		d := newDeployTestReader(stub, nil).Read()

		assert.Empty(t, d.Health, "a listing that failed is not a repo with no checks")
		assert.Equal(t, []string{"sloan/fractals-nextjs"}, d.HealthUnread)
	})

	t.Run("the checks name no known environment", func(t *testing.T) {
		t.Parallel()

		stub := &deployStub{
			repos: []string{"sloan/fractals-nextjs"},
			runs: map[string][]forgejo.ActionRun{"sloan/fractals-nextjs": {
				deploy,
				testHealthRun(1, "success", 5),
			}},
			// The workflow's jobs were renamed: the block knows neither of these.
			jobs: map[int64][]forgejo.ActionRunJob{1: {testJob("integration-staging", "success")}},
		}
		d := newDeployTestReader(stub, nil).Read()

		assert.Empty(t, d.Health, "checks the block cannot fold are not a repo with no checks")
		assert.Equal(t, []string{"sloan/fractals-nextjs"}, d.HealthMissing)
	})
}

// The strip's marker names the app whose newest check failed, and only it: a
// town with one app failing and one app off names one.
func TestDeployReaderGivenTwoAppsNamesOnlyTheFailedOne(t *testing.T) {
	t.Parallel()

	stub := &deployStub{
		repos: []string{"sloan/fractals-nextjs", "sloan/arkansas-beaver-committee"},
		runs: map[string][]forgejo.ActionRun{
			"sloan/fractals-nextjs":           {testHealthRun(2, "failure", 5)},
			"sloan/arkansas-beaver-committee": {testHealthRun(1, "success", 7)},
		},
		jobs: map[int64][]forgejo.ActionRunJob{
			2: healthJobs("success", "success", "failure"),
			1: healthJobs("skipped", "skipped", "skipped"),
		},
	}
	d := newDeployTestReader(stub, nil).Read()

	assert.Equal(t, []string{"sloan/fractals-nextjs"}, d.HealthFailed)
	require.Len(t, d.Health, 4)
	assert.Equal(t, []string{
		"arkansas-beaver-committee/staging-integration", "arkansas-beaver-committee/prod-smoke",
		"fractals-nextjs/staging-integration", "fractals-nextjs/prod-smoke",
	}, healthRowsOf(d.Health), "one row per app and environment, the repos in order")
	assert.Equal(t, "skipped", d.Health[0].Status, "the off app's rows are its skipped runs")
}

// A jobs call that fails is not a pass and not a failure: the row says its
// newest run could not be read, and the strip is not marked from a check the
// block never saw. A run whose jobs name no environment the block knows is the
// same answer at the repo's level, since there is no row to draw it on.
func TestDeployReaderSaysAHealthRowItCouldNotRead(t *testing.T) {
	t.Parallel()

	stub := &deployStub{
		repos:   []string{"sloan/fractals-nextjs"},
		runs:    map[string][]forgejo.ActionRun{"sloan/fractals-nextjs": {testHealthRun(2, "failure", 5)}},
		jobsErr: map[int64]error{2: io.EOF},
	}
	d := newDeployTestReader(stub, nil).Read()

	require.Empty(t, d.Error)
	assert.Empty(t, d.HealthFailed, "a run the block could not read is no word on the app")
	assert.Equal(t, []string{"sloan/fractals-nextjs"}, d.HealthUnread,
		"an app nobody has a reading of is named, not left to read as a town that is green")
	require.Len(t, d.Health, 2, "the app's checks are still drawn, as rows it could not read")
	for _, row := range d.Health {
		assert.True(t, row.Unread)
		assert.Empty(t, row.Status, "a result the block did not read is not a result")
		assert.Zero(t, row.Passed)
		assert.Zero(t, row.Runs)
	}

	// Every call failing is the same row: the check is not silently gone, and
	// the environments the block knows are still named.
	assert.Equal(t, []string{"fractals-nextjs/staging-integration", "fractals-nextjs/prod-smoke"}, healthRowsOf(d.Health))

	// A run whose jobs named no environment the block knows is not a check of
	// one, and draws no row at all: the repo is said as one whose checks the
	// block cannot read rather than drawn as a repo with no checks. The run
	// passed: a failing one is filed as failed (TestFoldHealthUnknownEnvKeepsFailure).
	stub.runs = map[string][]forgejo.ActionRun{"sloan/fractals-nextjs": {testHealthRun(2, "success", 5)}}
	stub.jobsErr = nil
	stub.jobs = map[int64][]forgejo.ActionRunJob{2: {testJob("off", "skipped"), testJob("something-else", "success")}}
	d = newDeployTestReader(stub, nil).Read()
	assert.Empty(t, d.Health)
	assert.Equal(t, []string{"sloan/fractals-nextjs"}, d.HealthMissing)
	assert.Empty(t, d.HealthFailed)
	assert.Empty(t, d.HealthUnread)
}

// The strip's marker is raised from the newest run or not at all: a newer run
// the block could not read is a check nobody has a reading of — the strip says
// that rather than the failure, which is an older run's — and an older run's
// failure under it is not what a marker says. The rows already refuse that
// claim, and a marker making it would be the strip disagreeing with the block
// it is drawn beside.
func TestDeployReaderRaisesNoMarkerFromARunUnderAnUnreadNewest(t *testing.T) {
	t.Parallel()

	stub := &deployStub{
		repos: []string{"sloan/fractals-nextjs"},
		runs: map[string][]forgejo.ActionRun{"sloan/fractals-nextjs": {
			testHealthRun(2, "failure", 41),
			testHealthRun(1, "failure", 21),
		}},
		jobs: map[int64][]forgejo.ActionRunJob{
			1: healthJobs("success", "success", "failure"),
		},
		jobsErr: map[int64]error{2: io.EOF},
	}
	d := newDeployTestReader(stub, nil).Read()

	require.Len(t, d.Health, 2)
	assert.True(t, d.Health[1].Unread)
	assert.Empty(t, d.Health[1].Status)
	assert.Empty(t, d.HealthFailed, "the newest check was not read, so nothing is known to have failed")
	assert.Equal(t, []string{"sloan/fractals-nextjs"}, d.HealthUnread, "and the strip says the check went unread")

	// And with the newest run read and passed, the failure under it is not the
	// marker's either: only the newest run's own result is.
	stub.jobsErr = nil
	stub.jobs[2] = healthJobs("success", "success", "success")
	stub.runs["sloan/fractals-nextjs"] = []forgejo.ActionRun{
		testHealthRun(2, "success", 41),
		testHealthRun(1, "failure", 21),
	}
	d = newDeployTestReader(stub, nil).Read()
	assert.Empty(t, d.HealthFailed, "the newest check passed, whatever the run under it did")
	assert.Equal(t, healthSuccess, d.Health[1].Status)
	assert.Equal(t, 1, d.Health[1].Passed, "the older failure is still in the count")
	assert.Equal(t, 2, d.Health[1].Runs)
}

// Two repos with the same base name are two apps, not one: the marker names
// each by its owner/name, so a failing pair is two entries rather than the same
// word twice, and the rows are grouped by repo so neither repo's environments
// are read as the other's.
func TestDeployReaderKeepsSameNamedReposApart(t *testing.T) {
	t.Parallel()

	failed := healthJobs("success", "success", "failure")
	stub := &deployStub{
		repos: []string{"north/web", "south/web"},
		runs: map[string][]forgejo.ActionRun{
			"north/web": {testHealthRun(1, "failure", 5)},
			"south/web": {testHealthRun(2, "failure", 6)},
		},
		jobs: map[int64][]forgejo.ActionRunJob{1: failed, 2: failed},
	}
	d := newDeployTestReader(stub, nil).Read()

	assert.Equal(t, []string{"north/web", "south/web"}, d.HealthFailed, "two repos are two entries, not one name twice")
	assert.Equal(t, []string{
		"web/staging-integration", "web/prod-smoke",
		"web/staging-integration", "web/prod-smoke",
	}, healthRowsOf(d.Health), "one repo's two environments, then the other's")
	assert.Equal(t, []string{"north/web", "north/web", "south/web", "south/web"},
		[]string{d.Health[0].Repo, d.Health[1].Repo, d.Health[2].Repo, d.Health[3].Repo},
		"the rows are keyed by owner/name, so the two repos cannot be read as one")
}

// A run the block could not read under a newer one the block could does not
// become the row's result: the newest run is the newest word on the app, and
// the passes the block did read are still counted.
func TestDeployReaderKeepsTheNewestRunAsTheRowsResult(t *testing.T) {
	t.Parallel()

	stub := &deployStub{
		repos: []string{"sloan/fractals-nextjs"},
		runs: map[string][]forgejo.ActionRun{"sloan/fractals-nextjs": {
			testHealthRun(2, "failure", 41),
			testHealthRun(1, "success", 21),
		}},
		jobs: map[int64][]forgejo.ActionRunJob{
			2: healthJobs("success", "success", "failure"),
		},
		jobsErr: map[int64]error{1: io.EOF},
	}
	d := newDeployTestReader(stub, nil).Read()

	require.Len(t, d.Health, 2)
	assert.Equal(t, "failure", d.Health[1].Status)
	assert.Equal(t, 0, d.Health[1].Passed, "the run under it was not read, so it is not counted either way")
	assert.Equal(t, 1, d.Health[1].Runs)
	assert.Equal(t, []string{"sloan/fractals-nextjs"}, d.HealthFailed)
}

// The page reads the block by the reader's own field names, so the names the
// scheduled rows and the strip's marker travel under are pinned here: a rename
// on either side of the wire is a row that silently stops being drawn, and a
// marker no app can raise.
func TestDeploysJSONCarriesTheNamesThePageReads(t *testing.T) {
	t.Parallel()

	d := &Deploys{
		Runs: []DeployRun{},
		Health: []DeployHealthRow{{
			Repo:      "sloan/fractals-nextjs",
			App:       "fractals-nextjs",
			Env:       healthEnvProd,
			Status:    healthFailure,
			At:        deployNow.Add(-time.Minute),
			LastGreen: deployNow.Add(-time.Hour),
			Passed:    23,
			Runs:      24,
			URL:       "https://forgejo.test/runs/7",
			Unread:    true,
		}},
		HealthFailed:  []string{"sloan/fractals-nextjs"},
		HealthUnread:  []string{"sloan/gt-cloud"},
		HealthMark:    &HealthMark{State: "red", Text: "health: sloan/fractals-nextjs"},
		HealthMissing: []string{"sloan/beads"},
	}
	b, err := json.Marshal(d)
	require.NoError(t, err)
	for _, want := range []string{
		`"health":[`, `"health_failed":["sloan/fractals-nextjs"]`, `"health_unread":["sloan/gt-cloud"]`,
		`"health_missing":["sloan/beads"]`, `"health_mark":{"state":"red","text":"health: sloan/fractals-nextjs"}`,
		`"repo":"sloan/fractals-nextjs"`, `"app":"fractals-nextjs"`, `"env":"prod-smoke"`,
		`"status":"failure"`, `"at":`, `"last_green":`, `"passed":23`, `"runs":24`,
		`"url":"https://forgejo.test/runs/7"`, `"unread":true`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("the block's JSON has no %s: the page reads nothing there", want)
		}
	}
}

// A repo whose checks could not be listed is not a repo with no checks: it is
// named, and its deploy runs are still drawn. An expired budget is that same
// answer rather than a silent one — the town's checks nobody could read must
// not read as a town with nothing to check.
func TestDeployReaderNamesTheReposWhoseChecksItCouldNotList(t *testing.T) {
	t.Parallel()

	for _, cause := range []error{io.EOF, context.DeadlineExceeded} {
		stub := &deployStub{
			repos: []string{"sloan/beads", "sloan/fractals-nextjs"},
			runs: map[string][]forgejo.ActionRun{
				"sloan/beads":           {testDeployRun(4, "v0.2.0", "success", 20)},
				"sloan/fractals-nextjs": {testHealthRun(1, "success", 5)},
			},
			healthErr: map[string]error{"sloan/fractals-nextjs": cause},
		}
		d := newDeployTestReader(stub, nil).Read()

		require.Empty(t, d.Error)
		require.Len(t, d.Runs, 1, "the repo's own runs answered, and are drawn whatever its checks did")
		assert.Empty(t, d.Missing, "the repo's deploy runs answered, so it is not a repo that failed")
		assert.Equal(t, []string{"sloan/fractals-nextjs"}, d.HealthUnread, "a listing that failed is an unread check, which the strip says")
		assert.Empty(t, d.HealthMissing, "the repo may well have health.yml; missing is for runs that name no known environment")
		assert.Equal(t, []string{"beads/staging-integration", "beads/prod-smoke"}, healthRowsOf(d.Health), "beads deploys with no check yet; fractals' checks are unknown, so it has no rows to draw")
		assert.Empty(t, d.HealthFailed)
		require.NotNil(t, d.HealthMark, "a repo whose checks could not be listed must not leave the strip quiet")
		assert.Equal(t, HealthMark{State: "degraded", Text: "not read: sloan/fractals-nextjs"}, *d.HealthMark)
	}
}

// A listing that answered with no health runs is a repo without the workflow:
// it raises nothing, unlike one that failed. Its rows are the NoChecks rows of
// an app that deploys, which are not a failure either.
func TestDeployReaderRaisesNothingForARepoWithNoHealthRuns(t *testing.T) {
	t.Parallel()

	stub := &deployStub{
		repos: []string{"sloan/beads"},
		runs:  map[string][]forgejo.ActionRun{"sloan/beads": {testDeployRun(4, "v0.2.0", "success", 20)}},
	}
	d := newDeployTestReader(stub, nil).Read()

	assert.Empty(t, d.HealthFailed)
	assert.Empty(t, d.HealthUnread)
	assert.Empty(t, d.HealthMissing)
	assert.Nil(t, d.HealthMark)
}

// The strip's pill is decided in Go: red while any check failed, degraded when
// only unread checks remain, absent when neither list has a name.
func TestHealthMark(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name           string
		failed, unread []string
		want           *HealthMark
	}{
		{name: "neither list leaves no pill"},
		{name: "a failure is red", failed: []string{"a/x", "b/y"}, want: &HealthMark{State: "red", Text: "health: a/x, b/y"}},
		{name: "unread alone is degraded", unread: []string{"a/x"}, want: &HealthMark{State: "degraded", Text: "not read: a/x"}},
		{name: "a failure beside an unread check stays red and says both", failed: []string{"a/x"}, unread: []string{"b/y"},
			want: &HealthMark{State: "red", Text: "health: a/x · not read: b/y"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, healthMark(tc.failed, tc.unread))
		})
	}
}

// A repo whose checks name no environment the block knows is not a repo with no
// checks: the runs are there and the block cannot fold them, which is the same
// answer as a listing it could not make, so it is named the same way. Drawing
// nothing would be the strip's alarm and the block's rows both going quiet over
// a workflow whose jobs were renamed.
func TestDeployReaderNamesTheReposWhoseChecksNameNoEnvironment(t *testing.T) {
	t.Parallel()

	stub := &deployStub{
		repos: []string{"sloan/beads", "sloan/fractals-nextjs"},
		runs: map[string][]forgejo.ActionRun{
			"sloan/beads":           {testDeployRun(4, "v0.2.0", "success", 20)},
			"sloan/fractals-nextjs": {testHealthRun(1, "success", 5)},
		},
		// The workflow's jobs were renamed: the block knows neither of these.
		jobs: map[int64][]forgejo.ActionRunJob{1: {testJob("off", "success"), testJob("integration-staging", "success")}},
	}
	d := newDeployTestReader(stub, nil).Read()

	require.Len(t, d.Runs, 1, "the repo's own runs are drawn whatever its checks did")
	assert.Equal(t, []string{"sloan/fractals-nextjs"}, d.HealthMissing)
	assert.Equal(t, []string{"beads/staging-integration", "beads/prod-smoke"}, healthRowsOf(d.Health), "beads deploys with no check yet; fractals has no rows to draw")
	assert.Empty(t, d.HealthFailed)
	assert.Empty(t, d.HealthUnread)
}

// The checks read on a budget of their own, so a refresh whose deploy reads
// spent the whole Forgejo timeout still reads them: the repo's health is neither
// unread nor missing for want of time the deploys took (gt-bfh09).
func TestReadHealthHasOwnDeadline(t *testing.T) {
	t.Parallel()

	stub := &deployStub{
		repos:        []string{"sloan/fractals-nextjs"},
		spendTimeout: true,
		runs: map[string][]forgejo.ActionRun{"sloan/fractals-nextjs": {
			testHealthRun(1, "success", 1),
		}},
		jobs: map[int64][]forgejo.ActionRunJob{1: healthJobs("success", "success", "success")},
	}
	r := newDeployTestReader(stub, nil)
	r.timeout = time.Millisecond
	d := r.Read()

	require.NoError(t, stub.healthCtxErr, "the checks' read began under the deploy reads' spent deadline")
	assert.Empty(t, d.HealthUnread)
	require.Len(t, d.Health, 2)
	assert.Equal(t, "success", d.Health[0].Status)
}

// A repo whose newest check failed and names no environment the block knows has
// no row to carry the failure, so the strip is the only place it can show: it
// is filed as failed, not as a repo whose checks are missing (gt-bfh09).
func TestFoldHealthUnknownEnvKeepsFailure(t *testing.T) {
	t.Parallel()

	window := []healthRun{{
		run:    testHealthRun(1, "failure", 5),
		envs:   map[string]string{},
		failed: true,
	}}
	got := foldHealth("sloan/fractals-nextjs", window)

	assert.True(t, got.failed, "a failing newest run is a failure whatever its jobs are called")
	assert.Empty(t, got.rows)

	stub := &deployStub{
		repos: []string{"sloan/fractals-nextjs"},
		runs:  map[string][]forgejo.ActionRun{"sloan/fractals-nextjs": {testHealthRun(1, "failure", 5)}},
		jobs:  map[int64][]forgejo.ActionRunJob{1: {testJob("off", "success"), testJob("integration-staging", "failure")}},
	}
	d := newDeployTestReader(stub, nil).Read()
	assert.Equal(t, []string{"sloan/fractals-nextjs"}, d.HealthFailed)
	assert.Empty(t, d.HealthMissing)
}

// Neither a cancelled check nor a run still moving is a failure: only a check
// that ended in failure raises the marker.
func TestDeployReaderTellsACancelledCheckFromARunStillMoving(t *testing.T) {
	t.Parallel()

	stub := &deployStub{
		repos: []string{"sloan/fractals-nextjs", "sloan/gt-cloud"},
		runs: map[string][]forgejo.ActionRun{
			"sloan/fractals-nextjs": {testHealthRun(1, forgejoRunCancelled, 5)},
			"sloan/gt-cloud":        {testHealthRun(2, "running", 5)},
		},
		jobs: map[int64][]forgejo.ActionRunJob{
			1: healthJobs("success", forgejoRunCancelled, "skipped"),
			2: healthJobs("success", "running", "waiting"),
		},
	}
	d := newDeployTestReader(stub, nil).Read()

	assert.Empty(t, d.HealthFailed, "a cancelled check is nobody's verdict, so it raises no marker")
	assert.Empty(t, d.HealthUnread)
	assert.Nil(t, d.HealthMark)

	require.Len(t, d.Health, 4)
	assert.Equal(t, forgejoRunCancelled, d.Health[0].Status, "the row carries the API's own word for it")
	assert.Equal(t, "waiting", d.Health[3].Status, "a check that has not finished is not a failure")

	// A run still in flight with a job that has already finished failed is the
	// same answer: nothing is marked until the run ends, so the strip cannot
	// alarm on a check that is still going, and cannot take the alarm away again
	// the moment that job is retried. The row still carries the job's own word.
	inFlight := &deployStub{
		repos: []string{"sloan/gt-cloud"},
		runs:  map[string][]forgejo.ActionRun{"sloan/gt-cloud": {testHealthRun(2, "running", 5)}},
		jobs:  map[int64][]forgejo.ActionRunJob{2: healthJobs("success", "failure", "waiting")},
	}
	d = newDeployTestReader(inFlight, nil).Read()
	assert.Empty(t, d.HealthFailed, "a run in flight is not a failure, whatever one of its jobs says")
	assert.Empty(t, d.HealthUnread)
	require.Len(t, d.Health, 2)
	assert.Equal(t, "failure", d.Health[0].Status, "the row still carries the job's own word")
}

// The marker reads the newest run, not only the two environments the rows name:
// a job the block does not know failing is still the app's newest check
// failing, and the rows beside it are not the whole word on the app.
func TestDeployReaderMarksAFailureInAJobTheRowsDoNotName(t *testing.T) {
	t.Parallel()

	stub := &deployStub{
		repos: []string{"sloan/fractals-nextjs"},
		runs:  map[string][]forgejo.ActionRun{"sloan/fractals-nextjs": {testHealthRun(1, "failure", 5)}},
		jobs: map[int64][]forgejo.ActionRunJob{1: {
			testJob("off", "success"),
			testJob("staging-integration", "success", "off"),
			testJob("prod-smoke", "success", "staging-integration"),
			testJob("notify", "failure", "prod-smoke"),
		}},
	}
	d := newDeployTestReader(stub, nil).Read()

	assert.Equal(t, []string{"sloan/fractals-nextjs"}, d.HealthFailed)
	require.Len(t, d.Health, 2)
	for _, row := range d.Health {
		assert.Equal(t, "success", row.Status, "both environments passed; the run itself is what failed")
	}
}

// The scheduled checks are the refresh's last read and run on a budget of their
// own: the jobs call per run of every window is the refresh's costliest read,
// and the deploy rows — the block's own subject — must not be what a slow
// Forgejo takes it out of.
func TestDeployReaderGivesTheChecksABudgetOfTheirOwn(t *testing.T) {
	t.Parallel()

	stub := &deployStub{
		repos: []string{"sloan/fractals-nextjs"},
		runs: map[string][]forgejo.ActionRun{"sloan/fractals-nextjs": {
			testDeployRun(7, "v0.1.0", "success", 30),
			testHealthRun(1, "success", 5),
		}},
		jobs: map[int64][]forgejo.ActionRunJob{7: {testJob("build", "success")}, 1: healthJobs("success", "success", "success")},
	}
	d := newDeployTestReader(stub, nil).Read()

	require.Len(t, d.Runs, 1)
	require.Len(t, d.Health, 2)
	require.False(t, stub.jobsDeadline.IsZero(), "the deploy run's jobs were never asked for")
	require.False(t, stub.healthDeadline.IsZero(), "the checks were never read")
	assert.True(t, stub.healthDeadline.Before(stub.jobsDeadline),
		"the checks' read is not on a budget of its own: it can spend the deadline the deploy rows need")
	assert.LessOrEqual(t, time.Until(stub.healthDeadline), healthReadBudget+time.Second,
		"the checks' budget is not what the reader says it is")
}

// A check that has ended is read once, not on every refresh: health.yml runs
// hourly and the panel refreshes by the minute, so a jobs call per run of the
// window per refresh is the block's costliest read, spent on runs that cannot
// change. A run still moving is read again, since its jobs are not final.
func TestDeployReaderReadsAFinishedChecksJobsOnce(t *testing.T) {
	t.Parallel()

	stub := &deployStub{
		repos: []string{"sloan/fractals-nextjs"},
		runs: map[string][]forgejo.ActionRun{"sloan/fractals-nextjs": {
			testHealthRun(1, "success", 5),
		}},
		jobs: map[int64][]forgejo.ActionRunJob{1: healthJobs("success", "success", "success")},
	}
	reader := newDeployTestReader(stub, nil)
	reader.Read()
	assert.Equal(t, []int64{1}, stub.jobsAsked)

	// The same window, an unchanged run: the second refresh asks for nothing.
	reader.Read()
	assert.Equal(t, []int64{1}, stub.jobsAsked, "a check that has ended was read again")

	// A run still in flight is not final, so it is read on each refresh until it
	// ends; the run under it is not read again.
	stub.runs["sloan/fractals-nextjs"] = []forgejo.ActionRun{
		testHealthRun(2, "running", 6),
		testHealthRun(1, "success", 5),
	}
	stub.jobs[2] = healthJobs("success", "running", "waiting")
	reader.Read()
	assert.Equal(t, []int64{1, 2}, stub.jobsAsked, "a run that has not finished is read again")

	reader.Read()
	assert.Equal(t, []int64{1, 2, 2}, stub.jobsAsked, "and only it is")
}

// Only a final read is held: an ended run with a job still waiting or blocked is
// read again, and so is a run that was re-run, which keeps its id but shows a
// new status or a new stop time.
func TestDeployReaderHoldsOnlyAFinalRead(t *testing.T) {
	t.Parallel()

	const repo = "sloan/fractals-nextjs"
	run := testHealthRun(1, "failure", 5)
	run.Stopped = stamp(4)
	stub := &deployStub{
		repos: []string{repo},
		runs:  map[string][]forgejo.ActionRun{repo: {run}},
		jobs:  map[int64][]forgejo.ActionRunJob{1: healthJobs("success", "success", "blocked")},
	}
	reader := newDeployTestReader(stub, nil)

	reader.Read()
	reader.Read()
	assert.Equal(t, []int64{1, 1}, stub.jobsAsked, "a job still blocked is not final, so the run is read again")

	stub.jobs[1] = healthJobs("success", "success", "failure")
	d := reader.Read()
	assert.Equal(t, []string{repo}, d.HealthFailed)
	reader.Read()
	assert.Equal(t, []int64{1, 1, 1}, stub.jobsAsked, "every job final: held from here")

	// The run is re-run: same id, now moving. Its fresh jobs are read, and the
	// failure it held is gone from the strip while it runs.
	rerun := run
	rerun.Status, rerun.Stopped = "running", ""
	stub.runs[repo] = []forgejo.ActionRun{rerun}
	stub.jobs[1] = healthJobs("success", "running", "waiting")
	d = reader.Read()
	assert.Equal(t, []int64{1, 1, 1, 1}, stub.jobsAsked)
	assert.Empty(t, d.HealthFailed, "a re-run in flight is not the failure it was")

	// It ends in failure again with the same status word but a new stop time.
	stub.jobs[1] = healthJobs("success", "success", "failure")
	again := run
	again.Stopped = stamp(2)
	stub.runs[repo] = []forgejo.ActionRun{again}
	reader.Read()
	reader.Read()
	assert.Equal(t, []int64{1, 1, 1, 1, 1}, stub.jobsAsked, "a new stop time is a new run of the job, read once more and then held")
}

// A check the block has read once is never read as unreadable after: the
// marker is raised from the newest run, so a refresh that runs out of budget
// under a check must not take an alarm off the strip that the refresh before it
// put there. No alarm may depend on which minute the page was opened at.
func TestDeployReaderKeepsAMarkerThroughAReadThatFails(t *testing.T) {
	t.Parallel()

	stub := &deployStub{
		repos: []string{"sloan/fractals-nextjs"},
		runs: map[string][]forgejo.ActionRun{"sloan/fractals-nextjs": {
			testHealthRun(1, "failure", 5),
		}},
		jobs: map[int64][]forgejo.ActionRunJob{1: healthJobs("success", "success", "failure")},
	}
	reader := newDeployTestReader(stub, nil)
	d := reader.Read()
	require.Equal(t, []string{"sloan/fractals-nextjs"}, d.HealthFailed)

	stub.jobsErr = map[int64]error{1: context.DeadlineExceeded}
	d = reader.Read()
	assert.Empty(t, d.HealthUnread, "a check that has been read is not read again as unreadable")
	assert.Equal(t, []string{"sloan/fractals-nextjs"}, d.HealthFailed, "and the strip's alarm stands")
	require.Len(t, d.Health, 2)
	assert.Equal(t, healthFailure, d.Health[1].Status)
}

// A run the API sent no jobs for is a check the block could not read, not one
// nothing went wrong in: the jobs are where a check says what it did, so an
// ended run with none is the same answer as a jobs call that failed. Nothing is
// held from it either, since holding it would make that answer permanent for a
// run that will never be asked again.
func TestDeployReaderReadsAnEndedCheckWithNoJobsAsUnread(t *testing.T) {
	t.Parallel()

	stub := &deployStub{
		repos: []string{"sloan/fractals-nextjs"},
		// No jobs entry for run 1: the API answered with an empty list.
		runs: map[string][]forgejo.ActionRun{"sloan/fractals-nextjs": {testHealthRun(1, "failure", 5)}},
	}
	reader := newDeployTestReader(stub, nil)
	d := reader.Read()

	assert.Empty(t, d.HealthFailed, "a run with no jobs is no word on the app, whatever the run's own status says")
	assert.Equal(t, []string{"sloan/fractals-nextjs"}, d.HealthUnread)
	require.Len(t, d.Health, 2)
	for _, row := range d.Health {
		assert.True(t, row.Unread)
		assert.Zero(t, row.Runs, "a run the block did not read is in neither the passes nor the window's runs")
	}

	// An answer that was empty is not an answer: the run has ended, and the
	// refresh after it still asks, since nothing about it was held.
	reader.Read()
	assert.Equal(t, []int64{1, 1}, stub.jobsAsked, "an ended run with no jobs was held as though it had been read")

	// The run's own status is read with its jobs, so a run the API calls a
	// failure with jobs that read green is still the app's newest check failing.
	stub.jobs = map[int64][]forgejo.ActionRunJob{1: healthJobs("success", "success", "success")}
	d = newDeployTestReader(stub, nil).Read()
	assert.Equal(t, []string{"sloan/fractals-nextjs"}, d.HealthFailed, "the run's own word on the app was ignored")
}

// The pass count is over the runs the block read the window for, and the runs it
// could not read are counted beside it: a window of three with one unread run is
// two passes out of two read, and one the block has no reading of — not two out
// of three, which would read as a check that failed.
func TestDeployReaderCountsTheRunsItCouldNotReadBesideThePasses(t *testing.T) {
	t.Parallel()

	stub := &deployStub{
		repos: []string{"sloan/fractals-nextjs"},
		runs: map[string][]forgejo.ActionRun{"sloan/fractals-nextjs": {
			testHealthRun(3, "success", 41),
			testHealthRun(2, "success", 21),
			testHealthRun(1, "success", 1),
		}},
		jobs: map[int64][]forgejo.ActionRunJob{
			3: healthJobs("success", "success", "success"),
			2: healthJobs("success", "success", "success"),
		},
		jobsErr: map[int64]error{1: io.EOF},
	}
	d := newDeployTestReader(stub, nil).Read()

	require.Len(t, d.Health, 2)
	for _, row := range d.Health {
		assert.Equal(t, 2, row.Runs, "the two runs the block read")
		assert.Equal(t, 2, row.Passed)
		assert.Equal(t, 1, row.NotRead, "and the run it could not read, said rather than dropped")
	}

	// The newest run unread is the same count with no passes to put it beside.
	stub.runs["sloan/fractals-nextjs"] = []forgejo.ActionRun{testHealthRun(3, "success", 41)}
	stub.jobsErr = map[int64]error{3: io.EOF}
	d = newDeployTestReader(stub, nil).Read()
	require.Len(t, d.Health, 2)
	for _, row := range d.Health {
		assert.Zero(t, row.Runs)
		assert.Zero(t, row.Passed)
		assert.Equal(t, 1, row.NotRead)
	}
}

// An app repo runs health.yml hourly behind its deploys, so the block asks for
// the deploy workflows by name: a repo's last release is days old and its checks
// are not, and a page of the repo's runs whole is the checks, with the deploys
// behind them falling off it.
func TestDeployReaderAsksForTheDeployWorkflowsByName(t *testing.T) {
	t.Parallel()

	runs := []forgejo.ActionRun{testDeployRun(9, "v0.1.0", "success", 60)}
	for i := 0; i < 3*healthRunWindow; i++ {
		runs = append(runs, testHealthRun(int64(100+i), "success", i))
	}
	stub := &deployStub{
		repos: []string{"sloan/fractals-nextjs"},
		runs:  map[string][]forgejo.ActionRun{"sloan/fractals-nextjs": runs},
		jobs:  map[int64][]forgejo.ActionRunJob{9: {testJob("build", "success")}},
	}
	d := newDeployTestReader(stub, nil).Read()

	require.Len(t, d.Runs, 1, "the release is drawn under the checks the repo has run since")
	assert.Equal(t, "v0.1.0", d.Runs[0].Ref)
	assert.Equal(t,
		[]string{"sloan/fractals-nextjs " + deployWorkflow, "sloan/fractals-nextjs " + stagingWorkflow},
		stub.runsAsked, "the deploy read names each workflow rather than taking the repo's runs whole")
}
