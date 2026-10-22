package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newsStub is a stand-in Hacker News: the front-page id list and the items
// behind it, on the paths the Firebase API serves, until it is told to fail.
// The reader's own request building and response handling run against it, over
// the in-process transport the Forgejo tests use — the unit tier opens no
// socket (testpolicy rule no-network).
type newsStub struct {
	mu        sync.Mutex
	ids       []int64
	items     map[int64]string
	fail      int            // the status every request answers once set
	failItems map[int64]bool // the item ids whose own reads answer 500 once set
	moved     string         // the host every request is redirected to once set
	body      string         // what every request answers with, when set
	asked     []string
}

func (s *newsStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, r.URL.Path)
	if s.moved != "" {
		w.Header().Set("Location", "http://"+s.moved+r.URL.Path)
		w.WriteHeader(http.StatusFound)
		return
	}
	if s.fail != 0 {
		w.WriteHeader(s.fail)
		return
	}
	if s.body != "" {
		_, _ = io.WriteString(w, s.body)
		return
	}
	switch {
	case r.URL.Path == "/v0/topstories.json":
		b, _ := json.Marshal(s.ids)
		_, _ = w.Write(b)
	case strings.HasPrefix(r.URL.Path, "/v0/item/"):
		var id int64
		if _, err := fmt.Sscanf(r.URL.Path, "/v0/item/%d.json", &id); err != nil {
			http.NotFound(w, r)
			return
		}
		if s.failItems[id] {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		body, ok := s.items[id]
		if !ok {
			_, _ = io.WriteString(w, "null")
			return
		}
		_, _ = io.WriteString(w, body)
	default:
		http.NotFound(w, r)
	}
}

func (s *newsStub) setFail(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = status
}

func (s *newsStub) setMoved(host string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.moved = host
}

// setFailItems is the item ids the stub answers 500 for, so a test can break
// one line of the front page without breaking the list it is on.
func (s *newsStub) setFailItems(ids ...int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failItems = make(map[int64]bool, len(ids))
	for _, id := range ids {
		s.failItems[id] = true
	}
}

// newsAsked is every path the stub was asked for, in order.
func (s *newsStub) newsAsked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.asked...)
}

// newsStubTopstories counts the front-page reads, which is what the pane's
// five-minute hold is measured in.
func (s *newsStub) topstoriesReads() int {
	n := 0
	for _, p := range s.newsAsked() {
		if p == "/v0/topstories.json" {
			n++
		}
	}
	return n
}

// newsItemJSON is one item in the shape the API serves.
func newsItemJSON(id int64, title, url string, score, comments int, at time.Time) string {
	return fmt.Sprintf(`{"id":%d,"title":%q,"url":%q,"score":%d,"descendants":%d,"time":%d,"type":"story"}`,
		id, title, url, score, comments, at.Unix())
}

// newsTestClock is a reader whose clock the test moves, so the pane's five
// minutes pass without anything waiting.
func newsTestClock(t *testing.T, stub *newsStub, at time.Time) (*NewsReader, func(time.Duration)) {
	t.Helper()
	r := NewNewsReader(stubTransport{stub})
	r.baseURL = "http://hn.test/v0"
	now := at
	r.now = func() time.Time { return now }
	return r, func(d time.Duration) { now = now.Add(d) }
}

// newsTestStub is a front page of five stories behind seven ids: the pane lists
// the first five ids, so the tail is never read.
func newsTestStub(at time.Time) *newsStub {
	return &newsStub{
		ids: []int64{11, 12, 13, 14, 15, 16, 17},
		items: map[int64]string{
			11: newsItemJSON(11, "Postgres 18", "https://www.postgresql.org/about/news/pg18/", 412, 96, at.Add(-3*time.Hour)),
			12: newsItemJSON(12, "The case for boring software", "https://example.com/boring", 130, 44, at.Add(-2*time.Hour)),
			13: newsItemJSON(13, "Ask HN: what broke in your stack this week?", "", 88, 121, at.Add(-90*time.Minute)),
			14: newsItemJSON(14, "A tour of the Zilog Z80", "https://oldbit.example/z80", 210, 17, at.Add(-40*time.Minute)),
			15: newsItemJSON(15, "Show HN: I made a spreadsheet of the town", "https://show.example/sheet", 64, 8, at.Add(-20*time.Minute)),
			16: newsItemJSON(16, "never read", "https://example.com/16", 1, 1, at),
			17: newsItemJSON(17, "never read", "https://example.com/17", 1, 1, at),
		},
	}
}

