package dashboard

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// forgejoFeedJSON is a two-event repo feed in the shape Forgejo 16.0.5 serves:
// newest first, ref_name a full ref, and content a JSON blob on the push row
// only.
const forgejoFeedJSON = `[
  {"id":2,"op_type":"commit_repo","created":"2026-10-04T23:42:12Z","ref_name":"refs/heads/main",
   "act_user":{"id":1,"login":"bot-polecat"},"repo":{"full_name":"sloan/beads"},
   "content":"{\"Commits\":[],\"HeadCommit\":{\"Message\":\"fix: the thing (gt-1)\"}}"},
  {"id":1,"op_type":"delete_branch","created":"2026-10-04T22:42:12Z","ref_name":"refs/heads/land/gt-1",
   "act_user":{"id":1,"login":"bot-landing"},"repo":{"full_name":"sloan/beads"},"content":""}
]`

// forgejoStub is a stand-in Forgejo that serves the two repos and one feed
// until told to fail, recording what it was asked for.
type forgejoStub struct {
	mu      sync.Mutex
	fail    int // the status every request answers once set
	paths   []string
	headers []string
}

func (s *forgejoStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paths = append(s.paths, r.URL.Path)
	s.headers = append(s.headers, r.Header.Get("Authorization"))
	if s.fail != 0 {
		w.WriteHeader(s.fail)
		_, _ = io.WriteString(w, `{"message":"boom"}`)
		return
	}
	switch r.URL.Path {
	case "/api/v1/user/repos":
		_, _ = io.WriteString(w, `[{"full_name":"sloan/beads"}]`)
	case "/api/v1/repos/sloan/beads/activities/feeds":
		_, _ = io.WriteString(w, forgejoFeedJSON)
	default:
		http.NotFound(w, r)
	}
}

func (s *forgejoStub) setFail(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = status
}

func (s *forgejoStub) asked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

// stubTransport answers each request in-process with the stub, so the reader's
// real request building and response handling run without a socket (unit tests
// may not open one, testpolicy rule no-network).
type stubTransport struct{ h http.Handler }

func (tr stubTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	tr.h.ServeHTTP(rec, r)
	resp := rec.Result()
	resp.Request = r
	return resp, nil
}

// newForgejoTestReader wires a reader to a stub Forgejo.
func newForgejoTestReader(t *testing.T, stub *forgejoStub, repos []string) *ForgejoReader {
	t.Helper()
	client, err := forgejo.NewClient("viewer",
		forgejo.WithToken("test-token"),
		forgejo.WithBaseURL("http://forgejo.test/api/v1"),
		forgejo.WithHTTPClient(&http.Client{Transport: stubTransport{stub}}),
	)
	require.NoError(t, err)
	return NewForgejoReader(client, repos)
}

func TestMergeForgejoActivitiesKeepsTheNewestAcrossReposOldestFirst(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	var acts []forgejo.Activity
	for i := 0; i < 40; i++ {
		repo := "sloan/beads"
		if i%2 == 0 {
			repo = "sloan/mango"
		}
		acts = append(acts, forgejo.Activity{
			OpType:  "commit_repo",
			RefName: "refs/heads/main",
			Created: base.Add(time.Duration(i) * time.Minute),
			ActUser: &forgejo.User{Login: "sloan"},
			Repo:    &forgejo.Repository{FullName: repo},
			Content: `{"HeadCommit":{"Message":"commit ` + strconv.Itoa(i) + `"}}`,
		})
	}
	feed := MergeForgejoActivities(acts)
	require.Len(t, feed.Events, forgejoKept)
	assert.Equal(t, "pushed main: commit 10", feed.Events[0].Summary, "the oldest kept event comes first")
	assert.Equal(t, "pushed main: commit 39", feed.Events[forgejoKept-1].Summary, "the newest event is last, where the panel follows")
	for i := 1; i < len(feed.Events); i++ {
		assert.False(t, feed.Events[i].At.Before(feed.Events[i-1].At), "events must run oldest to newest")
	}
	assert.Equal(t, "sloan/mango", feed.Events[0].Repo)
	assert.Equal(t, "sloan", feed.Events[0].Actor)
}

// An empty read must still be an empty list, never null: the page would draw
// nothing for null rather than its "no activity" line.
func TestMergeForgejoActivitiesWithoutAnyEvent(t *testing.T) {
	t.Parallel()
	feed := MergeForgejoActivities(nil)
	require.NotNil(t, feed.Events)
	assert.Empty(t, feed.Events)
}

