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
// until told to fail, recording what it was asked for. A path can also be made
// to fail on its own, hang until the reader's call bound ends it, or park until
// the test releases it.
type forgejoStub struct {
	mu      sync.Mutex
	fail    int // the status every request answers once set
	paths   []string
	headers []string
	// failPaths answers the status for one path, leaving the rest healthy.
	failPaths map[string]int
	// hangPaths answers only once the reader's per-call bound ends the request,
	// and then as a failure — no sleep is involved (gt-p7lkd).
	hangPaths map[string]bool
	// holdPaths parks a path until the test closes its channel, so a test can
	// keep it in flight while the reader reaches its neighbors. The call's own
	// context still ends the wait, so a reader that never gets there fails
	// rather than hanging the suite (gt-p7lkd).
	holdPaths map[string]chan struct{}
	// started closes its channel when the path is asked for, before the path
	// parks on its hold.
	started map[string]chan struct{}
}

func (s *forgejoStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.paths = append(s.paths, r.URL.Path)
	s.headers = append(s.headers, r.Header.Get("Authorization"))
	fail := s.fail
	if status, ok := s.failPaths[r.URL.Path]; ok {
		fail = status
	}
	hang := s.hangPaths[r.URL.Path]
	started := s.started[r.URL.Path]
	hold := s.holdPaths[r.URL.Path]
	s.mu.Unlock()

	if started != nil {
		close(started)
	}
	if hold != nil {
		select {
		case <-hold:
		case <-r.Context().Done():
			// The in-process transport does not surface a cancelled context as
			// an error, so a wait the bound ended answers a failure the reader
			// can see rather than an empty success.
			w.WriteHeader(http.StatusGatewayTimeout)
			return
		}
	}
	if hang {
		<-r.Context().Done()
		w.WriteHeader(http.StatusGatewayTimeout)
		return
	}
	if fail != 0 {
		w.WriteHeader(fail)
		_, _ = io.WriteString(w, `{"message":"boom"}`)
		return
	}
	switch {
	case r.URL.Path == "/api/v1/user/repos":
		_, _ = io.WriteString(w, `[{"full_name":"sloan/beads"}]`)
	case r.URL.Path == "/api/v1/repos/sloan/beads/activities/feeds":
		_, _ = io.WriteString(w, forgejoFeedJSON)
	case strings.HasSuffix(r.URL.Path, "/activities/feeds"):
		// Any other repo is readable and quiet; a test that wants it invisible
		// gives its path a 404 through failPaths.
		_, _ = io.WriteString(w, `[]`)
	case strings.HasSuffix(r.URL.Path, "/actions/runs"):
		_, _ = io.WriteString(w, `{"total_count":0,"workflow_runs":[]}`)
	default:
		http.NotFound(w, r)
	}
}

func (s *forgejoStub) setFail(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = status
}

// failPath answers status for one path from the next request on.
func (s *forgejoStub) failPath(path string, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failPaths == nil {
		s.failPaths = map[string]int{}
	}
	s.failPaths[path] = status
}

func (s *forgejoStub) asked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.paths...)
}

// activitiesAsked is what the stub was asked for bar the Actions section's runs
// calls, which ride the same refresh: the repo selection is asserted through
// the rest (gt-fn9e6.47).
func activitiesAsked(s *forgejoStub) []string {
	var out []string
	for _, p := range s.asked() {
		if strings.Contains(p, "/actions/runs") {
			continue
		}
		out = append(out, p)
	}
	return out
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
	assert.Equal(t, []string{"/api/v1/user/repos", "/api/v1/repos/sloan/beads/activities/feeds"}, activitiesAsked(stub))

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
	stub := &forgejoStub{failPaths: map[string]int{
		"/api/v1/repos/sloan/organic-mechanic/activities/feeds": http.StatusNotFound,
	}}
	feed := newForgejoTestReader(t, stub, []string{"sloan/beads", "sloan/organic-mechanic"}).Read()
	require.Empty(t, feed.Error)
	require.Len(t, feed.Events, 2, "the readable repo's feed still arrives")
	assert.Equal(t, []string{"sloan/organic-mechanic"}, feed.Missing)
	assert.Empty(t, feed.Failed, "a repo the token cannot see is a permission, not an outage")
	// The repos are read in parallel, so the order the stub saw them in is not
	// the assertion: every named repo is asked for, not just the readable ones.
	assert.ElementsMatch(t, []string{
		"/api/v1/repos/sloan/beads/activities/feeds",
		"/api/v1/repos/sloan/organic-mechanic/activities/feeds",
	}, activitiesAsked(stub))
}