func TestNewsReaderServesTheTopFiveStoriesWithTheirFields(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	stub := newsTestStub(at)
	reader, _ := newsTestClock(t, stub, at)

	feed := reader.Read()
	require.Empty(t, feed.Error)
	require.Len(t, feed.Stories, newsStories)
	assert.Equal(t, at, feed.At, "the snapshot is dated when it was read")

	top := feed.Stories[0]
	assert.Equal(t, 1, top.Rank)
	assert.Equal(t, int64(11), top.ID)
	assert.Equal(t, "Postgres 18", top.Title)
	assert.Equal(t, "https://www.postgresql.org/about/news/pg18/", top.URL)
	assert.Equal(t, "postgresql.org", top.Domain, "the domain is the host the page's hover names, without the www")
	assert.Equal(t, 412, top.Points)
	assert.Equal(t, 96, top.Comments)
	assert.Equal(t, at.Add(-3*time.Hour), top.At, "the story's own age is its posting time")

	for i, st := range feed.Stories {
		assert.Equal(t, i+1, st.Rank, "the lines are numbered in the order they are listed")
	}
	// The pane lists five, so the ids past them are never fetched: six calls,
	// one list and five items.
	assert.Equal(t, []string{
		"/v0/topstories.json", "/v0/item/11.json", "/v0/item/12.json", "/v0/item/13.json", "/v0/item/14.json", "/v0/item/15.json",
	}, stub.newsAsked())
}

// The five-minute hold: a page that reconnects, or a hub that is woken, must
// not cost Hacker News a second read inside it.
func TestNewsReaderServesACachedSnapshotInsideFiveMinutes(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	stub := newsTestStub(at)
	reader, advance := newsTestClock(t, stub, at)

	first := reader.Read()
	require.Len(t, first.Stories, newsStories)

	advance(newsRefresh - time.Second)
	second := reader.Read()
	require.Len(t, second.Stories, newsStories)
	assert.Equal(t, 1, stub.topstoriesReads(), "a read inside five minutes serves the snapshot already held")
	assert.Equal(t, first.At, second.At)

	advance(time.Second)
	third := reader.Read()
	require.Len(t, third.Stories, newsStories)
	assert.Equal(t, 2, stub.topstoriesReads(), "a read past five minutes refreshes")
	assert.Equal(t, at.Add(newsRefresh), third.At)

	// The hub owns the feed it is handed, so a later read cannot write through
	// a snapshot a page is already drawing.
	second.Stories[0].Title = "edited"
	assert.Equal(t, "Postgres 18", first.Stories[0].Title)
}

// A refresh that fails keeps the five stories the last good one read, dated
// when they were read, so the pane can show them dimmed as stale rather than
// reading the failure as a front page with nothing on it.
func TestNewsReaderKeepsTheLastGoodStoriesWhenARefreshFails(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	stub := newsTestStub(at)
	reader, advance := newsTestClock(t, stub, at)

	good := reader.Read()
	require.Empty(t, good.Error)

	advance(newsRefresh)
	stub.setFail(http.StatusInternalServerError)
	stale := reader.Read()
	assert.Equal(t, "http 500", stale.Error)
	assert.Equal(t, good.Stories, stale.Stories, "the last good stories are still what the pane shows")
	assert.Equal(t, good.At, stale.At, "a failed refresh leaves the snapshot's age alone")
}

// A refresh that fails before any success has nothing to keep, so the pane gets
// the class alone and says it is unavailable rather than showing an empty front
// page.
func TestNewsReaderBeforeAnyGoodRead(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	stub := newsTestStub(at)
	stub.setFail(http.StatusBadGateway)

	feed := newsTestReader(t, stub, at).Read()
	assert.Equal(t, "http 502", feed.Error)
	assert.Empty(t, feed.Stories)
	assert.True(t, feed.At.IsZero())

	// An empty read is an empty list, never null: the pane reads the field and
	// a null one would have it draw nothing at all.
	b, err := json.Marshal(feed)
	require.NoError(t, err)
	assert.Contains(t, string(b), `"stories":[]`)
}

// An Ask HN post carries no url of its own, and Hacker News's own front page
// sends it to the discussion; the pane's title link does the same, so a reader
// who clicks through lands where the comments are.
func TestNewsReaderLinksAnAskHnPostToItsDiscussionPage(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	feed := newsTestReader(t, newsTestStub(at), at).Read()

	ask := feed.Stories[2]
	require.Equal(t, int64(13), ask.ID)
	assert.True(t, strings.HasPrefix(ask.Title, "Ask HN:"))
	assert.Equal(t, newsItemURL+"13", ask.URL)
	assert.Empty(t, ask.Domain, "a post with no url has no domain to name")
	assert.Equal(t, 121, ask.Comments)
}

