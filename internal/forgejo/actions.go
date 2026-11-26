package forgejo

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// ActionRun is one workflow run.
type ActionRun struct {
	ID         int64  `json:"id"`
	Index      int64  `json:"index_in_repo"`
	Title      string `json:"title"`
	Status     string `json:"status"`
	Event      string `json:"event"`
	CommitSHA  string `json:"commit_sha"`
	PrettyRef  string `json:"prettyref"`
	WorkflowID string `json:"workflow_id"`
	HTMLURL    string `json:"html_url"`
	// Created, Started and Stopped are the run's RFC3339 timestamps as the API
	// sends them. Started is unset until a runner picks the run up and Stopped
	// is unset until it ends, so both read as the zero time while it waits.
	Created string `json:"created"`
	Started string `json:"started"`
	Stopped string `json:"stopped"`
	// Duration is the API's int64 nanosecond elapsed time, zero for a run that
	// has not ended (verified against 16.0.5's swagger: Duration is Go's
	// time.Duration, which marshals as a nanosecond count).
	Duration int64 `json:"duration"`
}

// ActionRunJob is one job of a run; its ID addresses the job's log.
type ActionRunJob struct {
	ID     int64    `json:"id"`
	RunID  int64    `json:"run_id"`
	Name   string   `json:"name"`
	Status string   `json:"status"`
	Needs  []string `json:"needs,omitempty"`
}

// RunList is a page of workflow runs.
type RunList struct {
	TotalCount int         `json:"total_count"`
	Runs       []ActionRun `json:"workflow_runs"`
}

// RunFilter narrows a run list; a zero field is not sent, except Page, which
// always goes as at least 1: Forgejo without a page parameter ignores limit
// and converts every run in the repository (gt-0izqj9).
type RunFilter struct {
	Event   []string
	Status  []string
	HeadSHA string
	Ref     string
	// WorkflowID narrows the list to one workflow. The API matches it on the
	// workflow's path in the repository, so its base name answers too
	// ("staging.yml" and ".forgejo/workflows/staging.yml" are the same
	// workflow; verified live against Forgejo 16.0.5).
	WorkflowID string
	Page       int
	Limit      int
}

// values renders f as query parameters.
func (f RunFilter) values() url.Values {
	q := url.Values{}
	for _, e := range f.Event {
		q.Add("event", e)
	}
	for _, s := range f.Status {
		q.Add("status", s)
	}
	if f.HeadSHA != "" {
		q.Set("head_sha", f.HeadSHA)
	}
	if f.Ref != "" {
		q.Set("ref", f.Ref)
	}
	if f.WorkflowID != "" {
		q.Set("workflow_id", f.WorkflowID)
	}
	page := f.Page
	if page <= 0 {
		page = 1
	}
	q.Set("page", strconv.Itoa(page))
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	return q
}