// A repo whose feed outruns its call bound is named, and the repos that
// answered still merge. Before the call bound existed, this repo spent the
// whole refresh's deadline and the pane dropped every repo already read
// (gt-p7lkd).
func TestForgejoReaderOneSlowRepoDoesNotBlankTheOthers(t *testing.T) {
	t.Parallel()
	stub := &forgejoStub{hangPaths: map[string]bool{"/api/v1/repos/sloan/slow/activities/feeds": true}}
	reader := newForgejoTestReader(t, stub, []string{"sloan/beads", "sloan/slow"})
	// The bound, not a sleep, is what ends the hung feed.
	reader.callTimeout = 50 * time.Millisecond
	feed := reader.Read()
	require.Empty(t, feed.Error)
	require.Len(t, feed.Events, 2, "the readable repo's feed still arrives")
	assert.Equal(t, []string{"sloan/slow"}, feed.Failed, "the slow repo is named, not silently dropped")
	assert.Empty(t, feed.Missing, "a slow repo is an outage, not a permission the viewer lacks")
}

// A repo whose feed fails is named, and the repos that answered still merge.
func TestForgejoReaderOneFailingRepoDoesNotBlankTheOthers(t *testing.T) {
	t.Parallel()
	stub := &forgejoStub{failPaths: map[string]int{
		"/api/v1/repos/sloan/broken/activities/feeds": http.StatusInternalServerError,
	}}
	feed := newForgejoTestReader(t, stub, []string{"sloan/beads", "sloan/broken"}).Read()
	require.Empty(t, feed.Error)
	require.Len(t, feed.Events, 2, "the readable repo's feed still arrives")
	assert.Equal(t, []string{"sloan/broken"}, feed.Failed)
	assert.Empty(t, feed.Missing)
}

// The repos are read concurrently, so one repo's feed never spends another's
// budget. Both feeds park until the test releases them: reaching the second
// while the first is still parked is what concurrency looks like, and no
// stopwatch decides it. A reader that took them one after the other serves the
// first from its own call bound and returns before the second is asked for, so
// the test sees Read finish first and fails (gt-p7lkd).
func TestForgejoReaderReadsTheReposConcurrently(t *testing.T) {
	t.Parallel()
	const (
		first  = "/api/v1/repos/sloan/first/activities/feeds"
		second = "/api/v1/repos/sloan/second/activities/feeds"
	)
	release := make(chan struct{})
	firstAsked, secondAsked := make(chan struct{}), make(chan struct{})
	stub := &forgejoStub{
		started:   map[string]chan struct{}{first: firstAsked, second: secondAsked},
		holdPaths: map[string]chan struct{}{first: release, second: release},
	}
	reader := newForgejoTestReader(t, stub, []string{"sloan/first", "sloan/second"})
	done := make(chan *ForgejoFeed, 1)
	go func() { done <- reader.Read() }()

	select {
	case <-firstAsked:
	case feed := <-done:
		t.Fatalf("the refresh ended before any feed was parked: %+v", feed)
	}
	select {
	case <-secondAsked:
	case feed := <-done:
		t.Fatalf("the feeds were read one after the other: the refresh ended with %+v while the first was still parked", feed)
	}
	close(release)
	feed := <-done
	assert.Empty(t, feed.Error)
	assert.Empty(t, feed.Missing)
	assert.Empty(t, feed.Failed, "both parked feeds were read beside each other, so neither outran its bound")
}