func TestForgejoActivitySummary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		act  forgejo.Activity
		want string
	}{
		{"push", forgejo.Activity{OpType: "commit_repo", RefName: "refs/heads/main", Content: `{"HeadCommit":{"Message":"fix: it (gt-1)"}}`}, "pushed main: fix: it (gt-1)"},
		{"push with no blob", forgejo.Activity{OpType: "commit_repo", RefName: "refs/heads/main"}, "pushed main"},
		{"push with no ref either", forgejo.Activity{OpType: "commit_repo"}, "pushed"},
		{"branch created", forgejo.Activity{OpType: "create_branch", RefName: "refs/heads/land/gt-1"}, "created branch land/gt-1"},
		{"branch deleted", forgejo.Activity{OpType: "delete_branch", RefName: "refs/heads/land/gt-1"}, "deleted branch land/gt-1"},
		{"tag pushed", forgejo.Activity{OpType: "push_tag", RefName: "refs/tags/v1"}, "created tag v1"},
		{"tag deleted", forgejo.Activity{OpType: "delete_tag", RefName: "refs/tags/v1"}, "deleted tag v1"},
		{"pull opened", forgejo.Activity{OpType: "create_pull_request"}, "opened a pull request"},
		{"pull merged", forgejo.Activity{OpType: "merge_pull_request"}, "merged a pull request"},
		{"pull closed", forgejo.Activity{OpType: "close_pull_request"}, "closed a pull request"},
		{"pull reopened", forgejo.Activity{OpType: "reopen_pull_request"}, "reopened a pull request"},
		{"pull approved", forgejo.Activity{OpType: "approve_pull_request"}, "approved a pull request"},
		{"pull rejected", forgejo.Activity{OpType: "reject_pull_request"}, "rejected a pull request"},
		{"pull commented", forgejo.Activity{OpType: "comment_pull"}, "commented on a pull request"},
		{"review requested", forgejo.Activity{OpType: "pull_request_review_request"}, "requested a pull request review"},
		{"review dismissed", forgejo.Activity{OpType: "pull_review_dismissed"}, "dismissed a pull request review"},
		{"an operation the panel does not name", forgejo.Activity{OpType: "publish_release"}, "publish_release"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, forgejoActivitySummary(tc.act))
		})
	}
}

func TestForgejoReaderMergesTheViewersRepos(t *testing.T) {
	t.Parallel()
	stub := &forgejoStub{}
	feed := newForgejoTestReader(t, stub, nil).Read()
	require.Empty(t, feed.Error)
	require.Len(t, feed.Events, 2)
	assert.Equal(t, []string{"/api/v1/user/repos", "/api/v1/repos/sloan/beads/activities/feeds"}, stub.asked())

	// The stub echoes the feed's newest row first; the panel turns it the
	// other way round so the newest line is the last one it appends.
	assert.Equal(t, ForgejoEvent{
		At:      time.Date(2026, 10, 4, 22, 42, 12, 0, time.UTC),
		Actor:   "bot-landing",
		Summary: "deleted branch land/gt-1",
		Repo:    "sloan/beads",
	}, feed.Events[0])
	assert.Equal(t, "pushed main: fix: the thing (gt-1)", feed.Events[1].Summary)
}

// The feed is what the page holds, so the token must not be anywhere in it.
func TestForgejoFeedCarriesNoToken(t *testing.T) {
	t.Parallel()
	stub := &forgejoStub{}
	feed := newForgejoTestReader(t, stub, nil).Read()
	b, err := json.Marshal(feed)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "test-token")

	stub.mu.Lock()
	defer stub.mu.Unlock()
	require.NotEmpty(t, stub.headers, "the stub saw no request to check")
	for _, h := range stub.headers {
		assert.Equal(t, "token test-token", h, "the token goes in the header, and only there")
	}
}

func TestForgejoReaderKeepsTheLastGoodFeedWhenARefreshFails(t *testing.T) {
	t.Parallel()
	stub := &forgejoStub{}
	reader := newForgejoTestReader(t, stub, nil)
	good := reader.Read()
	require.Empty(t, good.Error)
	require.Len(t, good.Events, 2)

	stub.setFail(http.StatusInternalServerError)
	stale := reader.Read()
	assert.Equal(t, "http 500", stale.Error)
	assert.Equal(t, good.Events, stale.Events, "a failed refresh shows the last good feed, marked stale")
}

// The snapshot's read time is what the page ages, so a failed refresh must not
// reset it: the pane is serving the older read and has to be able to say so
// (gt-faml5).
func TestForgejoFeedKeepsItsReadTimeAcrossAFailedRefresh(t *testing.T) {
	t.Parallel()
	stub := &forgejoStub{}
	reader := newForgejoTestReader(t, stub, nil)
	at := time.Date(2026, 10, 5, 16, 0, 0, 0, time.UTC)
	reader.now = func() time.Time { return at }
	good := reader.Read()
	require.Empty(t, good.Error)
	require.Equal(t, at, good.At)

	reader.now = func() time.Time { return at.Add(time.Minute) }
	stub.setFail(http.StatusInternalServerError)
	stale := reader.Read()
	require.Equal(t, "http 500", stale.Error)
	assert.Equal(t, at, stale.At, "a failed refresh leaves the snapshot's age alone")
}

