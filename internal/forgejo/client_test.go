package forgejo

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testBase is the API root the test clients address; requests never leave the
// process.
const testBase = "http://forgejo.test/api/v1"

// handlerTransport serves each request in-process with h, so the tests
// exercise the client's real request building and response handling without a
// socket.
type handlerTransport struct{ h http.Handler }

func (tr handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	tr.h.ServeHTTP(rec, r)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

// recorder answers every request with status and body, defaulting to 200, and
// keeps the request and its raw body for assertions.
type recorder struct {
	status int
	body   []byte
	req    *http.Request
	raw    []byte
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.req = req
	if req.Body != nil {
		if b, err := io.ReadAll(req.Body); err == nil {
			r.raw = b
		}
	}
	if r.status != 0 {
		w.WriteHeader(r.status)
	}
	_, _ = w.Write(r.body)
}

// newTestClient creates a Client whose requests recorder answers.
func newTestClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	c, err := NewClient("landing",
		WithToken("test-token"),
		WithBaseURL(testBase),
		WithHTTPClient(&http.Client{Transport: handlerTransport{h}}),
	)
	require.NoError(t, err)
	return c
}

// golden reads a recorded Forgejo response from testdata.
func golden(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return data
}

func TestNewClient_Defaults(t *testing.T) {
	t.Parallel()
	c, err := NewClient("landing", WithToken("t"))
	require.NoError(t, err)
	assert.Equal(t, defaultBaseURL, c.baseURL)
	assert.Equal(t, http.DefaultClient, c.httpClient)
	assert.Equal(t, "t", c.token)
}

func TestNewClient_RequiresToken(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "forgejo-landing.env")
	_, err := NewClient("landing", WithTokenFile(missing))
	assert.ErrorContains(t, err, "token file")
}

func TestNewClient_FromTokenFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "forgejo-landing.env")
	require.NoError(t, os.WriteFile(path, []byte("FORGEJO_TOKEN=file-token\n"), 0o600))
	c, err := NewClient("landing", WithTokenFile(path))
	require.NoError(t, err)
	assert.Equal(t, "file-token", c.token)
}

func TestTokenPath(t *testing.T) {
	t.Parallel()
	home := func() (string, error) { return "/home/agent", nil }
	tests := []struct {
		name    string
		env     map[string]string
		role    string
		want    string
		errPart string
	}{
		{name: "config home", role: "landing", want: "/home/agent/.config/gt/forgejo-landing.env"},
		{name: "xdg config home", env: map[string]string{"XDG_CONFIG_HOME": "/xdg"}, role: "polecat", want: "/xdg/gt/forgejo-polecat.env"},
		{name: "role with slash", role: "gastown/polecats/x", errPart: "plain name"},
		{name: "empty role", role: "", errPart: "plain name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := tokenPath(envOf(tt.env), home, tt.role)
			if tt.errPart != "" {
				assert.ErrorContains(t, err, tt.errPart)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestReadTokenFile_ParsesEnvLine(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "plain", body: "FORGEJO_TOKEN=plain\n", want: "plain"},
		{name: "export and quotes", body: "# landing bot\nexport FORGEJO_TOKEN=\"quoted value\"\n", want: "quoted value"},
		{name: "single quotes", body: "FORGEJO_TOKEN='single'\n", want: "single"},
		{name: "other keys around", body: "OTHER=1\nFORGEJO_TOKEN=wanted\nMORE=2\n", want: "wanted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(dir, tt.name+".env")
			require.NoError(t, os.WriteFile(path, []byte(tt.body), 0o600))
			token, err := ReadTokenFile(path)
			require.NoError(t, err)
			assert.Equal(t, tt.want, token)
		})
	}
}

func TestReadTokenFile_RefusesGroupOrOtherReadable(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "forgejo-landing.env")
	require.NoError(t, os.WriteFile(path, []byte("FORGEJO_TOKEN=leaky\n"), 0o644))
	_, err := ReadTokenFile(path)
	assert.ErrorContains(t, err, "must be 600")
}

func TestReadTokenFile_RequiresTheKeyAndAFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "forgejo-landing.env")
	require.NoError(t, os.WriteFile(path, []byte("OTHER=1\n"), 0o600))
	_, err := ReadTokenFile(path)
	assert.ErrorContains(t, err, EnvKey)

	_, err = ReadTokenFile(dir)
	assert.ErrorContains(t, err, "not a regular file")
}