// An id whose item Hacker News answers null for is one that has gone: the pane
// lists what it can draw rather than a blank line.
func TestNewsReaderLeavesOutAnItemWithNoTitle(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	stub := newsTestStub(at)
	delete(stub.items, 13)

	feed := newsTestReader(t, stub, at).Read()
	require.Empty(t, feed.Error)
	require.Len(t, feed.Stories, newsStories-1)
	assert.Equal(t, []int64{11, 12, 14, 15}, []int64{feed.Stories[0].ID, feed.Stories[1].ID, feed.Stories[2].ID, feed.Stories[3].ID})
	for i, st := range feed.Stories {
		assert.Equal(t, i+1, st.Rank, "the lines are numbered over what is listed, so the list has no gap")
	}
}

// One item the pane cannot read costs that one line, not the refresh: failing
// the read over it would throw away the four lines it did read, and the pane
// would go stale for five minutes over one item (gt-9ktrr).
func TestNewsReaderKeepsTheLinesItCouldRead(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	stub := newsTestStub(at)
	stub.setFailItems(13)

	feed := newsTestReader(t, stub, at).Read()
	require.Empty(t, feed.Error, "a read that got four of five stories is not a failed refresh")
	require.Len(t, feed.Stories, newsStories-1)
	assert.Equal(t, []int64{11, 12, 14, 15}, []int64{feed.Stories[0].ID, feed.Stories[1].ID, feed.Stories[2].ID, feed.Stories[3].ID})
	assert.Equal(t, at, feed.At, "the snapshot is the read that just happened, not a kept one")
}

// A refresh that read no story at all is the failure it is, so it keeps the
// last good stories: five item reads Hacker News answered for with nothing are
// not a front page with no stories on it, and publishing the empty list over
// the last good five would blank the pane for five minutes (gt-9ktrr).
func TestNewsReaderKeepsTheLastGoodStoriesWhenItReadsNone(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	stub := newsTestStub(at)
	reader, advance := newsTestClock(t, stub, at)

	good := reader.Read()
	require.Len(t, good.Stories, newsStories)

	advance(newsRefresh)
	stub.setFailItems(11, 12, 13, 14, 15)
	stale := reader.Read()
	assert.Equal(t, "http 500", stale.Error)
	assert.Equal(t, good.Stories, stale.Stories, "the pane keeps the last good five rather than an empty front page")
	assert.Equal(t, good.At, stale.At, "and their own read time, so the pane can age them")
}

// An empty front page is a read the pane could not make rather than a front
// page with nothing on it: Hacker News serves stories whenever it answers at
// all, so the pane says it is unavailable instead of drawing a page it read as
// empty (gt-9ktrr).
func TestNewsReaderReadsAnEmptyStoryListAsAFailure(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

	feed := newsTestReader(t, &newsStub{}, at).Read()
	assert.Equal(t, "unreadable", feed.Error)
	assert.Empty(t, feed.Stories)
	assert.True(t, feed.At.IsZero())
}

// A story whose url is not one a browser can be sent to links to its discussion
// instead: the pane's links open in a new tab, and a url that runs code would
// run it in the dashboard's own origin (gt-9ktrr).
func TestNewsReaderWillNotLinkToAUrlThatIsNotHttp(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		url  string
	}{
		{"a url that runs code", "javascript:alert(1)"},
		{"a scheme of its own", "data:text/html,<b>x</b>"},
		{"a url with no host", "https:///nowhere"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := &newsStub{
				ids:   []int64{11},
				items: map[int64]string{11: newsItemJSON(11, "free money", tc.url, 1, 1, at)},
			}
			feed := newsTestReader(t, stub, at).Read()
			require.Len(t, feed.Stories, 1)
			assert.Equal(t, newsItemURL+"11", feed.Stories[0].URL)
			assert.Empty(t, feed.Stories[0].Domain)
		})
	}
}

// newsTestReader wires a reader to the stub on a clock that never moves.
func newsTestReader(t *testing.T, stub *newsStub, at time.Time) *NewsReader {
	t.Helper()
	reader, _ := newsTestClock(t, stub, at)
	return reader
}

// The pane's data comes from the Hacker News API: a redirect is how another
// host would serve something in its place, so the reader refuses to follow one.
func TestNewsReaderRefusesARedirectToAnotherHost(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	stub := newsTestStub(at)
	stub.setMoved("elsewhere.test")

	feed := newsTestReader(t, stub, at).Read()
	assert.Equal(t, "unreachable", feed.Error)
	assert.Empty(t, feed.Stories)
	assert.Equal(t, []string{"/v0/topstories.json"}, stub.newsAsked(), "nothing past the refused redirect is read")
}

// A response that is not the API's — over the cap, or not the JSON it serves —
// is the pane's own class of failure, since it is not the network that failed.
func TestNewsReaderReportsAResponseItCannotRead(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		body string
	}{
		{"over the cap", `"` + strings.Repeat("x", newsMaxBody) + `"`},
		{"not json", "<html>not the api</html>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stub := &newsStub{body: tc.body}
			feed := newsTestReader(t, stub, at).Read()
			assert.Equal(t, "unreadable", feed.Error)
			assert.Empty(t, feed.Stories)
		})
	}
}

