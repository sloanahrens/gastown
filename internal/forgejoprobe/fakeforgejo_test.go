package forgejoprobe

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// The repository, commit and runner ids the fake Forgejo answers for.
const (
	fakeRepoPath = "/repos/acme/rig"
	fakeRunID    = 12
	fakeJobID    = 311
)

// fakeCall is one request the probe sent to the fake Forgejo: the method, the
// escaped path below /api/v1, the query and the token the request presented.
type fakeCall struct {
	method string
	path   string
	query  string
	token  string
}

// fakeForgejo is one Forgejo instance holding the repository acme/rig, served as
// an in-process http.RoundTripper so the probe's real client reaches it with no
// socket. Its commit status for sha walks plan, one entry per read, and the last
// entry repeats. An entry is a state, "absent" for no status yet, or a
// comma-separated list of states the context carries at once, the later one with
// the higher id deciding. The fake records every request, so a test sees each
// call the code makes.
type fakeForgejo struct {
	sha     string
	context string
	plan    []string

	// repoStatus answers GET /repos/acme/rig when it is set, with repoBody.
	repoStatus int
	repoBody   string
	// transportErr fails every request before it is answered, as a dead socket does.
	transportErr error
	// noRun answers the run list with no run, and jobName names the failing job.
	noRun   bool
	jobName string
	jobLog  string

	calls   []fakeCall
	reads   int
	deleted []string
}

// RoundTrip answers one request from the routes the probe calls. A request whose
// context was canceled fails before it is answered, as a real transport does, so a
// call made under a canceled run's context fails here. Only cancellation is
// honoured: a deadline is taken from the injected clock, which a test sets to a
// fixed time, not from the wall clock a real transport compares it with. Any other
// route answers 404, so a call the probe should not make fails loudly.
func (f *fakeForgejo) RoundTrip(req *http.Request) (*http.Response, error) {
	if f.transportErr != nil {
		return nil, f.transportErr
	}
	if errors.Is(req.Context().Err(), context.Canceled) {
		return nil, context.Canceled
	}
	path := strings.TrimPrefix(req.URL.EscapedPath(), "/api/v1")
	f.calls = append(f.calls, fakeCall{
		method: req.Method,
		path:   path,
		query:  req.URL.RawQuery,
		token:  strings.TrimPrefix(req.Header.Get("Authorization"), "token "),
	})
	get := req.Method == http.MethodGet
	switch {
	case get && path == fakeRepoPath:
		if f.repoStatus != 0 {
			return fakeResponse(req, f.repoStatus, f.repoBody), nil
		}
		return fakeResponse(req, http.StatusOK, `{"full_name":"acme/rig"}`), nil
	case get && path == fakeRepoPath+"/commits/"+f.sha+"/status":
		f.reads++
		return fakeResponse(req, http.StatusOK, f.statusBody()), nil
	case get && path == fakeRepoPath+"/actions/runs":
		return fakeResponse(req, http.StatusOK, f.runsBody()), nil
	case get && path == fakeRepoPath+"/actions/runs/12/jobs":
		return fakeResponse(req, http.StatusOK, f.jobsBody()), nil
	case get && path == fakeRepoPath+"/actions/jobs/311/logs":
		return fakeResponse(req, http.StatusOK, f.jobLog), nil
	case req.Method == http.MethodDelete && path == fakeRepoPath+"/branches/land%2Fprobe-rig":
		f.deleted = append(f.deleted, "land/probe-rig")
		return fakeResponse(req, http.StatusNoContent, ""), nil
	}
	return fakeResponse(req, http.StatusNotFound, `{"message":"unhandled fake route"}`), nil
}

// statusBody is the combined status of sha for the current read of the plan.
func (f *fakeForgejo) statusBody() string {
	entry := f.plan[min(f.reads, len(f.plan))-1]
	if entry == "absent" {
		return mustJSON(forgejo.CombinedStatus{State: forgejo.StatePending, SHA: f.sha, Statuses: []forgejo.CommitStatus{}})
	}
	states := strings.Split(entry, ",")
	cs := forgejo.CombinedStatus{State: forgejo.CommitState(states[len(states)-1]), SHA: f.sha, TotalCount: 1}
	for i, st := range states {
		cs.Statuses = append(cs.Statuses, forgejo.CommitStatus{
			ID:      int64(3 + 6*i),
			Status:  forgejo.CommitState(st),
			Context: f.context,
		})
	}
	return mustJSON(cs)
}

// runsBody is the run list: the push run on sha, unless the case names none.
func (f *fakeForgejo) runsBody() string {
	list := forgejo.RunList{Runs: []forgejo.ActionRun{}}
	if !f.noRun {
		list.TotalCount = 1
		list.Runs = append(list.Runs, forgejo.ActionRun{
			ID:         fakeRunID,
			Title:      "probe",
			Status:     "failure",
			Event:      "push",
			CommitSHA:  f.sha,
			PrettyRef:  "land/probe-rig",
			WorkflowID: "ci.yml",
		})
	}
	return mustJSON(list)
}

// jobsBody is the jobs of the run: one failing job, named jobName.
func (f *fakeForgejo) jobsBody() string {
	return mustJSON([]forgejo.ActionRunJob{{ID: fakeJobID, RunID: fakeRunID, Name: f.jobName, Status: "failure"}})
}

// count reports how many requests with method reached exactly path.
func (f *fakeForgejo) count(method, path string) int {
	n := 0
	for _, c := range f.calls {
		if c.method == method && c.path == path {
			n++
		}
	}
	return n
}

// countContaining reports how many requests with method reached a path that
// contains sub.
func (f *fakeForgejo) countContaining(method, sub string) int {
	n := 0
	for _, c := range f.calls {
		if c.method == method && strings.Contains(c.path, sub) {
			n++
		}
	}
	return n
}

// fakeResponse is the response the transport hands back for one request.
func fakeResponse(req *http.Request, code int, body string) *http.Response {
	return &http.Response{
		StatusCode: code,
		Status:     http.StatusText(code),
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

// mustJSON encodes v for a response body; the fake's values always encode.
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