func TestPostStatus(t *testing.T) {
	t.Parallel()
	rec := &recorder{body: golden(t, "commit_status.json")}
	c := newTestClient(t, rec)
	want := StatusRequest{
		State:       StateSuccess,
		Context:     "ci / gate (push)",
		Description: "make gate passed",
		TargetURL:   "https://forgejo.test/gastownhall/gastown/actions/runs/91",
	}

	got, err := c.PostStatus(context.Background(), "gastownhall", "gastown", "cafe1234", want)
	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, rec.req.Method)
	assert.Equal(t, "/api/v1/repos/gastownhall/gastown/statuses/cafe1234", rec.req.URL.Path)
	assert.Equal(t, "token test-token", rec.req.Header.Get("Authorization"))

	var sent StatusRequest
	require.NoError(t, json.Unmarshal(rec.raw, &sent))
	assert.Equal(t, want, sent)

	require.NotNil(t, got)
	assert.Equal(t, int64(4211), got.ID)
	assert.Equal(t, StateSuccess, got.Status)
	assert.Equal(t, "ci / gate (push)", got.Context)
}

func TestCombinedStatus(t *testing.T) {
	t.Parallel()
	rec := &recorder{body: golden(t, "combined_status.json")}
	c := newTestClient(t, rec)

	got, err := c.CombinedStatus(context.Background(), "gastownhall", "gastown", "cafe1234")
	require.NoError(t, err)
	assert.Equal(t, http.MethodGet, rec.req.Method)
	assert.Equal(t, "/api/v1/repos/gastownhall/gastown/commits/cafe1234/status", rec.req.URL.Path)
	assert.Equal(t, StateSuccess, got.State)

	gate := got.StatusFor("ci / gate (push)")
	require.NotNil(t, gate)
	assert.Equal(t, StateSuccess, gate.Status)
	assert.Nil(t, gate.Creator, "Actions posts the gate status, so it has no user creator")

	review := got.StatusFor("om / review")
	require.NotNil(t, review)
	require.NotNil(t, review.Creator)
	assert.Equal(t, "landing-bot", review.Creator.Login)

	assert.Nil(t, got.StatusFor("no / such / context"))
}

func TestCreatePull(t *testing.T) {
	t.Parallel()
	rec := &recorder{body: golden(t, "pull_request.json")}
	c := newTestClient(t, rec)

	got, err := c.CreatePull(context.Background(), "gastownhall", "gastown", CreatePullRequestOption{
		Title: "land/gt-fn9e6.5 -> main",
		Head:  "land/gt-fn9e6.5",
		Base:  "main",
	})
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/repos/gastownhall/gastown/pulls", rec.req.URL.Path)

	var sent CreatePullRequestOption
	require.NoError(t, json.Unmarshal(rec.raw, &sent))
	assert.Equal(t, "land/gt-fn9e6.5", sent.Head)
	assert.Equal(t, "main", sent.Base)

	require.NotNil(t, got)
	assert.Equal(t, int64(7), got.Number)
	assert.Equal(t, "land/gt-fn9e6.5", got.Head.Ref)
	assert.Equal(t, "cafe1234567890", got.Head.SHA)
}

func TestGetPull(t *testing.T) {
	t.Parallel()
	rec := &recorder{body: golden(t, "pull_request.json")}
	c := newTestClient(t, rec)

	got, err := c.GetPull(context.Background(), "gastownhall", "gastown", 7)
	require.NoError(t, err)
	assert.Equal(t, http.MethodGet, rec.req.Method)
	assert.Equal(t, "/api/v1/repos/gastownhall/gastown/pulls/7", rec.req.URL.Path)
	assert.Equal(t, int64(7), got.Number)
	assert.True(t, got.Mergeable)
}

func TestOpenPulls(t *testing.T) {
	t.Parallel()
	rec := &recorder{body: []byte(`[{"number":3,"head":{"ref":"land/gt-abc"},"base":{"ref":"main"}}]`)}
	c := newTestClient(t, rec)

	got, err := c.OpenPulls(context.Background(), "gastownhall", "gastown")
	require.NoError(t, err)
	assert.Equal(t, http.MethodGet, rec.req.Method)
	assert.Equal(t, "/api/v1/repos/gastownhall/gastown/pulls", rec.req.URL.Path)
	assert.Equal(t, "open", rec.req.URL.Query().Get("state"))
	require.Len(t, got, 1)
	assert.Equal(t, int64(3), got[0].Number)
	assert.Equal(t, "land/gt-abc", got[0].Head.Ref)
}

