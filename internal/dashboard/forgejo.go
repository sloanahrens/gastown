package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/forgejo"
)

const (
	// forgejoKept is how many events the panel shows: the newest across every
	// repo the viewer reads. It is also the page read from each repo, since one
	// busy repo may hold the whole window.
	forgejoKept = 30
	// forgejoTimeout bounds one whole refresh — the repo list and every feed
	// call share it — so a slow Forgejo cannot hold the panel's neighbors.
	forgejoTimeout = 5 * time.Second
)

// ForgejoEvent is one line of the merged Forgejo activity feed.
type ForgejoEvent struct {
	At      time.Time `json:"at"`
	Actor   string    `json:"actor"`
	Summary string    `json:"summary"`
	Repo    string    `json:"repo"`
}

// ForgejoFeed is the Forgejo panel's data: the newest events of the repos the
// viewer can see, oldest first. Error is the class of a failed refresh, whose
// events are then the last good read rather than a fresh one.
type ForgejoFeed struct {
	Events []ForgejoEvent `json:"events"`
	Error  string         `json:"error,omitempty"`
}

// errRepoName is a repo name that is not owner/name. The panel shows it as the
// operator's mistake rather than as an outage.
var errRepoName = errors.New("repo is not owner/name")

// forgejoFeeds is the part of the Forgejo client the reader calls.
type forgejoFeeds interface {
	ListUserRepos(ctx context.Context) ([]forgejo.Repository, error)
	ListRepoActivities(ctx context.Context, owner, repo string, limit int) ([]forgejo.Activity, error)
}

// ForgejoReader builds the panel's feed from the viewer's repos, holding the
// last good read so a failed refresh shows that feed marked stale instead of
// an empty panel.
type ForgejoReader struct {
	api   forgejoFeeds
	repos []string // owner/name overrides; empty reads the viewer's whole list

	mu   sync.Mutex
	last *ForgejoFeed
}

// NewForgejoReader builds a reader over api. A non-empty repos is the exact
// owner/name list to read instead of every repo the viewer can see.
func NewForgejoReader(api forgejoFeeds, repos []string) *ForgejoReader {
	return &ForgejoReader{api: api, repos: append([]string(nil), repos...)}
}

// Read refreshes the feed. A refresh that fails returns the last good feed
// with the error class set; one that fails before any good read returns the
// error alone.
func (r *ForgejoReader) Read() *ForgejoFeed {
	feed, err := r.fetch()
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		class := forgejoErrorClass(err)
		if r.last == nil {
			return &ForgejoFeed{Events: []ForgejoEvent{}, Error: class}
		}
		return &ForgejoFeed{Events: r.last.Events, Error: class}
	}
	r.last = feed
	return feed
}

// fetch reads every repo's feed under one deadline and merges them. The
// deadline covers the whole refresh rather than each call, so the poll costs
// the panel at most forgejoTimeout however many repos the viewer sees.
func (r *ForgejoReader) fetch() (*ForgejoFeed, error) {
	ctx, cancel := context.WithTimeout(context.Background(), forgejoTimeout)
	defer cancel()

	var names []string
	if len(r.repos) > 0 {
		names = append(names, r.repos...)
	} else {
		repos, err := r.api.ListUserRepos(ctx)
		if err != nil {
			return nil, err
		}
		for _, repo := range repos {
			names = append(names, repo.FullName)
		}
	}

	var all []forgejo.Activity
	for _, name := range names {
		owner, repo, ok := strings.Cut(name, "/")
		if !ok || owner == "" || repo == "" {
			return nil, fmt.Errorf("%w: %s", errRepoName, strconv.Quote(name))
		}
		acts, err := r.api.ListRepoActivities(ctx, owner, repo, forgejoKept)
		if err != nil {
			return nil, err
		}
		all = append(all, acts...)
	}
	return MergeForgejoActivities(all), nil
}

// MergeForgejoActivities merges the activities of every repo into one feed:
// the newest forgejoKept events, oldest first, so the newest line is the last
// one the panel appends.
func MergeForgejoActivities(acts []forgejo.Activity) *ForgejoFeed {
	sorted := append([]forgejo.Activity(nil), acts...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Created.After(sorted[j].Created) })
	if len(sorted) > forgejoKept {
		sorted = sorted[:forgejoKept]
	}
	events := make([]ForgejoEvent, 0, len(sorted))
	for i := len(sorted) - 1; i >= 0; i-- {
		a := sorted[i]
		events = append(events, ForgejoEvent{
			At:      a.Created,
			Actor:   forgejoActor(a),
			Summary: forgejoActivitySummary(a),
			Repo:    forgejoRepoName(a),
		})
	}
	return &ForgejoFeed{Events: events}
}

// forgejoActivitySummary renders one activity as the panel's compact phrase.
// An operation this does not name is shown by its op_type, so a Forgejo
// release the panel predates still reads as something.
func forgejoActivitySummary(a forgejo.Activity) string {
	ref := shortRef(a.RefName)
	switch a.OpType {
	case "commit_repo":
		if msg, ok := a.HeadMessage(); ok {
			return phrase("pushed", ref) + ": " + msg
		}
		return phrase("pushed", ref)
	case "create_branch":
		return phrase("created branch", ref)
	case "delete_branch":
		return phrase("deleted branch", ref)
	case "push_tag":
		return phrase("created tag", ref)
	case "delete_tag":
		return phrase("deleted tag", ref)
	case "create_pull_request":
		return "opened a pull request"
	case "merge_pull_request":
		return "merged a pull request"
	case "close_pull_request":
		return "closed a pull request"
	case "reopen_pull_request":
		return "reopened a pull request"
	case "approve_pull_request":
		return "approved a pull request"
	case "reject_pull_request":
		return "rejected a pull request"
	case "comment_pull":
		return "commented on a pull request"
	case "pull_request_review_request":
		return "requested a pull request review"
	case "pull_review_dismissed":
		return "dismissed a pull request review"
	}
	return a.OpType
}

// phrase joins a verb and the ref it acted on, leaving the verb bare when the
// activity carried no ref.
func phrase(verb, ref string) string {
	if ref == "" {
		return verb
	}
	return verb + " " + ref
}

// shortRef turns a full ref name into the branch or tag it names, so the panel
// shows "main" rather than "refs/heads/main".
func shortRef(ref string) string {
	for _, prefix := range []string{"refs/heads/", "refs/tags/"} {
		if strings.HasPrefix(ref, prefix) {
			return strings.TrimPrefix(ref, prefix)
		}
	}
	return ref
}

func forgejoActor(a forgejo.Activity) string {
	if a.ActUser == nil {
		return ""
	}
	return a.ActUser.Login
}

func forgejoRepoName(a forgejo.Activity) string {
	if a.Repo == nil {
		return ""
	}
	return a.Repo.FullName
}

// forgejoErrorClass reduces a failed refresh to the short class the panel note
// shows. The token is never part of it: an *APIError carries only the request,
// the status and the server's own message.
func forgejoErrorClass(err error) string {
	var api *forgejo.APIError
	switch {
	case errors.Is(err, errRepoName):
		return "bad repo name"
	case errors.As(err, &api):
		if api.StatusCode == http.StatusUnauthorized || api.StatusCode == http.StatusForbidden {
			return "unauthorized"
		}
		return "http " + strconv.Itoa(api.StatusCode)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	return "unreachable"
}
