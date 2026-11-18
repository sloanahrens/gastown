package dashboard

import (
	"context"
	"errors"
	"fmt"
	"path"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/forgejo"
)

const (
	pipelineTestRepo = "acme/gastown"
	pipelineTestSHA  = "645edaf6a1b2c3d4e5f60718293a4b5c6d7e8f90"
	pipelineTestURL  = "https://forgejo.test/acme/gastown/actions/runs/"
)

// pipelineStub answers the calls the pipeline reader makes. It narrows the runs
// to the workflow a query names, as Forgejo does, unless unfiltered is set: then
// it answers every query with every run, which a server that ignores the filter
// would do.
type pipelineStub struct {
	runs        []forgejo.ActionRun
	err         error
	workflowErr map[string]error // a failure for one workflow's query only
	unfiltered  bool
	asked       []string // owner/repo and workflow of each run query
	limits      []int
}

func (s *pipelineStub) ListUserRepos(context.Context) ([]forgejo.Repository, error) {
	return nil, errors.New("not used by the pipeline reader")
}

func (s *pipelineStub) ListRunJobs(context.Context, string, string, int64) ([]forgejo.ActionRunJob, error) {
	return nil, errors.New("not used by the pipeline reader")
}

func (s *pipelineStub) ListRuns(_ context.Context, owner, repo string, f forgejo.RunFilter) (*forgejo.RunList, error) {
	s.asked = append(s.asked, owner+"/"+repo+" "+f.WorkflowID)
	s.limits = append(s.limits, f.Limit)
	if s.err != nil {
		return nil, s.err
	}
	if err := s.workflowErr[f.WorkflowID]; err != nil {
		return nil, err
	}
	if s.unfiltered {
		return &forgejo.RunList{Runs: s.runs}, nil
	}
	var out []forgejo.ActionRun
	for _, run := range s.runs {
		if path.Base(run.WorkflowID) == f.WorkflowID {
			out = append(out, run)
		}
	}
	return &forgejo.RunList{Runs: out}, nil
}

// pipelineTestRun builds a run of the named workflow the way Forgejo sends it:
// the workflow by its path in the repository, the commit, the status and the
// created stamp. Started and Stopped are left for the test to set.
func pipelineTestRun(id int64, workflow, status string, created int) forgejo.ActionRun {
	return forgejo.ActionRun{
		ID:         id,
		WorkflowID: ".forgejo/workflows/" + workflow,
		CommitSHA:  pipelineTestSHA,
		Status:     status,
		Event:      "push",
		Created:    stamp(created),
		HTMLURL:    pipelineTestURL + fmt.Sprint(id),
	}
}

// newPipelineTestReader wires a reader to a stub with the tests' clock.
func newPipelineTestReader(stub *pipelineStub, now *time.Time) *GastownPipelineReader {
	r := NewGastownPipelineReader(stub, pipelineTestRepo)
	r.now = func() time.Time { return *now }
	return r
}

func TestGastownPipelineNewestFirstWithShaStatusAndTimes(t *testing.T) {
	t.Parallel()
	post := pipelineTestRun(1, postLandWorkflow, "success", 1)
	post.Started, post.Stopped = stamp(2), stamp(5)
	images := pipelineTestRun(2, imagesWorkflow, "running", 3)
	images.Started = stamp(4)
	queued := pipelineTestRun(3, postLandWorkflow, "waiting", 6)
	other := pipelineTestRun(4, "deploy.yml", "success", 9)
	stub := &pipelineStub{runs: []forgejo.ActionRun{post, images, queued, other}}
	now := deployNow
	got := newPipelineTestReader(stub, &now).Read()

	require.Empty(t, got.Error)
	require.Len(t, got.Runs, 3, "the deploy.yml run is not in the block")
	assert.Equal(t, []string{postLandWorkflow, imagesWorkflow, postLandWorkflow}, []string{got.Runs[0].Workflow, got.Runs[1].Workflow, got.Runs[2].Workflow})
	assert.Equal(t, "queued", got.Runs[0].Status)
	assert.True(t, got.Runs[0].Started.IsZero(), "a queued run has not started")
	assert.True(t, got.Runs[0].Ended.IsZero())
	assert.Equal(t, "running", got.Runs[1].Status)
	assert.Equal(t, stamp(4), got.Runs[1].Started.Format(time.RFC3339))
	assert.True(t, got.Runs[1].Ended.IsZero(), "a running run has no end")
	assert.Equal(t, "success", got.Runs[2].Status)
	assert.Equal(t, stamp(2), got.Runs[2].Started.Format(time.RFC3339))
	assert.Equal(t, stamp(5), got.Runs[2].Ended.Format(time.RFC3339))
	assert.Equal(t, pipelineTestSHA[:12], got.Runs[2].SHA, "the sha is its first twelve characters")
	assert.Equal(t, pipelineTestURL+"1", got.Runs[2].URL)
}

func TestGastownPipelineStatusWords(t *testing.T) {
	t.Parallel()
	cases := []struct {
		api, want string
	}{
		{"waiting", "queued"},
		{"blocked", "queued"},
		{"running", "running"},
		{"success", "success"},
		{"failure", "failure"},
		// A status outside the four is drawn as the API names it, never as a
		// success or a failure it is not.
		{"cancelled", "cancelled"},
		{"skipped", "skipped"},
	}
	for _, tc := range cases {
		t.Run(tc.api, func(t *testing.T) {
			stub := &pipelineStub{runs: []forgejo.ActionRun{pipelineTestRun(1, postLandWorkflow, tc.api, 1)}}
			now := deployNow
			got := newPipelineTestReader(stub, &now).Read()
			require.Len(t, got.Runs, 1)
			assert.Equal(t, tc.want, got.Runs[0].Status)
		})
	}
}