func TestMergePull_SendsHeadCommitID(t *testing.T) {
	t.Parallel()
	rec := &recorder{body: []byte("{}")}
	c := newTestClient(t, rec)

	err := c.MergePull(context.Background(), "gastownhall", "gastown", 7, MergePullRequestOption{
		Style:                  MergeStyleMerge,
		HeadCommitID:           "cafe1234567890",
		DeleteBranchAfterMerge: true,
	})
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/repos/gastownhall/gastown/pulls/7/merge", rec.req.URL.Path)

	var sent map[string]any
	require.NoError(t, json.Unmarshal(rec.raw, &sent))
	assert.Equal(t, "merge", sent["Do"])
	assert.Equal(t, "cafe1234567890", sent["head_commit_id"])
	assert.Equal(t, true, sent["delete_branch_after_merge"])
}

func TestMergePull_ReportsConflict(t *testing.T) {
	t.Parallel()
	rec := &recorder{status: http.StatusConflict, body: []byte(`{"message":"head out of date"}`)}
	c := newTestClient(t, rec)

	err := c.MergePull(context.Background(), "gastownhall", "gastown", 7, MergePullRequestOption{Style: MergeStyleMerge})
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusConflict, apiErr.StatusCode)
	assert.True(t, apiErr.IsConflict())
	assert.Contains(t, apiErr.Error(), "head out of date")
	assert.NotContains(t, apiErr.Error(), "test-token")
}

func TestDeleteBranch_EscapesTheName(t *testing.T) {
	t.Parallel()
	rec := &recorder{status: http.StatusNoContent}
	c := newTestClient(t, rec)

	require.NoError(t, c.DeleteBranch(context.Background(), "gastownhall", "gastown", "land/gt-fn9e6.21"))
	assert.Equal(t, http.MethodDelete, rec.req.Method)
	// The branch route is a wildcard the server reads without unescaping, so
	// the "/" must arrive as %2F (verified on Forgejo 16.0.5).
	assert.Equal(t, "/api/v1/repos/gastownhall/gastown/branches/land%2Fgt-fn9e6.21", rec.req.URL.EscapedPath())
}

func TestDeleteBranch_ReportsFailure(t *testing.T) {
	t.Parallel()
	rec := &recorder{status: http.StatusForbidden, body: []byte(`{"message":"protected branch"}`)}
	c := newTestClient(t, rec)

	err := c.DeleteBranch(context.Background(), "gastownhall", "gastown", "land/gt-abc")
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusForbidden, apiErr.StatusCode)
	assert.Contains(t, apiErr.Error(), "protected branch")
}

func TestListRuns_Filters(t *testing.T) {
	t.Parallel()
	rec := &recorder{body: golden(t, "run_list.json")}
	c := newTestClient(t, rec)

	got, err := c.ListRuns(context.Background(), "gastownhall", "gastown", RunFilter{
		Event:      []string{"push"},
		Status:     []string{"failure"},
		HeadSHA:    "cafe1234567890",
		Ref:        "land/gt-fn9e6.5",
		WorkflowID: "staging.yml",
		Page:       2,
		Limit:      30,
	})
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/repos/gastownhall/gastown/actions/runs", rec.req.URL.Path)
	q := rec.req.URL.Query()
	assert.Equal(t, []string{"push"}, q["event"])
	assert.Equal(t, []string{"failure"}, q["status"])
	assert.Equal(t, "cafe1234567890", q.Get("head_sha"))
	assert.Equal(t, "land/gt-fn9e6.5", q.Get("ref"))
	assert.Equal(t, "staging.yml", q.Get("workflow_id"))
	assert.Equal(t, "2", q.Get("page"))
	assert.Equal(t, "30", q.Get("limit"))

	assert.Equal(t, 1, got.TotalCount)
	require.Len(t, got.Runs, 1)
	assert.Equal(t, int64(91), got.Runs[0].ID)
	assert.Equal(t, "failure", got.Runs[0].Status)
	assert.Equal(t, "land/gt-fn9e6.5", got.Runs[0].PrettyRef)
}

func TestListRunJobs(t *testing.T) {
	t.Parallel()
	rec := &recorder{body: golden(t, "run_jobs.json")}
	c := newTestClient(t, rec)

	got, err := c.ListRunJobs(context.Background(), "gastownhall", "gastown", 91)
	require.NoError(t, err)
	assert.Equal(t, "/api/v1/repos/gastownhall/gastown/actions/runs/91/jobs", rec.req.URL.Path)
	require.Len(t, got, 1)
	assert.Equal(t, int64(311), got[0].ID)
	assert.Equal(t, "gate", got[0].Name)
	assert.Equal(t, "failure", got[0].Status)
}

