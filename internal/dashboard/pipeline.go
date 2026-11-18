package dashboard

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// The Pipeline block: the gastown repository's post-land and images workflow
// runs, newest first, one row each. It reads through the same viewer client the
// Deploys block reads, and adds nothing of its own: a run's id, status and
// stamps are all it draws.

const (
	// postLandWorkflow and imagesWorkflow are the workflows whose runs the block
	// lists, by their base names: the landing's post-land step and the image
	// build. A run of any other workflow is not in the block.
	postLandWorkflow = "post-land.yml"
	imagesWorkflow   = "images.yml"

	// pipelineRunsKept is how many runs the block lists, both workflows together.
	// It is also the page each workflow is asked for: a run of one workflow
	// cannot be in the newest few overall while a newer run of the same workflow
	// is left out.
	pipelineRunsKept = 12

	// pipelineSHAKept is how much of a run's commit the block shows, the
	// head sha as the twelve characters the row can name a commit by.
	pipelineSHAKept = 12
)

// pipelineWorkflows are the workflows the block lists, in the order it asks for
// them.
var pipelineWorkflows = []string{postLandWorkflow, imagesWorkflow}

// The run statuses the block draws. Forgejo's waiting and blocked both read as
// queued: a run that has not started yet. A status outside these, such as
// canceled or skipped, is drawn as the API names it, so it is never read as a
// success or a failure it is not.
const (
	pipelineQueued  = "queued"
	pipelineRunning = "running"
	pipelineSuccess = "success"
	pipelineFailure = "failure"
)

// PipelineRun is one post-land or images run as the block draws it.
type PipelineRun struct {
	// Workflow is the workflow's base name: "post-land.yml" or "images.yml".
	Workflow string `json:"workflow"`
	// SHA is the commit the run is for, the first pipelineSHAKept characters.
	SHA string `json:"sha,omitempty"`
	// Status is queued, running, success or failure, or the API's own word for a
	// run that ended some other way.
	Status string `json:"status"`
	// Started and Ended are when the run began and finished, absent while the run
	// has not reached them: a queued run has neither, a running run has no end.
	Started time.Time `json:"started,omitzero"`
	Ended   time.Time `json:"ended,omitzero"`
	// URL is the run's html_url, absent when the API sent none or sent something
	// that is not http(s).
	URL string `json:"url,omitempty"`
}

// GastownPipeline is the Pipeline block's data: the newest runs and when they
// were read. Error is the class of a refresh that failed, whose Runs are then
// the last good read and At the time of that read.
type GastownPipeline struct {
	Runs  []PipelineRun `json:"runs"`
	At    time.Time     `json:"at"`
	Error string        `json:"error,omitempty"`
}

// GastownPipelineReader reads the gastown repository's pipeline runs. It holds
// the last good read, so a refresh that cannot reach the repository shows the
// runs it last read, with their age and the failure, rather than an empty block
// that reads as a repository with no runs.
type GastownPipelineReader struct {
	api  deployAPI
	repo string // owner/name of the gastown repository
	now  func() time.Time

	mu   sync.Mutex
	last *GastownPipeline
}

// NewGastownPipelineReader builds a reader over api, the same client the Deploys
// block reads, for the repository repo names as owner/name. The caller names the
// repository: the source tree does not carry the operator's Forgejo owner, and
// the rig's Forgejo config is where it is read from.
func NewGastownPipelineReader(api deployAPI, repo string) *GastownPipelineReader {
	return &GastownPipelineReader{api: api, repo: repo, now: time.Now}
}

// Read refreshes the block. A refresh that fails returns the last good runs with
// the error class set and their read time unchanged; one that fails before any
// good read returns no runs and the error alone.
func (r *GastownPipelineReader) Read() *GastownPipeline {
	runs, err := r.fetch()
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		class := forgejoErrorClass(err)
		if r.last == nil {
			return &GastownPipeline{Runs: []PipelineRun{}, Error: class}
		}
		return &GastownPipeline{Runs: r.last.Runs, At: r.last.At, Error: class}
	}
	r.last = &GastownPipeline{Runs: runs, At: r.now()}
	return &GastownPipeline{Runs: runs, At: r.last.At}
}

// fetch reads both workflows' newest runs of the gastown repository and merges
// them. A query that fails fails the read: a run list short one workflow would
// read as a workflow with no runs.
func (r *GastownPipelineReader) fetch() ([]PipelineRun, error) {
	owner, repo, ok := strings.Cut(r.repo, "/")
	if !ok || owner == "" || repo == "" {
		return nil, fmt.Errorf("%w: %s", errRepoName, strconv.Quote(r.repo))
	}
	ctx, cancel := context.WithTimeout(context.Background(), forgejoTimeout)
	defer cancel()

	var found []deployRun
	for _, workflow := range pipelineWorkflows {
		list, err := r.api.ListRuns(ctx, owner, repo, forgejo.RunFilter{WorkflowID: workflow, Page: 1, Limit: pipelineRunsKept})
		if err != nil {
			return nil, err
		}
		for _, run := range list.Runs {
			// The API matches the workflow on its path, so the base name is the same
			// query; a server that answers loosely still cannot put another
			// workflow's run in the block.
			if path.Base(run.WorkflowID) != workflow {
				continue
			}
			found = append(found, deployRun{owner: owner, name: repo, repo: r.repo, workflow: workflow, run: run, created: parseRunStamp(run.Created)})
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].created.After(found[j].created) })
	if len(found) > pipelineRunsKept {
		found = found[:pipelineRunsKept]
	}

	runs := make([]PipelineRun, 0, len(found))
	for _, dr := range found {
		runs = append(runs, PipelineRun{
			Workflow: dr.workflow,
			SHA:      pipelineSHA(dr.run.CommitSHA),
			Status:   pipelineStatus(dr.run.Status),
			Started:  parseRunStamp(dr.run.Started),
			Ended:    parseRunStamp(dr.run.Stopped),
			URL:      deployURL(dr.run.HTMLURL),
		})
	}
	return runs, nil
}

// pipelineStatus is the block's word for a run's status: the four the block
// draws, and the API's own word for any other.
func pipelineStatus(status string) string {
	switch status {
	case deployWaiting, forgejoRunBlocked:
		return pipelineQueued
	case "running":
		return pipelineRunning
	case "success":
		return pipelineSuccess
	case "failure":
		return pipelineFailure
	}
	return status
}

// pipelineSHA is the head of a commit sha as the block shows it, or the whole
// value when it is shorter.
func pipelineSHA(sha string) string {
	if len(sha) <= pipelineSHAKept {
		return sha
	}
	return sha[:pipelineSHAKept]
}
