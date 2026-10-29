package forgejo

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The probe's existence check reads the repo it names: the reply decodes, and
// the request addresses both the owner and the name, so a call for one owner
// never asks about another's repository of the same name (gt-tpg0p.86).
func TestRepositoryReturnsTheRepo(t *testing.T) {
	t.Parallel()
	rec := &recorder{body: []byte(`{
	  "id": 12,
	  "name": "gastown",
	  "full_name": "gastownhall/gastown",
	  "empty": false
	}`)}
	c := newTestClient(t, rec)

	got, err := c.Repository(context.Background(), "gastownhall", "gastown")
	require.NoError(t, err)
	assert.Equal(t, http.MethodGet, rec.req.Method)
	assert.Equal(t, "/api/v1/repos/gastownhall/gastown", rec.req.URL.Path)
	assert.Equal(t, "token test-token", rec.req.Header.Get("Authorization"))
	require.NotNil(t, got)
	assert.Equal(t, "gastownhall/gastown", got.FullName)
}

// A repository that is not on the instance answers 404, and the caller gets
// the package's *APIError so it can tell "absent" from a transport or auth
// failure rather than reading either as absent (gt-tpg0p.86).
func TestRepositoryNotFoundIsAnAPIError(t *testing.T) {
	t.Parallel()
	rec := &recorder{
		status: http.StatusNotFound,
		body:   []byte(`{"message":"repository does not exist [id: 0, uid: 0]"}`),
	}
	c := newTestClient(t, rec)

	got, err := c.Repository(context.Background(), "gastownhall", "absent")
	require.Error(t, err)
	assert.Nil(t, got)

	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusNotFound, apiErr.StatusCode)
	assert.Contains(t, apiErr.Error(), "repository does not exist")
	assert.NotContains(t, apiErr.Error(), "test-token")
}

// The other side of that distinction: a failure that is not a 404 keeps its
// own status, so the probe's "404 means absent" check does not read a server
// or auth failure as a missing repository and skip the run instead of failing
// it (gt-kj9hj).
func TestRepositoryOtherFailuresAreNotAbsent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		status int
		body   []byte
	}{
		{
			name:   "the instance failing the request",
			status: http.StatusInternalServerError,
			body:   []byte(`{"message":"boom"}`),
		},
		{
			name:   "a token the instance rejects",
			status: http.StatusForbidden,
			body:   []byte(`{"message":"token does not have access"}`),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := &recorder{status: tt.status, body: tt.body}
			c := newTestClient(t, rec)

			got, err := c.Repository(context.Background(), "gastownhall", "gastown")
			require.Error(t, err)
			assert.Nil(t, got)

			var apiErr *APIError
			require.ErrorAs(t, err, &apiErr)
			assert.Equal(t, tt.status, apiErr.StatusCode)
			assert.NotEqual(t, http.StatusNotFound, apiErr.StatusCode)
		})
	}
}

// A failure below HTTP — the instance unreachable — answers as an error with
// no status at all rather than as a 404, so it cannot be read as an absent
// repository either (gt-kj9hj).
func TestRepositoryTransportFailureIsNotAnAPIError(t *testing.T) {
	t.Parallel()
	down := errors.New("connection refused")
	c, err := NewClient("landing",
		WithToken("test-token"),
		WithBaseURL(testBase),
		WithHTTPClient(&http.Client{Transport: unreachableTransport{err: down}}),
	)
	require.NoError(t, err)

	got, err := c.Repository(context.Background(), "gastownhall", "gastown")
	require.Error(t, err)
	assert.Nil(t, got)
	assert.ErrorIs(t, err, down)

	var apiErr *APIError
	assert.False(t, errors.As(err, &apiErr))
}

// unreachableTransport fails every request the way an instance that is down
// does, without opening a socket.
type unreachableTransport struct{ err error }

func (tr unreachableTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, tr.err
}

// owner and name travel as escaped path segments, as every path this package
// builds does (repoPath), so a character that would otherwise end the segment
// stays inside it (gt-tpg0p.86).
func TestRepositoryEscapesPathSegments(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		owner string
		repo  string
		want  string
	}{
		{
			name:  "owner with a space",
			owner: "gas town",
			repo:  "gastown",
			want:  "/api/v1/repos/gas%20town/gastown",
		},
		{
			name:  "name with a slash",
			owner: "gastownhall",
			repo:  "gas/town",
			want:  "/api/v1/repos/gastownhall/gas%2Ftown",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := &recorder{body: []byte(`{"full_name":"gastownhall/gastown"}`)}
			c := newTestClient(t, rec)

			_, err := c.Repository(context.Background(), tt.owner, tt.repo)
			require.NoError(t, err)
			assert.Equal(t, tt.want, rec.req.URL.EscapedPath())
		})
	}
}
