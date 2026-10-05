package forgejo

import (
	"context"
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