// A repo the viewer's token cannot read must not cost the panel the repos it
// can, and must be named. Reading the subset silently is how om's landings went
// missing with no error anywhere on the pane (gt-faml5).
func TestForgejoReaderNamesReposTheViewerCannotRead(t *testing.T) {
	t.Parallel()
	stub := &forgejoStub{}
	feed := newForgejoTestReader(t, stub, []string{"sloan/beads", "sloan/organic-mechanic"}).Read()
	require.Empty(t, feed.Error)
	require.Len(t, feed.Events, 2, "the readable repo's feed still arrives")
	assert.Equal(t, []string{"sloan/organic-mechanic"}, feed.Missing)
	assert.Equal(t, []string{
		"/api/v1/repos/sloan/beads/activities/feeds",
		"/api/v1/repos/sloan/organic-mechanic/activities/feeds",
	}, stub.asked(), "every named repo is asked for, not just the readable ones")
}

// The panel's poll interval rides the feed so the page can age the snapshot
// against it: the hub owns the cadence, the page only reads it.
func TestForgejoPollStampsTheIntervalForThePage(t *testing.T) {
	t.Parallel()
	h := NewHub(Config{
		Forgejo:      func() *ForgejoFeed { return &ForgejoFeed{} },
		ForgejoEvery: 3 * time.Minute,
	})
	h.pollForgejo()
	require.NotNil(t, h.State().Forgejo)
	assert.Equal(t, 180.0, h.State().Forgejo.EverySec)
}

// A refresh that fails before any good read has no feed to show, so the panel
// gets the class alone.
func TestForgejoReaderBeforeAnyGoodRead(t *testing.T) {
	t.Parallel()
	stub := &forgejoStub{}
	stub.setFail(http.StatusUnauthorized)
	feed := newForgejoTestReader(t, stub, nil).Read()
	assert.Equal(t, "unauthorized", feed.Error)
	assert.Empty(t, feed.Events)
}

func TestForgejoReaderReadsTheNamedReposInsteadOfTheViewerList(t *testing.T) {
	t.Parallel()
	stub := &forgejoStub{}
	feed := newForgejoTestReader(t, stub, []string{"sloan/beads"}).Read()
	require.Empty(t, feed.Error)
	assert.Equal(t, []string{"/api/v1/repos/sloan/beads/activities/feeds"}, stub.asked())

	bad := newForgejoTestReader(t, &forgejoStub{}, []string{"beads"}).Read()
	assert.Equal(t, "bad repo name", bad.Error)
}

func TestForgejoErrorClass(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"unauthorized", &forgejo.APIError{StatusCode: http.StatusUnauthorized}, "unauthorized"},
		{"forbidden", &forgejo.APIError{StatusCode: http.StatusForbidden}, "unauthorized"},
		{"server error", &forgejo.APIError{StatusCode: http.StatusBadGateway}, "http 502"},
		{"a name that is not owner/name", errRepoName, "bad repo name"},
		{"anything else", io.EOF, "unreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, forgejoErrorClass(tc.err))
		})
	}
}

// The hub starts a worker only for the readers it was given, so a town with no
// viewer token carries no Forgejo key at all.
func TestForgejoPanelIsLeftOutWithoutAReader(t *testing.T) {
	t.Parallel()
	h := NewHub(Config{})
	assert.Nil(t, h.State().Forgejo)

	b, err := json.Marshal(h.State())
	require.NoError(t, err)
	assert.False(t, strings.Contains(string(b), "forgejo"), "the snapshot must omit the Forgejo panel, got %s", b)

	h2 := NewHub(Config{Forgejo: func() *ForgejoFeed { return nil }})
	h2.pollForgejo()
	assert.Nil(t, h2.State().Forgejo, "a reader with nothing to report leaves the panel out")
}

func TestForgejoWorkerFillsState(t *testing.T) {
	t.Parallel()
	feed := &ForgejoFeed{Events: []ForgejoEvent{{Summary: "pushed main"}}}
	h := NewHub(Config{Forgejo: func() *ForgejoFeed { return feed }})
	page, _ := h.Subscribe()
	defer h.Unsubscribe(page)

	h.pollForgejo()
	st := h.State()
	require.NotNil(t, st.Forgejo)
	assert.Equal(t, feed.Events, st.Forgejo.Events)
}