func TestGastownPipelineIgnoresOtherWorkflowsEvenWhenServerDoesNotFilter(t *testing.T) {
	t.Parallel()
	runs := []forgejo.ActionRun{
		pipelineTestRun(1, postLandWorkflow, "success", 1),
		pipelineTestRun(2, imagesWorkflow, "success", 2),
		pipelineTestRun(3, "deploy.yml", "success", 3),
		pipelineTestRun(4, "staging.yml", "failure", 4),
		pipelineTestRun(5, "health.yml", "failure", 5),
	}
	for _, tc := range []struct {
		name       string
		unfiltered bool
	}{
		{"server narrows by workflow", false},
		{"server answers every query with every run", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &pipelineStub{runs: runs, unfiltered: tc.unfiltered}
			now := deployNow
			got := newPipelineTestReader(stub, &now).Read()
			var ids []string
			for _, run := range got.Runs {
				ids = append(ids, run.URL)
			}
			assert.Equal(t, []string{pipelineTestURL + "2", pipelineTestURL + "1"}, ids)
		})
	}
}

func TestGastownPipelineCapsAtTwelve(t *testing.T) {
	t.Parallel()
	var runs []forgejo.ActionRun
	for i := 1; i <= 15; i++ {
		workflow := postLandWorkflow
		if i%2 == 0 {
			workflow = imagesWorkflow
		}
		runs = append(runs, pipelineTestRun(int64(i), workflow, "success", i))
	}
	stub := &pipelineStub{runs: runs}
	now := deployNow
	got := newPipelineTestReader(stub, &now).Read()

	require.Len(t, got.Runs, 12)
	assert.Equal(t, pipelineTestURL+"15", got.Runs[0].URL, "the newest run comes first")
	assert.Equal(t, pipelineTestURL+"4", got.Runs[11].URL, "the twelve newest are kept")
	for _, limit := range stub.limits {
		assert.Equal(t, 12, limit, "each workflow is asked for at most the cap")
	}
}

func TestGastownPipelineReadsOnlyTheRepositoryItIsGiven(t *testing.T) {
	t.Parallel()
	stub := &pipelineStub{}
	now := deployNow
	newPipelineTestReader(stub, &now).Read()

	assert.Equal(t, []string{"acme/gastown post-land.yml", "acme/gastown images.yml"}, stub.asked)
}

func TestGastownPipelineRefusesARepoThatIsNotOwnerName(t *testing.T) {
	t.Parallel()
	for _, repo := range []string{"", "gastown", "/gastown", "acme/"} {
		t.Run(repo, func(t *testing.T) {
			stub := &pipelineStub{runs: []forgejo.ActionRun{pipelineTestRun(1, postLandWorkflow, "success", 1)}}
			now := deployNow
			r := NewGastownPipelineReader(stub, repo)
			r.now = func() time.Time { return now }
			got := r.Read()

			assert.Equal(t, "bad repo name", got.Error)
			assert.Empty(t, got.Runs)
			assert.Empty(t, stub.asked, "no run list is asked for a name that is not owner/name")
		})
	}
}

func TestGastownPipelineUnreadableRepoKeepsLastGood(t *testing.T) {
	t.Parallel()
	stub := &pipelineStub{runs: []forgejo.ActionRun{
		pipelineTestRun(1, postLandWorkflow, "success", 1),
		pipelineTestRun(2, imagesWorkflow, "failure", 2),
	}}
	now := deployNow
	r := newPipelineTestReader(stub, &now)
	first := r.Read()
	require.Empty(t, first.Error)
	require.Len(t, first.Runs, 2)

	// The repository becomes unreadable a minute later: the block keeps the runs it
	// last read, says the refresh failed, and keeps the time of the good read.
	now = deployNow.Add(time.Minute)
	stub.err = errors.New("connection refused")
	failed := r.Read()
	assert.Equal(t, "unreachable", failed.Error)
	assert.Equal(t, first.Runs, failed.Runs)
	assert.Equal(t, first.At, failed.At, "the age of the snapshot is the last good read's")

	// A recovered repository replaces the last good read and clears the failure.
	now = deployNow.Add(2 * time.Minute)
	stub.err = nil
	stub.runs = append(stub.runs, pipelineTestRun(3, postLandWorkflow, "success", 3))
	recovered := r.Read()
	assert.Empty(t, recovered.Error)
	assert.Len(t, recovered.Runs, 3)
	assert.Equal(t, now, recovered.At)
}

func TestGastownPipelineUnreadableBeforeAnyGoodReadIsEmptyWithError(t *testing.T) {
	t.Parallel()
	stub := &pipelineStub{err: errors.New("connection refused")}
	now := deployNow
	got := newPipelineTestReader(stub, &now).Read()

	assert.Equal(t, "unreachable", got.Error)
	assert.Empty(t, got.Runs)
	assert.True(t, got.At.IsZero(), "there is no read time to show before any good read")
}

func TestGastownPipelineOneWorkflowFailingFailsTheRead(t *testing.T) {
	t.Parallel()
	// A run list that answers for post-land but not for images is not a list with no
	// images runs: the read fails, and the block keeps what it last had.
	stub := &pipelineStub{runs: []forgejo.ActionRun{pipelineTestRun(1, postLandWorkflow, "success", 1)}}
	now := deployNow
	r := newPipelineTestReader(stub, &now)
	require.Len(t, r.Read().Runs, 1)

	stub.workflowErr = map[string]error{imagesWorkflow: &forgejo.APIError{StatusCode: 403}}
	got := r.Read()
	assert.Equal(t, "unauthorized", got.Error)
	assert.Len(t, got.Runs, 1, "the last good list, not one with the post-land run alone")
}
