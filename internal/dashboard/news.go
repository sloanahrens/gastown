package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// newsStories is how many top stories the pane lists: the first ids of
	// Hacker News's own list, in its own order.
	newsStories = 5
	// newsRefresh is how long a read serves the snapshot the last fetch
	// produced, failure included. It is the pane's whole clock: Hacker News
	// sees at most one refresh every five minutes however many pages are open.
	newsRefresh = 5 * time.Minute
	// newsPoll is how often the hub asks the reader, a fraction of the
	// snapshot's life rather than a length of its own: a poll landing on the
	// snapshot's edge serves the held copy and leaves the refresh it should
	// have carried to the poll after it, halving the pane's rate (gt-9ktrr).
	newsPoll = newsRefresh / 5
	// newsTimeout bounds one whole refresh — the story list and every item read
	// share it — so a slow Hacker News cannot hold the hub's other pollers.
	newsTimeout = 10 * time.Second
	// newsMaxBody caps one response. The API's largest answer is the story id
	// list, a few kilobytes, so the cap only ever stops a response that is not
	// the API's.
	newsMaxBody = 64 << 10
	// newsBaseURL is the Hacker News Firebase API root.
	newsBaseURL = "https://hacker-news.firebaseio.com/v0"
	// newsItemURL is the discussion page an item id names: where the pane's
	// comments link goes, and where an Ask HN post links, having no url of its
	// own.
	newsItemURL = "https://news.ycombinator.com/item?id="
)

// errNewsUnreadable is a response the pane cannot read as a front page: one
// over the size cap, one that is not the JSON the API serves, or one with no
// story on it. It is a class of its own because it is not the network that
// failed.
var errNewsUnreadable = errors.New("unreadable response")

// newsItem is the part of one Hacker News item the pane reads. An Ask HN or
// other text post carries no url; a post that is gone decodes to nothing.
type newsItem struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	Score       int    `json:"score"`
	Descendants int    `json:"descendants"`
	Time        int64  `json:"time"` // unix seconds
}

// NewsStory is one line of the News pane. URL is where the title links: the
// story itself, or its discussion page when the post has no url of its own.
type NewsStory struct {
	Rank     int       `json:"rank"`
	ID       int64     `json:"id"`
	Title    string    `json:"title"`
	URL      string    `json:"url"`
	Domain   string    `json:"domain,omitempty"`
	Points   int       `json:"points"`
	Comments int       `json:"comments"`
	At       time.Time `json:"at"`
}

// NewsFeed is the News pane's data: the top stories as the last good refresh
// read them. Error is the class of a failed refresh, whose stories are then the
// last good ones with At left at when they were read, so the page can show them
// dimmed under a "stale <age>" note. A refresh that fails before any success
// leaves no stories and the error alone.
type NewsFeed struct {
	Stories []NewsStory `json:"stories"`
	Error   string      `json:"error,omitempty"`
	At      time.Time   `json:"at"`
}

// clone hands the hub the pane's data without the reader's own copy, so a feed
// already published is never written through by a later read. The stories are
// copied rather than appended to a nil slice, which would turn a feed with
// nothing to report into a null the page has to guess at.
func (f *NewsFeed) clone() *NewsFeed {
	out := *f
	out.Stories = make([]NewsStory, len(f.Stories))
	copy(out.Stories, f.Stories)
	return &out
}

// NewsReader reads Hacker News's top stories and holds the snapshot for the
// refresh interval, so a read inside it costs Hacker News nothing.
type NewsReader struct {
	client  *http.Client
	baseURL string
	now     func() time.Time

	mu     sync.Mutex
	at     time.Time // when the cached snapshot was fetched
	cached *NewsFeed // what that fetch produced, last good stories included
	// held is whether the last fetch kept the last good feed over a thinner
	// read, so the next thinner read in a row is taken rather than held again.
	held bool
}

// NewNewsReader builds the pane's reader over transport, which a test replaces
// with a stub and production leaves nil for the default. The client around it
// is the pane's own, so every read carries the redirect refusal below whatever
// the transport is.
func NewNewsReader(transport http.RoundTripper) *NewsReader {
	return &NewsReader{client: newsHTTPClient(transport), baseURL: newsBaseURL, now: time.Now}
}

// Read returns the pane's data, fetching at most once every five minutes; a
// fetch that fails returns the last good stories with the error class set.
func (r *NewsReader) Read() *NewsFeed {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if r.cached != nil && now.Sub(r.at) < newsRefresh {
		return r.cached.clone()
	}
	r.cached = r.fetch(now)
	r.at = now
	return r.cached.clone()
}

// fetch reads the top story ids and then each of their items under one
// deadline. A read that comes back with nothing to show keeps the last good
// stories with their own read time: the pane ages what it is showing rather
// than reading a failed refresh as a front page with no stories on it. So does
// a read that lost items to errors and came back thinner than the last good
// feed: it would otherwise replace the full list as fresh for five minutes. It
// is held once, though. An item that keeps failing would pin the pane to the
// stories it showed before, so the thinner read after a held one is taken.
func (r *NewsReader) fetch(now time.Time) *NewsFeed {
	ctx, cancel := context.WithTimeout(context.Background(), newsTimeout)
	defer cancel()

	wasHeld := r.held
	r.held = false
	read, err := r.topStories(ctx)
	if err == nil {
		thinner := read.itemErr != nil && r.cached != nil && len(read.stories) < len(r.cached.Stories)
		if !thinner || wasHeld {
			return &NewsFeed{Stories: read.stories, At: now}
		}
		r.held = true
		err = read.itemErr
	}
	class := newsErrorClass(err)
	if r.cached == nil {
		return &NewsFeed{Stories: []NewsStory{}, Error: class}
	}
	return &NewsFeed{Stories: r.cached.Stories, Error: class, At: r.cached.At}
}

