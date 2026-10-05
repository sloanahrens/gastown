package forgejo

import (
	"context"
	"errors"
	"net/http"
	"testing"

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

// TestListRepoRunnersReadsTheArray: the runner list is a bare array (Forgejo
// 16), and a runner is online when its status is idle or active.
func TestListRepoRunnersReadsTheArray(t *testing.T) {
	t.Parallel()
	rec := &recorder{body: []byte(`[{"id":1,"name":"runner-1","status":"offline"},{"id":2,"name":"runner-2","status":"idle"},{"id":3,"name":"runner-3","status":"active"}]`)}
	runners, err := newTestClient(t, rec).ListRepoRunners(context.Background(), "gastown", "gastown")
	require.NoError(t, err)
	require.Len(t, runners, 3)
	assert.Equal(t, "/api/v1/repos/gastown/gastown/actions/runners", rec.req.URL.Path)
	assert.Equal(t, "token test-token", rec.req.Header.Get("Authorization"))
	assert.Equal(t, []bool{false, true, true}, []bool{runners[0].Online(), runners[1].Online(), runners[2].Online()})
}

// TestListRepoRunnersUnknownStatusIsNotOnline: a status this code does not
// know is not evidence that CI can pick a run up.
func TestListRepoRunnersUnknownStatusIsNotOnline(t *testing.T) {
	t.Parallel()
	rec := &recorder{body: []byte(`[{"id":1,"name":"runner-1","status":"hibernating"}]`)}
	runners, err := newTestClient(t, rec).ListRepoRunners(context.Background(), "gastown", "gastown")
	require.NoError(t, err)
	require.Len(t, runners, 1)
	assert.False(t, runners[0].Online())
}

// TestListRepoRunnersNotFoundIsAnError: a repository the token cannot read is
// an error, so the probe reports a failure rather than an empty list.
func TestListRepoRunnersNotFoundIsAnError(t *testing.T) {
	t.Parallel()
	rec := &recorder{status: http.StatusNotFound, body: []byte(`{"message":"not found"}`)}
	_, err := newTestClient(t, rec).ListRepoRunners(context.Background(), "gastown", "gastown")
	var apiErr *APIError
	require.Error(t, err)
	assert.True(t, errors.As(err, &apiErr))
	assert.Equal(t, http.StatusNotFound, apiErr.StatusCode)
}
