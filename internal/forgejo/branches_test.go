package forgejo

import (
	"context"
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
