package forgejo

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestListBranches_ReadsOnePageWithItsNamesAndCommits: the call is a GET of the
// repository's branches at the page and limit asked for, and each branch comes
// back with the full commit it points at, which the pair check reads.
func TestListBranches_ReadsOnePageWithItsNamesAndCommits(t *testing.T) {
	t.Parallel()
	sha := "0123456789abcdef0123456789abcdef01234567"
	rec := &recorder{body: []byte(`[{"name":"green/0123456789ab","commit":{"id":"` + sha + `"}}]`)}
	c := newTestClient(t, rec)

	got, err := c.ListBranches(context.Background(), "owner", "gastown", 2, 50)
	require.NoError(t, err)
	assert.Equal(t, http.MethodGet, rec.req.Method)
	assert.Equal(t, "/api/v1/repos/owner/gastown/branches", rec.req.URL.Path)
	assert.Equal(t, "2", rec.req.URL.Query().Get("page"))
	assert.Equal(t, "50", rec.req.URL.Query().Get("limit"))
	require.Len(t, got, 1)
	assert.Equal(t, "green/0123456789ab", got[0].Name)
	assert.Equal(t, sha, got[0].Commit.ID)
}

// TestListBranches_RefusesAServerError: a viewer that cannot read the
// repository is an error, not an empty page that would read as "no green refs".
func TestListBranches_RefusesAServerError(t *testing.T) {
	t.Parallel()
	rec := &recorder{status: http.StatusNotFound, body: []byte(`{"message":"Not Found"}`)}
	c := newTestClient(t, rec)

	_, err := c.ListBranches(context.Background(), "owner", "gastown", 1, 50)
	require.Error(t, err)
}

// TestGetBranch_ReadsOneBranchWithItsCommit: the call is a GET of the branch as
// one escaped path segment, and the branch comes back with the full commit it
// points at.
func TestGetBranch_ReadsOneBranchWithItsCommit(t *testing.T) {
	t.Parallel()
	sha := "0123456789abcdef0123456789abcdef01234567"
	rec := &recorder{body: []byte(`{"name":"crew/agent/gt-abc","commit":{"id":"` + sha + `"}}`)}
	c := newTestClient(t, rec)

	got, err := c.GetBranch(context.Background(), "owner", "gastown", "crew/agent/gt-abc")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, http.MethodGet, rec.req.Method)
	assert.Equal(t, "/api/v1/repos/owner/gastown/branches/crew%2Fagent%2Fgt-abc", rec.req.URL.EscapedPath())
	assert.Equal(t, "crew/agent/gt-abc", got.Name)
	assert.Equal(t, sha, got.Commit.ID)
}

// TestGetBranch_RefusesAMissingBranchAndAServerError: a branch that is not on
// the repository and a server that cannot answer are each an error with a nil
// branch, so a caller never reads either as a branch that exists.
func TestGetBranch_RefusesAMissingBranchAndAServerError(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			rec := &recorder{status: status, body: []byte(`{"message":"no"}`)}
			c := newTestClient(t, rec)

			got, err := c.GetBranch(context.Background(), "owner", "gastown", "crew/agent/gt-abc")
			require.Error(t, err)
			assert.Nil(t, got)
			var apiErr *APIError
			require.True(t, errors.As(err, &apiErr))
			assert.Equal(t, status, apiErr.StatusCode)
		})
	}
}