func TestNewsErrorClass(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"a status the api did not answer with", &newsHTTPError{StatusCode: http.StatusNotFound}, "http 404"},
		{"a deadline", context.DeadlineExceeded, "timeout"},
		{"a body the pane cannot read", errNewsUnreadable, "unreadable"},
		{"anything else", io.EOF, "unreachable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, newsErrorClass(tc.err))
		})
	}
}

// The hub starts a worker only for the readers it was given, so a town that
// switched the pane off carries no news key at all and the page draws no
// section.
func TestNewsPanelIsLeftOutWithoutAReader(t *testing.T) {
	t.Parallel()
	h := NewHub(Config{})
	assert.Nil(t, h.State().News)

	b, err := json.Marshal(h.State())
	require.NoError(t, err)
	assert.False(t, strings.Contains(string(b), "news"), "the snapshot must omit the News pane, got %s", b)

	h2 := NewHub(Config{News: func() *NewsFeed { return nil }})
	h2.pollNews()
	assert.Nil(t, h2.State().News, "a reader with nothing to report leaves the pane out")
}

// A read that fails publishes the last good stories with their error and raises
// nothing: Hacker News being slow is not a town condition the operator is
// paged for (gt-9ktrr).
func TestNewsPollPublishesWithoutAlerting(t *testing.T) {
	t.Parallel()
	feed := &NewsFeed{Stories: []NewsStory{{Rank: 1, Title: "Postgres 18"}}, Error: "unreachable"}
	h := NewHub(Config{News: func() *NewsFeed { return feed }})
	page, _ := h.Subscribe()
	defer h.Unsubscribe(page)

	h.pollNews()
	st := h.State()
	require.NotNil(t, st.News)
	assert.Equal(t, feed.Stories, st.News.Stories)
	assert.Empty(t, st.Alerts, "a failed news read raises no alert")
	assert.Empty(t, st.Reads, "and leaves no reading behind for the health pill to carry")
}

// The hub's poll has to fall inside the five minutes the reader holds its
// snapshot for. The two at the same length is the defect this pins: a poll
// landing a moment before the snapshot came due serves the held copy, and the
// refresh that belonged to it waits for the poll after, so the pane refreshes
// about every ten minutes while reading as though it refreshed every five
// (gt-9ktrr).
func TestNewsPollFallsInsideTheSnapshotItReads(t *testing.T) {
	t.Parallel()

	var c Config
	c.defaults()
	require.NotZero(t, c.NewsEvery, "the pane needs a poll of its own")
	assert.Less(t, c.NewsEvery, newsRefresh, "a poll on the snapshot's own edge can serve it and lose the refresh")
}

// A refresh that lost an item to an error and so came back with fewer stories
// than the last good feed does not replace it: the thinner list would read as
// fresh for five minutes. The pane keeps the full five at their own read time
// and says why, whichever items failed.
func TestTopStoriesPartialReadKeepsLastGood(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		failed []int64
	}{
		{name: "one item fails", failed: []int64{11}},
		{name: "a run of items fails", failed: []int64{11, 12, 13, 14}},
		{name: "the last listed item fails", failed: []int64{15}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
			stub := newsTestStub(at)
			reader, advance := newsTestClock(t, stub, at)

			good := reader.Read()
			require.Len(t, good.Stories, newsStories)

			advance(newsRefresh)
			stub.setFailItems(tc.failed...)
			partial := reader.Read()
			assert.Equal(t, "http 500", partial.Error)
			assert.Equal(t, good.Stories, partial.Stories, "the pane keeps the last full five")
			assert.Equal(t, good.At, partial.At, "and their own read time, so the pane ages them")
		})
	}
}

// An item that keeps failing must not pin the pane to the stories it showed
// before. The first thinner read is held; the next one is taken as it comes,
// and the pane shows the four it read at their own time, with no error, since
// the item it lost is left out the same way a titleless item is.
func TestTopStoriesPersistentItemErrorDoesNotPinTheFeed(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	stub := newsTestStub(at)
	reader, advance := newsTestClock(t, stub, at)

	good := reader.Read()
	require.Len(t, good.Stories, newsStories)

	stub.setFailItems(11)
	advance(newsRefresh)
	held := reader.Read()
	assert.Equal(t, good.Stories, held.Stories, "the first thinner read keeps the last full five")
	assert.Equal(t, good.At, held.At)

	advance(newsRefresh)
	taken := reader.Read()
	assert.Len(t, taken.Stories, newsStories-1, "the second thinner read in a row is taken")
	assert.Equal(t, at.Add(2*newsRefresh), taken.At, "at its own read time")
	assert.Empty(t, taken.Error)
}
