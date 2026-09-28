package bitbucket

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testBase is the API base the test clients address; requests never leave
// the process.
const testBase = "http://bitbucket.test"

// handlerTransport serves each request in-process with h, so tests exercise
// the client's real request building and response handling without a socket.
type handlerTransport struct{ h http.Handler }

func (tr handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	tr.h.ServeHTTP(rec, r)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

// newTestClient creates a Client whose requests are answered by handler.
func newTestClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	c, err := NewClient(
		WithToken("test-token"),
		WithHTTPClient(&http.Client{Transport: handlerTransport{handler}}),
		WithRESTBase(testBase),
	)
	require.NoError(t, err)
	return c
}

// envOf returns a getenv over a fixed environment.
func envOf(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestNewClient_RequiresToken(t *testing.T) {
	t.Parallel()
	_, err := newClient(envOf(nil))
	assert.ErrorContains(t, err, "BITBUCKET_TOKEN is required")
}

func TestNewClient_FromEnv(t *testing.T) {
	t.Parallel()
	c, err := newClient(envOf(map[string]string{"BITBUCKET_TOKEN": "env-token"}))
	require.NoError(t, err)
	assert.Equal(t, "env-token", c.token)
	assert.Equal(t, http.DefaultClient, c.httpClient)
	assert.Equal(t, defaultRESTBase, c.restBase)
}

func TestNewClient_WithTokenOverridesEnv(t *testing.T) {
	t.Parallel()
	c, err := newClient(envOf(map[string]string{"BITBUCKET_TOKEN": "env-token"}), WithToken("explicit"))
	require.NoError(t, err)
	assert.Equal(t, "explicit", c.token)

	// The exported constructor takes the same options.
	c, err = NewClient(WithToken("explicit"))
	require.NoError(t, err)
	assert.Equal(t, "explicit", c.token)
}

func TestCreateDraftPR(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /repositories/myws/myrepo/pullrequests", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))

		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, true, body["draft"])
		assert.Equal(t, "Add feature", body["title"])

		source := body["source"].(map[string]any)
		srcBranch := source["branch"].(map[string]any)
		assert.Equal(t, "feat-branch", srcBranch["name"])

		dest := body["destination"].(map[string]any)
		destBranch := dest["branch"].(map[string]any)
		assert.Equal(t, "main", destBranch["name"])

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"id": 42,
			"links": map[string]any{
				"html": map[string]any{
					"href": "https://bitbucket.org/myws/myrepo/pull-requests/42",
				},
			},
		})
	})

	c := newTestClient(t, mux)
	result, err := c.CreateDraftPR(t.Context(), "myws", "myrepo", "feat-branch", "main", "Add feature", "Description")
	require.NoError(t, err)
	assert.Equal(t, 42, result.ID)
	assert.Equal(t, "https://bitbucket.org/myws/myrepo/pull-requests/42", result.URL)
}

func TestUpdatePRDescription(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /repositories/myws/myrepo/pullrequests/42", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "Updated body", body["description"])
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	})

	c := newTestClient(t, mux)
	err := c.UpdatePRDescription(t.Context(), "myws", "myrepo", 42, "Updated body")
	require.NoError(t, err)
}

func TestGetPRApprovalStatus_Approved(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repositories/myws/myrepo/pullrequests/42", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"participants": []map[string]any{
				{"role": "REVIEWER", "approved": true, "state": "approved", "user": map[string]any{"display_name": "alice"}},
				{"role": "REVIEWER", "approved": true, "state": "approved", "user": map[string]any{"display_name": "bob"}},
			},
		})
	})

	c := newTestClient(t, mux)
	state, err := c.GetPRApprovalStatus(t.Context(), "myws", "myrepo", 42)
	require.NoError(t, err)
	assert.Equal(t, ReviewApproved, state)
}

func TestGetPRApprovalStatus_ChangesRequested(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repositories/myws/myrepo/pullrequests/42", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"participants": []map[string]any{
				{"role": "REVIEWER", "approved": true, "state": "approved", "user": map[string]any{"display_name": "alice"}},
				{"role": "REVIEWER", "approved": false, "state": "changes_requested", "user": map[string]any{"display_name": "bob"}},
			},
		})
	})

	c := newTestClient(t, mux)
	state, err := c.GetPRApprovalStatus(t.Context(), "myws", "myrepo", 42)
	require.NoError(t, err)
	assert.Equal(t, ReviewChangesRequired, state)
}