// ListRuns returns the workflow runs matching f.
func (c *Client) ListRuns(ctx context.Context, owner, repo string, f RunFilter) (*RunList, error) {
	var out RunList
	if err := c.call(ctx, http.MethodGet, repoPath(owner, repo, "/actions/runs"), f.values(), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// taskPage is one page of a repository's Actions tasks; its pointers make a
// body without total_count or workflow_runs an error, not an empty answer.
type taskPage struct {
	TotalCount   *int         `json:"total_count"`
	WorkflowRuns *[]taskEntry `json:"workflow_runs"`
}

// taskEntry is one entry of a tasks page: the id the API sends for the task.
type taskEntry struct {
	ID *int64 `json:"id"`
}

// taskPageSize is the page size the walk asks for; maxTaskPages bounds it.
const (
	taskPageSize = 50
	maxTaskPages = 200
)

// RunningTaskIDs returns the running Actions task ids, page by page until the
// ids read equal the server's total_count; a count that moves mid-walk is
// refused, and an id the server repeats is returned once. A set whose unique
// ids pass total_count never reads equal, so maxTaskPages ends that walk with
// an error. Offset pages cannot see one task replaced by another with an
// unchanged count, so that one task may go unread until the next call.
func (c *Client) RunningTaskIDs(ctx context.Context, owner, repo string) ([]int64, error) {
	path := repoPath(owner, repo, "/actions/tasks")
	ids := make([]int64, 0, taskPageSize)
	seen := make(map[int64]struct{})
	total := 0
	for page := 1; page <= maxTaskPages; page++ {
		q := url.Values{}
		q.Set("status", "running")
		q.Set("page", strconv.Itoa(page))
		q.Set("limit", strconv.Itoa(taskPageSize))
		var out taskPage
		if err := c.call(ctx, http.MethodGet, path, q, nil, &out); err != nil {
			return nil, err
		}
		if out.TotalCount == nil {
			return nil, fmt.Errorf("forgejo: %s page %d has no total_count", path, page)
		}
		if page == 1 {
			total = *out.TotalCount
		} else if *out.TotalCount != total {
			return nil, fmt.Errorf("forgejo: %s page %d counts %d tasks, page 1 counted %d; the running set moved", path, page, *out.TotalCount, total)
		}
		if out.WorkflowRuns == nil {
			return nil, fmt.Errorf("forgejo: %s page %d has no workflow_runs", path, page)
		}
		for i, task := range *out.WorkflowRuns {
			if task.ID == nil {
				return nil, fmt.Errorf("forgejo: %s page %d entry %d has no id", path, page, i)
			}
			if _, dup := seen[*task.ID]; dup {
				continue
			}
			seen[*task.ID] = struct{}{}
			ids = append(ids, *task.ID)
		}
		if len(ids) == total {
			return ids, nil
		}
		if len(*out.WorkflowRuns) == 0 {
			return nil, fmt.Errorf("forgejo: %s page %d was empty after %d of %d tasks", path, page, len(ids), total)
		}
	}
	return nil, fmt.Errorf("forgejo: %s is still short of its %d tasks after %d pages", path, total, maxTaskPages)
}

// DeleteRun removes workflow run runID. A non-2xx answer comes back as an
// *APIError.
func (c *Client) DeleteRun(ctx context.Context, owner, repo string, runID int64) error {
	path := repoPath(owner, repo, "/actions/runs/"+strconv.FormatInt(runID, 10))
	return c.call(ctx, http.MethodDelete, path, nil, nil, nil)
}

// ListRunJobs returns the jobs of run runID.
func (c *Client) ListRunJobs(ctx context.Context, owner, repo string, runID int64) ([]ActionRunJob, error) {
	var out []ActionRunJob
	path := repoPath(owner, repo, "/actions/runs/"+strconv.FormatInt(runID, 10)+"/jobs")
	if err := c.call(ctx, http.MethodGet, path, nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// JobLogs returns a job's whole log.
func (c *Client) JobLogs(ctx context.Context, owner, repo string, jobID int64) (string, error) {
	return c.jobLogs(ctx, owner, repo, jobID, 0)
}

// JobLogsTail returns at most the last maxBytes of a job's log, which is what
// a rework note carries. It asks the server for that byte range so a long log
// is not fetched whole; a server that ignores the range returns the whole log,
// and the caller keeps the tail it needs.
func (c *Client) JobLogsTail(ctx context.Context, owner, repo string, jobID int64, maxBytes int64) (string, error) {
	return c.jobLogs(ctx, owner, repo, jobID, maxBytes)
}

// jobLogs fetches a job log, tailBytes > 0 requesting the last tailBytes.
func (c *Client) jobLogs(ctx context.Context, owner, repo string, jobID, tailBytes int64) (string, error) {
	path := repoPath(owner, repo, "/actions/jobs/"+strconv.FormatInt(jobID, 10)+"/logs")
	header := http.Header{"Accept": {"text/plain"}}
	if tailBytes > 0 {
		header.Set("Range", "bytes=-"+strconv.FormatInt(tailBytes, 10))
	}
	data, err := c.do(ctx, http.MethodGet, path, nil, nil, header)
	if err != nil {
		return "", err
	}
	return string(data), nil
}
