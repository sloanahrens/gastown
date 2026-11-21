package forgejo

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestListRefs_ReadsOneCallWithNamesAndShas: the call is one GET of the refs
// route under the prefix, with no page or limit, and each ref comes back named
// without refs/heads/ and with the commit SHA of its object.
func TestListRefs_ReadsOneCallWithNamesAndShas(t *testing.T) {
	t.Parallel()
	sha := "0123456789abcdef0123456789abcdef01234567"
	rec := &recorder{body: []byte(`[{"ref":"refs/heads/green/0123456789ab","object":{"sha":"` + sha + `","type":"commit"}}]`)}
	c := newTestClient(t, rec)

	got, err := c.ListRefs(context.Background(), "owner", "gastown", "green/")
	require.NoError(t, err)
	assert.Equal(t, http.MethodGet, rec.req.Method)
	assert.Equal(t, "/api/v1/repos/owner/gastown/git/refs/heads/green/", rec.req.URL.Path)
	assert.Empty(t, rec.req.URL.RawQuery)
	require.Len(t, got, 1)
	assert.Equal(t, "green/0123456789ab", got[0].Name)
	assert.Equal(t, sha, got[0].SHA)
}

// TestListRefs_DropsAnElementOutsideThePrefix: the route matches by string
// prefix, so green2/x comes back for green/ and must not reach the caller.
func TestListRefs_DropsAnElementOutsideThePrefix(t *testing.T) {
	t.Parallel()
	sha := "0123456789abcdef0123456789abcdef01234567"
	rec := &recorder{body: []byte(`[` +
		`{"ref":"refs/heads/green/0123456789ab","object":{"sha":"` + sha + `","type":"commit"}},` +
		`{"ref":"refs/heads/green2/x","object":{"sha":"` + sha + `","type":"commit"}}]`)}
	c := newTestClient(t, rec)

	got, err := c.ListRefs(context.Background(), "owner", "gastown", "green/")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "green/0123456789ab", got[0].Name)
}

// TestListRefs_RefusesATagObjectAndAServerError: a tag object is an error naming
// its ref, and a 404 or a 500 is an error with a nil list, never an empty one
// that would read as "no green refs".
func TestListRefs_RefusesATagObjectAndAServerError(t *testing.T) {
	t.Parallel()
	sha := "0123456789abcdef0123456789abcdef01234567"

	t.Run("tag object names its ref", func(t *testing.T) {
		t.Parallel()
		rec := &recorder{body: []byte(`[{"ref":"refs/heads/green/0123456789ab","object":{"sha":"` + sha + `","type":"tag"}}]`)}
		c := newTestClient(t, rec)

		got, err := c.ListRefs(context.Background(), "owner", "gastown", "green/")
		require.Error(t, err)
		assert.Nil(t, got)
		assert.Contains(t, err.Error(), "green/0123456789ab")
	})

	for _, status := range []int{http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			rec := &recorder{status: status, body: []byte(`{"message":"no"}`)}
			c := newTestClient(t, rec)

			got, err := c.ListRefs(context.Background(), "owner", "gastown", "green/")
			require.Error(t, err)
			assert.Nil(t, got)
			var apiErr *APIError
			require.True(t, errors.As(err, &apiErr))
			assert.Equal(t, status, apiErr.StatusCode)
		})
	}
}

// TestListRefs_RefusesAnEmptyPrefix: an empty prefix would ask for and keep
// every branch, so it is refused before any request is sent.
func TestListRefs_RefusesAnEmptyPrefix(t *testing.T) {
	t.Parallel()
	rec := &recorder{body: []byte(`[]`)}
	c := newTestClient(t, rec)

	got, err := c.ListRefs(context.Background(), "owner", "gastown", "")
	require.Error(t, err)
	assert.Nil(t, got)
	assert.Nil(t, rec.req)
}
