package forgejo

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newProbeClient builds a client the way the town health probe does: no role
// token, an explicit instance.
func newProbeClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	c, err := NewClient("",
		WithoutToken(),
		WithBaseURL(testBase),
		WithHTTPClient(&http.Client{Transport: handlerTransport{h}}))
	require.NoError(t, err)
	return c
}

// TestVersionReadsTheUnauthenticatedEndpoint: the liveness probe asks
// /version and sends no Authorization header, so it works on an instance
// whose token file is missing — which is exactly when an operator needs to
// know whether the instance itself is up (gt-fn9e6.11).
func TestVersionReadsTheUnauthenticatedEndpoint(t *testing.T) {
	t.Parallel()
	rec := &recorder{body: []byte(`{"version":"16.0.5"}`)}
	v, err := newProbeClient(t, rec).Version(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "16.0.5", v.Version)
	assert.Equal(t, "/api/v1/version", rec.req.URL.Path)
	assert.Empty(t, rec.req.Header.Get("Authorization"), "the version probe must send no token")
}

// TestVersionNon2xxIsAnError: a non-2xx answer is a failure, not a version.
func TestVersionNon2xxIsAnError(t *testing.T) {
	t.Parallel()
	rec := &recorder{status: http.StatusServiceUnavailable, body: []byte(`{"message":"down"}`)}
	_, err := newProbeClient(t, rec).Version(context.Background())
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusServiceUnavailable, apiErr.StatusCode)
}

// TestWaitingRunReadsTheOldestQueuedRun: the run queue read asks for the runs
// a runner has not picked up, and answers with the oldest one — a run that
// started is not in the queue whatever its status (gt-fn9e6.56).
func TestWaitingRunReadsTheOldestQueuedRun(t *testing.T) {
	t.Parallel()
	rec := &recorder{body: []byte(`{"workflow_runs":[
		{"id":41,"status":"blocked","created":"2026-10-05T11:20:00Z","started":"0001-01-01T00:00:00Z"},
		{"id":42,"status":"waiting","created":"2026-10-05T11:40:00Z","started":"0001-01-01T00:00:00Z"},
		{"id":43,"status":"waiting","created":"2026-10-05T11:10:00Z","started":"2026-10-05T11:11:00Z"},
		{"id":44,"status":"success","created":"2026-10-05T11:00:00Z","started":"2026-10-05T11:00:00Z"}
	]}`)}
	got, err := newTestClient(t, rec).WaitingRun(context.Background(), "gastown", "gastown")
	require.NoError(t, err)
	assert.True(t, got.Waiting)
	assert.Equal(t, int64(41), got.ID)
	assert.Equal(t, "2026-10-05T11:20:00Z", got.Since.UTC().Format(time.RFC3339))
	assert.Equal(t, "/api/v1/repos/gastown/gastown/actions/runs", rec.req.URL.Path)
	assert.Equal(t, []string{"waiting", "blocked"}, rec.req.URL.Query()["status"])
}

// TestWaitingRunEmptyQueueIsNoWait: an empty queue is not a failure, it is
// nothing waiting — the state the field reads green on.
func TestWaitingRunEmptyQueueIsNoWait(t *testing.T) {
	t.Parallel()
	rec := &recorder{body: []byte(`{"workflow_runs":[],"total_count":0}`)}
	got, err := newTestClient(t, rec).WaitingRun(context.Background(), "gastown", "gastown")
	require.NoError(t, err)
	assert.False(t, got.Waiting)
}

// TestWaitingRunUnreadableCreatedIsAnError: an age the probe cannot read is
// not evidence about the queue, so it is an error rather than a verdict
// (gt-fn9e6.56).
func TestWaitingRunUnreadableCreatedIsAnError(t *testing.T) {
	t.Parallel()
	rec := &recorder{body: []byte(`{"workflow_runs":[{"id":7,"status":"waiting","created":"yesterday"}]}`)}
	_, err := newTestClient(t, rec).WaitingRun(context.Background(), "gastown", "gastown")
	require.Error(t, err)
	assert.ErrorContains(t, err, "run 7")
}

// TestWaitingRunForbiddenIsAnError: a run list the token cannot read is an
// error, so the probe reports an unanswerable question rather than an empty
// queue (gt-fn9e6.56).
func TestWaitingRunForbiddenIsAnError(t *testing.T) {
	t.Parallel()
	rec := &recorder{status: http.StatusForbidden, body: []byte(`{"message":"user should be the owner of the repo"}`)}
	_, err := newTestClient(t, rec).WaitingRun(context.Background(), "gastown", "gastown")
	var apiErr *APIError
	require.Error(t, err)
	assert.True(t, errors.As(err, &apiErr))
	assert.Equal(t, http.StatusForbidden, apiErr.StatusCode)
}
