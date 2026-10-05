package forgejo

import (
	"context"
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

// RunFilter narrows a run list; a zero field is not sent.
type RunFilter struct {
	Event   []string
	Status  []string
	HeadSHA string
	Ref     string
	Page    int
	Limit   int
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
	if f.Page > 0 {
		q.Set("page", strconv.Itoa(f.Page))
	}
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