func TestGetPRApprovalStatus_NoParticipants(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repositories/myws/myrepo/pullrequests/42", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"participants": []map[string]any{},
		})
	})

	c := newTestClient(t, mux)
	state, err := c.GetPRApprovalStatus(t.Context(), "myws", "myrepo", 42)
	require.NoError(t, err)
	assert.Equal(t, ReviewPending, state)
}

func TestGetPRApprovalStatus_NonReviewerIgnored(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repositories/myws/myrepo/pullrequests/42", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"participants": []map[string]any{
				{"role": "AUTHOR", "approved": false, "state": "", "user": map[string]any{"display_name": "author"}},
				{"role": "PARTICIPANT", "approved": true, "state": "approved", "user": map[string]any{"display_name": "viewer"}},
			},
		})
	})

	c := newTestClient(t, mux)
	state, err := c.GetPRApprovalStatus(t.Context(), "myws", "myrepo", 42)
	require.NoError(t, err)
	assert.Equal(t, ReviewPending, state)
}

func TestGetPRComments(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repositories/myws/myrepo/pullrequests/42/comments", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"values": []map[string]any{
				{
					"id":         101,
					"content":    map[string]any{"raw": "Fix this"},
					"inline":     map[string]any{"path": "main.go", "to": 10},
					"created_on": "2026-01-01T00:00:00+00:00",
					"user":       map[string]any{"display_name": "alice"},
					"links":      map[string]any{"html": map[string]any{"href": "https://bitbucket.org/myws/myrepo/pull-requests/42#comment-101"}},
				},
			},
		})
	})

	c := newTestClient(t, mux)
	comments, err := c.GetPRComments(t.Context(), "myws", "myrepo", 42)
	require.NoError(t, err)
	require.Len(t, comments, 1)
	assert.Equal(t, int64(101), comments[0].ID)
	assert.Equal(t, "Fix this", comments[0].Body)
	assert.Equal(t, "alice", comments[0].User)
	assert.Equal(t, "main.go", comments[0].Path)
	assert.Equal(t, 10, comments[0].Line)
}

func TestReplyToPRComment(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /repositories/myws/myrepo/pullrequests/42/comments", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		content := body["content"].(map[string]any)
		assert.Equal(t, "Thanks, fixed!", content["raw"])
		parent := body["parent"].(map[string]any)
		assert.Equal(t, float64(101), parent["id"])
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{}`))
	})

	c := newTestClient(t, mux)
	err := c.ReplyToPRComment(t.Context(), "myws", "myrepo", 42, 101, "Thanks, fixed!")
	require.NoError(t, err)
}

func TestMergePR(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /repositories/myws/myrepo/pullrequests/42/merge", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		assert.Equal(t, "squash", body["merge_strategy"])
		assert.Equal(t, false, body["close_source_branch"])
		json.NewEncoder(w).Encode(map[string]any{"state": "MERGED"})
	})

	c := newTestClient(t, mux)
	err := c.MergePR(t.Context(), "myws", "myrepo", 42, "squash")
	require.NoError(t, err)
}

func TestGetRepoMergeStrategies(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repositories/myws/myrepo/branching-model", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"development": map[string]any{
				"merge_strategy": "fast_forward",
			},
		})
	})

	c := newTestClient(t, mux)
	strategy, err := c.GetRepoMergeStrategies(t.Context(), "myws", "myrepo")
	require.NoError(t, err)
	assert.Equal(t, "fast_forward", strategy)
}

func TestGetRepoMergeStrategies_DefaultsToSquash(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repositories/myws/myrepo/branching-model", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"message":"Not Found"}}`))
	})

	c := newTestClient(t, mux)
	strategy, err := c.GetRepoMergeStrategies(t.Context(), "myws", "myrepo")
	require.NoError(t, err)
	assert.Equal(t, "squash", strategy)
}

func TestAPIError(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repositories/myws/myrepo/pullrequests/999", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"message":"Not Found"}}`))
	})

	c := newTestClient(t, mux)
	_, err := c.GetPRApprovalStatus(t.Context(), "myws", "myrepo", 999)
	require.Error(t, err)

	var apiErr *APIError
	assert.ErrorAs(t, err, &apiErr)
	assert.Equal(t, 404, apiErr.StatusCode)
}