func TestJobLogs(t *testing.T) {
	t.Parallel()
	rec := &recorder{body: []byte("line one\nline two\nFAIL: TestThing\n")}
	c := newTestClient(t, rec)

	got, err := c.JobLogs(context.Background(), "gastownhall", "gastown", 311)
	require.NoError(t, err)
	assert.Equal(t, http.MethodGet, rec.req.Method)
	assert.Equal(t, "/api/v1/repos/gastownhall/gastown/actions/jobs/311/logs", rec.req.URL.Path)
	assert.Equal(t, "text/plain", rec.req.Header.Get("Accept"))
	assert.Empty(t, rec.req.Header.Get("Range"))
	assert.Equal(t, "line one\nline two\nFAIL: TestThing\n", got)
}

func TestJobLogsTail(t *testing.T) {
	t.Parallel()
	t.Run("asks for the last bytes", func(t *testing.T) {
		t.Parallel()
		rec := &recorder{status: http.StatusPartialContent, body: []byte("FAIL: TestThing\n")}
		c := newTestClient(t, rec)

		got, err := c.JobLogsTail(context.Background(), "gastownhall", "gastown", 311, 2048)
		require.NoError(t, err)
		assert.Equal(t, "bytes=-2048", rec.req.Header.Get("Range"))
		assert.Equal(t, "FAIL: TestThing\n", got)
	})
	t.Run("no limit fetches the whole log", func(t *testing.T) {
		t.Parallel()
		rec := &recorder{body: []byte("everything\n")}
		c := newTestClient(t, rec)

		got, err := c.JobLogsTail(context.Background(), "gastownhall", "gastown", 311, 0)
		require.NoError(t, err)
		assert.Empty(t, rec.req.Header.Get("Range"))
		assert.Equal(t, "everything\n", got)
	})
}

func TestListPushMirrors(t *testing.T) {
	t.Parallel()
	rec := &recorder{body: golden(t, "push_mirrors.json")}
	c := newTestClient(t, rec)

	got, err := c.ListPushMirrors(context.Background(), "gastownhall", "gastown")
	require.NoError(t, err)
	assert.Equal(t, http.MethodGet, rec.req.Method)
	assert.Equal(t, "/api/v1/repos/gastownhall/gastown/push_mirrors", rec.req.URL.Path)
	require.Len(t, got, 1)
	assert.Equal(t, "main", got[0].BranchFilter)
	assert.True(t, got[0].SyncOnCommit)
	assert.Empty(t, got[0].LastError)
	assert.Equal(t, time.Date(2026, 10, 4, 19, 2, 0, 0, time.UTC), got[0].LastUpdate)
}

func TestCreatePushMirror(t *testing.T) {
	t.Parallel()
	rec := &recorder{body: golden(t, "push_mirror.json")}
	c := newTestClient(t, rec)

	got, err := c.CreatePushMirror(context.Background(), "gastownhall", "gastown", CreatePushMirrorOption{
		RemoteAddress: "git@github.com:gastownhall/gastown.git",
		UseSSH:        true,
		BranchFilter:  "main",
		SyncOnCommit:  true,
		Interval:      "10m0s",
	})
	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, rec.req.Method)
	assert.Equal(t, "/api/v1/repos/gastownhall/gastown/push_mirrors", rec.req.URL.Path)

	var sent CreatePushMirrorOption
	require.NoError(t, json.Unmarshal(rec.raw, &sent))
	assert.Equal(t, "git@github.com:gastownhall/gastown.git", sent.RemoteAddress)
	assert.True(t, sent.UseSSH)
	assert.Equal(t, "main", sent.BranchFilter)

	require.NotNil(t, got)
	assert.Equal(t, "github", got.RemoteName)
	assert.Contains(t, got.PublicKey, "ssh-ed25519")
}

func TestCall_ServerError(t *testing.T) {
	t.Parallel()
	rec := &recorder{status: http.StatusInternalServerError, body: []byte(`{"message":"boom"}`)}
	c := newTestClient(t, rec)

	_, err := c.CombinedStatus(context.Background(), "gastownhall", "gastown", "cafe1234")
	var apiErr *APIError
	require.ErrorAs(t, err, &apiErr)
	assert.Equal(t, http.StatusInternalServerError, apiErr.StatusCode)
	assert.False(t, apiErr.IsConflict())
	assert.Contains(t, apiErr.Error(), "boom")
}

// envOf returns a getenv over a fixed environment.
func envOf(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}
