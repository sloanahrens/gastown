package forgejo

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A run carries the timing the Actions panel orders, ages and takes its stats
// by: RFC3339 created, started and stopped, and a duration the API counts in
// nanoseconds (27s arrives as 27000000000).
func TestListRunsReadsTheRunTiming(t *testing.T) {
	t.Parallel()
	rec := &recorder{body: []byte(`{
	  "total_count": 1,
	  "workflow_runs": [
	    {
	      "id": 91,
	      "index_in_repo": 12,
	      "title": "land: polecat/mica/gt-fn9e6.47+muvk2gi0 (aaaa1111) onto main (bbbb2222)",
	      "status": "success",
	      "event": "push",
	      "commit_sha": "cafe1234567890",
	      "prettyref": "land/gt-fn9e6.47",
	      "workflow_id": "gate.yml",
	      "html_url": "https://forgejo.test/gastownhall/gastown/actions/runs/91",
	      "created": "2026-10-05T18:00:40Z",
	      "started": "2026-10-05T18:00:45Z",
	      "stopped": "2026-10-05T18:01:12Z",
	      "duration": 27000000000
	    }
	  ]
	}`)}
	c := newTestClient(t, rec)

	got, err := c.ListRuns(context.Background(), "gastownhall", "gastown", RunFilter{})
	require.NoError(t, err)
	require.Len(t, got.Runs, 1)
	run := got.Runs[0]
	assert.Equal(t, "2026-10-05T18:00:40Z", run.Created)
	assert.Equal(t, "2026-10-05T18:00:45Z", run.Started)
	assert.Equal(t, "2026-10-05T18:01:12Z", run.Stopped)
	assert.Equal(t, int64(27000000000), run.Duration, "the API's duration is nanoseconds")
	assert.Equal(t, "land: polecat/mica/gt-fn9e6.47+muvk2gi0 (aaaa1111) onto main (bbbb2222)", run.Title)
}

// A run that has not started yet answers without its start and stop, and the
// reader keeps them empty rather than inventing the zero time.
func TestListRunsQueuedRunHasNoTimingYet(t *testing.T) {
	t.Parallel()
	rec := &recorder{body: []byte(`{
	  "total_count": 1,
	  "workflow_runs": [
	    {"id": 92, "title": "land: (aaaa1111) onto main", "status": "waiting",
	     "created": "2026-10-05T18:00:40Z", "duration": 0}
	  ]
	}`)}
	c := newTestClient(t, rec)

	got, err := c.ListRuns(context.Background(), "gastownhall", "gastown", RunFilter{})
	require.NoError(t, err)
	require.Len(t, got.Runs, 1)
	assert.Equal(t, "2026-10-05T18:00:40Z", got.Runs[0].Created)
	assert.Empty(t, got.Runs[0].Started)
	assert.Empty(t, got.Runs[0].Stopped)
	assert.Zero(t, got.Runs[0].Duration)
}

// A run list always carries a page: Forgejo reads a request without one as
// unpaged, ignores limit and converts every run in the repository, so the list
// costs the repo's whole run history (gt-0izqj9).
func TestRunFilterValues_AlwaysSendsAPage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		filter    RunFilter
		wantPage  string
		wantLimit string // empty means the limit stays unset
	}{
		{name: "zero page defaults to 1", filter: RunFilter{}, wantPage: "1"},
		{name: "negative page defaults to 1", filter: RunFilter{Page: -2}, wantPage: "1"},
		{name: "explicit page is kept", filter: RunFilter{Page: 3}, wantPage: "3"},
		{name: "zero limit stays unset", filter: RunFilter{Page: 1, Limit: 0}, wantPage: "1"},
		{name: "limit goes beside the page", filter: RunFilter{Page: 3, Limit: 7}, wantPage: "3", wantLimit: "7"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			q := tt.filter.values()
			assert.Equal(t, tt.wantPage, q.Get("page"))
			if tt.wantLimit == "" {
				assert.False(t, q.Has("limit"), "limit should be absent when zero")
			} else {
				assert.Equal(t, tt.wantLimit, q.Get("limit"))
			}
		})
	}
}

// ListRuns sends the page its filter defaults to, so a caller that leaves Page
// zero still gets one bounded page instead of the server's full conversion.
func TestListRunsSendsPageOneForAZeroPageFilter(t *testing.T) {
	t.Parallel()
	rec := &recorder{body: []byte(`{"total_count": 0, "workflow_runs": []}`)}
	c := newTestClient(t, rec)

	_, err := c.ListRuns(context.Background(), "alice", "gastown", RunFilter{Status: []string{"waiting"}})
	require.NoError(t, err)
	q := rec.req.URL.Query()
	assert.True(t, q.Has("page"), "the request must carry a page parameter")
	assert.Equal(t, "1", q.Get("page"))
	assert.False(t, q.Has("limit"), "a zero Limit stays unset")
}

// DeleteRun sends one DELETE to the run's own path with the token, so the
// retention step removes exactly the run it planned.
func TestDeleteRunSendsDeleteToTheRunPath(t *testing.T) {
	t.Parallel()
	rec := &recorder{status: http.StatusNoContent}
	c := newTestClient(t, rec)

	require.NoError(t, c.DeleteRun(context.Background(), "alice", "gastown", 4217))
	assert.Equal(t, http.MethodDelete, rec.req.Method)
	assert.Equal(t, "/api/v1/repos/alice/gastown/actions/runs/4217", rec.req.URL.Path)
	assert.Equal(t, "token test-token", rec.req.Header.Get("Authorization"))
}

