package forgejo

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCompareCommits_ReadsTheCommitCount: the call is a GET of the API's own
// base...head path, and the only field the landing path reads is the count of
// commits the head has that the base does not.
func TestCompareCommits_ReadsTheCommitCount(t *testing.T) {
	t.Parallel()
	rec := &recorder{body: []byte(`{"total_commits":2,"commits":[{"sha":"aa"},{"sha":"bb"}],"files":[]}`)}
	c := newTestClient(t, rec)

	got, err := c.CompareCommits(context.Background(), "owner", "fractals-nextjs", "3ad82c15dd", "23d2048323")
	require.NoError(t, err)
	assert.Equal(t, http.MethodGet, rec.req.Method)
	assert.Equal(t, "/api/v1/repos/owner/fractals-nextjs/compare/3ad82c15dd...23d2048323", rec.req.URL.Path)
	assert.Equal(t, 2, got.TotalCommits)
}

// TestCompareCommits_EqualCommitsAreZero: the same commit on both sides is the
// answer the ancestry question is read from, so it has to come back as zero
// rather than as a decode failure.
func TestCompareCommits_EqualCommitsAreZero(t *testing.T) {
	t.Parallel()
	rec := &recorder{body: []byte(`{"total_commits":0,"commits":[],"files":[]}`)}
	c := newTestClient(t, rec)

	got, err := c.CompareCommits(context.Background(), "owner", "fractals-nextjs", "aa", "aa")
	require.NoError(t, err)
	assert.Equal(t, 0, got.TotalCommits)
}

// TestCompareCommits_RefusesABodyWithoutTheCount: an empty 200 body, a 204,
// `{}`, a body with only other fields and a null count each leave the count
// unread. The zero they would decode to is the answer "the head is already
// contained", so each has to be an error instead.
func TestCompareCommits_RefusesABodyWithoutTheCount(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"empty 200 body", 0, ""},
		{"204", http.StatusNoContent, ""},
		{"empty object", 0, `{}`},
		{"other fields only", 0, `{"commits":[]}`},
		{"null count", 0, `{"total_commits":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := &recorder{status: tc.status, body: []byte(tc.body)}
			c := newTestClient(t, rec)

			_, err := c.CompareCommits(context.Background(), "owner", "fractals-nextjs", "aa", "bb")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "total_commits")
		})
	}
}

// TestCompareCommits_ExplicitZeroIsTheAnswer: a body that does carry zero is a
// success with the count, because zero is the ancestry answer the caller reads.
func TestCompareCommits_ExplicitZeroIsTheAnswer(t *testing.T) {
	t.Parallel()
	rec := &recorder{body: []byte(`{"total_commits":0}`)}
	c := newTestClient(t, rec)

	got, err := c.CompareCommits(context.Background(), "owner", "fractals-nextjs", "aa", "bb")
	require.NoError(t, err)
	assert.Equal(t, 0, got.TotalCommits)
}

// TestCompareCommits_RefusesAServerError: a viewer that cannot read the
// repository is an error, not an empty answer that would read as "no commits".
func TestCompareCommits_RefusesAServerError(t *testing.T) {
	t.Parallel()
	rec := &recorder{status: http.StatusNotFound, body: []byte(`{"message":"Not Found"}`)}
	c := newTestClient(t, rec)

	_, err := c.CompareCommits(context.Background(), "owner", "devops", "aa", "bb")
	require.Error(t, err)
}