// topStories reads the first ids of the front-page list and the item behind
// each. An id whose item has no title is not a story the pane can show — Hacker
// News answers null for one that has gone — and is left out, so the list holds
// what can be drawn rather than a blank line. An item the pane cannot read
// costs that one line the same way rather than the whole refresh: failing the
// refresh over it would throw away the four the read did get. What is left is
// an error when it is nothing at all, which is what a refresh the pane read no
// stories from is, so a read that produced nothing keeps the last good stories
// instead of replacing them with an empty front page. The first item error of a
// read that still got stories comes back beside them, for the caller to weigh
// against the feed it already holds.
func (r *NewsReader) topStories(ctx context.Context) (newsRead, error) {
	var ids []int64
	if err := r.get(ctx, "/topstories.json", &ids); err != nil {
		return newsRead{}, err
	}
	if len(ids) > newsStories {
		ids = ids[:newsStories]
	}
	out := make([]NewsStory, 0, len(ids))
	var firstErr error
	for _, id := range ids {
		var it newsItem
		if err := r.get(ctx, "/item/"+strconv.FormatInt(id, 10)+".json", &it); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if it.Title == "" {
			continue
		}
		out = append(out, it.story(len(out)+1))
	}
	if len(out) == 0 {
		if firstErr != nil {
			return newsRead{}, firstErr
		}
		return newsRead{}, fmt.Errorf("%w: no story on the front page", errNewsUnreadable)
	}
	return newsRead{stories: out, itemErr: firstErr}, nil
}

// newsRead is what one pass over the front page produced: the stories it got
// and the first item error it met on the way, nil when every item answered.
type newsRead struct {
	stories []NewsStory
	itemErr error
}

// story renders one item as the pane's line.
func (it newsItem) story(rank int) NewsStory {
	return NewsStory{
		Rank:     rank,
		ID:       it.ID,
		Title:    it.Title,
		URL:      it.link(),
		Domain:   newsDomain(it.URL),
		Points:   it.Score,
		Comments: it.Descendants,
		At:       time.Unix(it.Time, 0).UTC(),
	}
}

// link is where the pane's title goes: the story's own url when it is one a
// browser can be sent to, and the discussion page otherwise. An Ask HN or other
// text post has no url at all, and Hacker News's own front page sends those to
// the discussion; a url that is not http(s) — which Hacker News refuses to
// accept as a submission, so this is belt and braces — would run as code in the
// dashboard's own origin, and is not a link this pane puts in an href.
func (it newsItem) link() string {
	discussion := newsItemURL + strconv.FormatInt(it.ID, 10)
	u, err := url.Parse(it.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return discussion
	}
	return it.URL
}

// newsDomain is a story's host without the www Hacker News drops, which is what
// the line's hover names. A url the pane cannot parse has no domain rather than
// a wrong one.
func newsDomain(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Hostname(), "www.")
}

// newsHTTPClient is the client the pane's own reads go through: no redirect off
// the API's host, since the pane's data comes from there and a redirect is how
// another host would serve it instead.
func newsHTTPClient(transport http.RoundTripper) *http.Client {
	return &http.Client{Transport: transport, CheckRedirect: newsRedirect}
}

// newsRedirect refuses a redirect to another host, and one that goes round in
// circles on the API's own.
func newsRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 3 {
		return fmt.Errorf("too many redirects: %s", req.URL)
	}
	if req.URL.Host != via[0].URL.Host {
		return fmt.Errorf("redirect to another host: %s", req.URL.Host)
	}
	return nil
}

// newsHTTPError is a response that is not the 200 the API answers with.
type newsHTTPError struct{ StatusCode int }

func (e *newsHTTPError) Error() string {
	return "http " + strconv.Itoa(e.StatusCode)
}

// get reads one API call into out.
func (r *NewsReader) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return &newsHTTPError{StatusCode: resp.StatusCode}
	}
	// One byte past the cap is read so a response at it is told from one over
	// it, rather than truncated into a decode error the pane would misreport.
	data, err := io.ReadAll(io.LimitReader(resp.Body, newsMaxBody+1))
	if err != nil {
		return err
	}
	if len(data) > newsMaxBody {
		return fmt.Errorf("%w: over %d bytes", errNewsUnreadable, newsMaxBody)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%w: %w", errNewsUnreadable, err)
	}
	return nil
}

// newsErrorClass reduces a failed refresh to the short class the pane shows.
func newsErrorClass(err error) string {
	var status *newsHTTPError
	switch {
	case errors.As(err, &status):
		return "http " + strconv.Itoa(status.StatusCode)
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, errNewsUnreadable):
		return "unreadable"
	}
	return "unreachable"
}