// A refused delete surfaces as the API error with its status, so the caller
// can tell a run Forgejo would not remove from a transport failure.
func TestDeleteRunReturnsTheRefusalAsAnAPIError(t *testing.T) {
	t.Parallel()
	rec := &recorder{status: http.StatusConflict, body: []byte(`{"message":"conflict"}`)}
	c := newTestClient(t, rec)

	err := c.DeleteRun(context.Background(), "alice", "gastown", 4217)
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusConflict, apiErr.StatusCode)
	assert.True(t, apiErr.IsConflict())
}

// taskPages answers each task page with the body stored at its page number,
// so a walk runs without a socket; fallback answers unlisted pages.
type taskPages struct {
	bodies   map[string][]byte
	fallback []byte
	status   map[string]int
	queries  []url.Values
	paths    []string
}

func (p *taskPages) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p.queries = append(p.queries, q)
	p.paths = append(p.paths, r.URL.Path)
	body, ok := p.bodies[q.Get("page")]
	if !ok {
		body = p.fallback
	}
	if s := p.status[q.Get("page")]; s != 0 {
		w.WriteHeader(s)
	}
	if body != nil {
		_, _ = w.Write(body)
	}
}

// tasksBody renders a tasks page of n entries from firstID, counting total.
func tasksBody(firstID int64, n, total int) []byte {
	entries := make([]string, n)
	for i := range entries {
		entries[i] = fmt.Sprintf(`{"id":%d}`, firstID+int64(i))
	}
	return []byte(fmt.Sprintf(`{"total_count":%d,"workflow_runs":[%s]}`, total, strings.Join(entries, ",")))
}

// RunningTaskIDs returns every page's ids with the running filter, page and
// limit on each request; an empty page gives none and a short page is walked.
func TestRunningTaskIDs(t *testing.T) {
	t.Parallel()
	const path = "/api/v1/repos/gastownhall/gastown/actions/tasks"
	tests := []struct {
		name               string
		pages              map[string][]byte
		count, first, last int64
	}{
		{name: "pages of fifty then two", pages: map[string][]byte{"1": tasksBody(1, 50, 52), "2": tasksBody(51, 2, 52)}, count: 52, first: 1, last: 52},
		{name: "an empty first page gives none", pages: map[string][]byte{"1": tasksBody(1, 0, 0)}},
		{name: "a short page is walked to the counted total", pages: map[string][]byte{"1": tasksBody(1, 10, 12), "2": tasksBody(11, 2, 12)}, count: 12, first: 1, last: 12},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := &taskPages{bodies: tt.pages}
			c := newTestClient(t, h)

			ids, err := c.RunningTaskIDs(context.Background(), "gastownhall", "gastown")
			require.NoError(t, err)
			require.Len(t, ids, int(tt.count))
			if tt.count > 0 {
				assert.Equal(t, tt.first, ids[0], "first id")
				assert.Equal(t, tt.last, ids[len(ids)-1], "last id")
			}
			require.Len(t, h.queries, len(tt.pages), "requests sent")
			for i, q := range h.queries {
				assert.Equal(t, "limit=50&page="+fmt.Sprint(i+1)+"&status=running", q.Encode(), "page %d query", i+1)
				assert.Equal(t, path, h.paths[i], "page %d path", i+1)
			}
		})
	}
}

// RunningTaskIDs returns an id the server repeats on two pages once, so the
// repeat cannot stand in for a task the walk has not read yet.
func TestRunningTaskIDsSkipsDuplicates(t *testing.T) {
	t.Parallel()
	h := &taskPages{bodies: map[string][]byte{
		"1": tasksBody(1, 50, 52),
		// 50 repeats page 1's last entry.
		"2": tasksBody(50, 3, 52),
	}}
	c := newTestClient(t, h)

	ids, err := c.RunningTaskIDs(context.Background(), "gastownhall", "gastown")
	require.NoError(t, err)
	require.Len(t, ids, 52)
	assert.Equal(t, int64(1), ids[0], "first id")
	assert.Equal(t, int64(52), ids[len(ids)-1], "last id")
	require.Len(t, h.queries, 2, "the walk ends when the unique ids reach the total")
}

// RunningTaskIDs refuses what it cannot read as the running set rather than
// returning a shorter list: a failed page, a bad body, a moved count, a short
// page, or a walk past the page bound.
func TestRunningTaskIDsRefuses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		bodies   map[string][]byte
		fallback []byte
		status   map[string]int
		want     string
	}{
		{name: "a failure on the first page", status: map[string]int{"1": http.StatusInternalServerError}, want: "returned 500"},
		{name: "a failure after the first page", bodies: map[string][]byte{"1": tasksBody(1, 50, 52)}, status: map[string]int{"2": http.StatusInternalServerError}, want: "returned 500"},
		{name: "a body without total_count", bodies: map[string][]byte{"1": []byte(`{"workflow_runs":[]}`)}, want: "no total_count"},
		{name: "a body without workflow_runs", bodies: map[string][]byte{"1": []byte(`{"total_count":0}`)}, want: "no workflow_runs"},
		{name: "an entry without an id", bodies: map[string][]byte{"1": []byte(`{"total_count":1,"workflow_runs":[{}]}`)}, want: "no id"},
		{name: "a count that changes mid-walk", bodies: map[string][]byte{"1": tasksBody(1, 50, 60), "2": tasksBody(51, 10, 50)}, want: "the running set moved"},
		{name: "an empty page before the count", bodies: map[string][]byte{"1": tasksBody(1, 50, 60), "2": tasksBody(51, 0, 60)}, want: "was empty"},
		{name: "a walk past the page bound", fallback: tasksBody(1, 1, 1000), want: "after 200 pages"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h := &taskPages{bodies: tt.bodies, fallback: tt.fallback, status: tt.status}
			c := newTestClient(t, h)

			ids, err := c.RunningTaskIDs(context.Background(), "gastownhall", "gastown")
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			assert.Nil(t, ids)
		})
	}
}