// A refresh where every repo fails reads nothing, so it is a failed refresh
// rather than a quiet town (gt-p7lkd).
func TestForgejoReaderEveryRepoFailingIsAFailedRefresh(t *testing.T) {
	t.Parallel()
	stub := &forgejoStub{failPaths: map[string]int{
		"/api/v1/repos/sloan/beads/activities/feeds": http.StatusInternalServerError,
		"/api/v1/repos/sloan/mango/activities/feeds": http.StatusBadGateway,
	}}
	feed := newForgejoTestReader(t, stub, []string{"sloan/beads", "sloan/mango"}).Read()
	assert.Equal(t, "http 500", feed.Error)
	assert.Empty(t, feed.Events)
}

// The same failed refresh after a good one keeps the last good feed and its
// age, so the pane shows stale landings rather than a town with none
// (gt-p7lkd).
func TestForgejoReaderKeepsTheLastGoodFeedWhenEveryRepoFails(t *testing.T) {
	t.Parallel()
	stub := &forgejoStub{}
	reader := newForgejoTestReader(t, stub, []string{"sloan/beads", "sloan/mango"})
	at := time.Date(2026, 10, 5, 16, 0, 0, 0, time.UTC)
	reader.now = func() time.Time { return at }
	good := reader.Read()
	require.Empty(t, good.Error)
	require.Len(t, good.Events, 2)

	stub.failPath("/api/v1/repos/sloan/beads/activities/feeds", http.StatusInternalServerError)
	stub.failPath("/api/v1/repos/sloan/mango/activities/feeds", http.StatusInternalServerError)
	reader.now = func() time.Time { return at.Add(time.Minute) }
	stale := reader.Read()
	assert.Equal(t, "http 500", stale.Error)
	assert.Equal(t, good.Events, stale.Events, "the last good landings stay on the pane")
	assert.Equal(t, at, stale.At, "and keep the age of the read they came from")
}

// A refresh where every repo is invisible read nothing either. Storing it as a
// good read would replace the last good feed with an empty one, so the pane
// would report a town with no landings the moment a token lost its access
// (gt-p7lkd).
func TestForgejoReaderDoesNotStoreAnAllInvisibleRefreshAsGood(t *testing.T) {
	t.Parallel()
	stub := &forgejoStub{}
	reader := newForgejoTestReader(t, stub, []string{"sloan/beads"})
	at := time.Date(2026, 10, 5, 16, 0, 0, 0, time.UTC)
	reader.now = func() time.Time { return at }
	good := reader.Read()
	require.Empty(t, good.Error)
	require.Len(t, good.Events, 2)

	stub.failPath("/api/v1/repos/sloan/beads/activities/feeds", http.StatusNotFound)
	reader.now = func() time.Time { return at.Add(time.Minute) }
	stale := reader.Read()
	assert.Equal(t, "http 404", stale.Error)
	assert.Equal(t, good.Events, stale.Events, "an unreadable repo keeps the last good feed, never an empty one")
	assert.Equal(t, at, stale.At)

	// With nothing good to fall back on, the panel is told the read failed
	// rather than shown "no activity".
	first := newForgejoTestReader(t, &forgejoStub{failPaths: map[string]int{
		"/api/v1/repos/sloan/beads/activities/feeds": http.StatusNotFound,
	}}, []string{"sloan/beads"}).Read()
	assert.Equal(t, "http 404", first.Error, "an all-invisible first read is an outage, not an empty town")
	assert.Empty(t, first.Events)
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
	assert.Equal(t, []string{"/api/v1/repos/sloan/beads/activities/feeds"}, activitiesAsked(stub))

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
