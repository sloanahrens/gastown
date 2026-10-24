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
	// forgejoCallTimeout bounds one API call. The repos are read in parallel
	// and each call carries its own bound, so one slow endpoint costs that
	// call rather than the refresh, and the other repos' feeds still arrive
	// (gt-p7lkd).
	forgejoCallTimeout = 2 * time.Second
	// forgejoMaxParallel bounds how many repos one refresh reads at once. The
	// reads block on the network, so one goroutine per repo makes the fan-out
	// the repo list's length; a fixed crew of workers keeps it bounded, and a
	// repo past the crew waits its turn (gt-p7lkd).
	forgejoMaxParallel = 8
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
	// At is when these events were read, and stays put across a failed refresh:
	// it is the age of the snapshot, which the page needs to tell a
	// current-but-quiet feed from a stale pane (gt-faml5).
	At time.Time `json:"at"`
	// EverySec is the panel's poll interval in seconds, set by the hub that
	// polls. The page marks the snapshot stale past two of them.
	EverySec float64 `json:"every_seconds,omitempty"`
	// Missing names the repos the viewer's token cannot see. Naming them is
	// what keeps a rig whose repository is invisible from reading as a rig with
	// no landings (gt-faml5).
	Missing []string `json:"missing,omitempty"`
	// Failed names the repos this refresh could not read — a call hit an error
	// or outran its bound. It is kept apart from Missing because a permission
	// to fix and an outage to retry are different answers for the operator
	// (gt-p7lkd).
	Failed []string `json:"failed,omitempty"`
	// Actions is the section's workflow runs, read in the same refresh as the
	// events. It is nil when no repo answered for its runs — a reader with no
	// repos at all, or one whose every run list failed — and then carries the
	// last good read, exactly as the events do (gt-fn9e6.47).
	Actions *ForgejoActions `json:"actions,omitempty"`
}

// errRepoName is a repo name that is not owner/name. The panel shows it as the
// operator's mistake rather than as an outage.
var errRepoName = errors.New("repo is not owner/name")

// forgejoFeeds is the part of the Forgejo client the reader calls.
type forgejoFeeds interface {
	ListUserRepos(ctx context.Context) ([]forgejo.Repository, error)
	ListRepoActivities(ctx context.Context, owner, repo string, limit int) ([]forgejo.Activity, error)
	ListRuns(ctx context.Context, owner, repo string, f forgejo.RunFilter) (*forgejo.RunList, error)
}

// ForgejoReader builds the panel's feed from the viewer's repos, holding the
// last good read so a failed refresh shows that feed marked stale instead of
// an empty panel.
type ForgejoReader struct {
	api   forgejoFeeds
	repos []string // owner/name overrides; empty reads the viewer's whole list
	now   func() time.Time
	// callTimeout bounds one API call; zero uses forgejoCallTimeout. It is a
	// field so a test can shorten the bound rather than wait on the default.
	callTimeout time.Duration

	mu   sync.Mutex
	last *ForgejoFeed
}

// NewForgejoReader builds a reader over api. A non-empty repos is the exact
// owner/name list to read instead of every repo the viewer can see.
func NewForgejoReader(api forgejoFeeds, repos []string) *ForgejoReader {
	return &ForgejoReader{api: api, repos: append([]string(nil), repos...), now: time.Now, callTimeout: forgejoCallTimeout}
}

// Read refreshes the feed. A refresh that fails returns the last good feed
// with the error class set and its read time unchanged; one that fails before
// any good read returns the error alone.
func (r *ForgejoReader) Read() *ForgejoFeed {
	feed, err := r.fetch()
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		class := forgejoErrorClass(err)
		if r.last == nil {
			return &ForgejoFeed{Events: []ForgejoEvent{}, Error: class}
		}
		// The last good read keeps its own age: a pane serving it is stale, and
		// saying when it was read is the only way the page can show that.
		return &ForgejoFeed{Events: r.last.Events, Error: class, At: r.last.At, Missing: r.last.Missing, Failed: r.last.Failed, Actions: r.last.Actions}
	}
	if feed.Actions == nil && r.last != nil {
		// No repo answered for its runs: the Actions section keeps the last
		// good value rather than emptying, exactly as the feed does with its
		// events (gt-fn9e6.47).
		feed.Actions = r.last.Actions
	}
	feed.At = r.now()
	r.last = feed
	return feed
}

// fetch reads every repo's feed and workflow runs under one deadline and
// merges them. The deadline still covers the whole refresh, so the poll costs
// the panel at most forgejoTimeout however many repos the viewer sees. The
// repos are read in parallel by a fixed crew of workers, each call under its
// own bound: a refresh of nine repos is one repo's five calls deep, not
// forty-five calls deep, so a call slower than the whole budget allows cannot
// starve the rest (gt-p7lkd).
func (r *ForgejoReader) fetch() (*ForgejoFeed, error) {
	ctx, cancel := context.WithTimeout(context.Background(), forgejoTimeout)
	defer cancel()

	names, err := r.repoNames(ctx)
	if err != nil {
		return nil, err
	}
	// A name that is not owner/name is the operator's mistake, not an outage,
	// so it is named before any read rather than as one repo among the results.
	for _, name := range names {
		if owner, repo, ok := strings.Cut(name, "/"); !ok || owner == "" || repo == "" {
			return nil, fmt.Errorf("%w: %s", errRepoName, strconv.Quote(name))
		}
	}

	reads := make([]repoRead, len(names))
	work := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < min(len(names), forgejoMaxParallel); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				reads[i] = r.readRepo(ctx, names[i])
			}
		}()
	}
	for i := range names {
		work <- i
	}
	close(work)
	wg.Wait()
	return mergeRepoReads(reads)
}

// repoNames is the repos one refresh reads: the reader's own owner/name list,
// or every repository the viewer can see.
func (r *ForgejoReader) repoNames(ctx context.Context) ([]string, error) {
	if len(r.repos) > 0 {
		return append([]string(nil), r.repos...), nil
	}
	callCtx, cancel := r.callCtx(ctx)
	defer cancel()
	repos, err := r.api.ListUserRepos(callCtx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(repos))
	for _, repo := range repos {
		names = append(names, repo.FullName)
	}
	return names, nil
}

// callCtx bounds one API call under the refresh's deadline. Every call gets
// its own bound, so a hung endpoint spends one call's worth of the refresh and
// is then reported, rather than holding the rest of the refresh (gt-p7lkd).
func (r *ForgejoReader) callCtx(parent context.Context) (context.Context, context.CancelFunc) {
	t := r.callTimeout
	if t <= 0 {
		t = forgejoCallTimeout
	}
	return context.WithTimeout(parent, t)
}

// listActivities reads one repo's activity feed under its own bound.
func (r *ForgejoReader) listActivities(ctx context.Context, owner, repo string) ([]forgejo.Activity, error) {
	callCtx, cancel := r.callCtx(ctx)
	defer cancel()
	return r.api.ListRepoActivities(callCtx, owner, repo, forgejoKept)
}

// repoRead is one repo's part of a refresh: its feed and runs, or why it was
// left out.
type repoRead struct {
	name string
	acts []forgejo.Activity
	runs repoRuns
	// err is a read that failed; invisible marks the failure Forgejo answers
	// for a repository the viewer may not see, which names a permission rather
	// than an outage.
	err       error
	invisible bool
}

// readRepo reads one repo's activities and, when they arrived, its runs.
func (r *ForgejoReader) readRepo(ctx context.Context, name string) repoRead {
	out := repoRead{name: name}
	owner, repo, _ := strings.Cut(name, "/") // the caller validated the name
	acts, err := r.listActivities(ctx, owner, repo)
	if err != nil {
		out.err = err
		out.invisible = invisibleToViewer(err)
		return out
	}
	out.acts = acts
	// The Actions section rides the same refresh: a repo the viewer cannot read
	// has already been skipped above, so a runs failure here is a real one and
	// the repo is named in the section's errors.
	out.runs = r.readRepoRuns(ctx, name, owner, repo)
	return out
}

// mergeRepoReads merges every repo's read into the feed. A repo that could not
// be read is named and the rest merge, so one slow or failing feed never blanks
// the others. A refresh where no repo answered at all is an error, which Read
// turns into the last good feed and its age — including a refresh where every
// repo was invisible, which must not be stored as a good read of an empty town
// (gt-p7lkd).
func mergeRepoReads(reads []repoRead) (*ForgejoFeed, error) {
	var (
		all      []forgejo.Activity
		missing  []string
		failed   []string
		runs     []repoRuns
		answered int
		firstErr error
	)
	for _, rr := range reads {
		switch {
		case rr.invisible:
			// One repo the viewer cannot read must not cost the panel the repos
			// it can: the feeds of the read repos still merge, and the rest are
			// named (gt-faml5).
			missing = append(missing, rr.name)
			if firstErr == nil {
				firstErr = rr.err
			}
		case rr.err != nil:
			failed = append(failed, rr.name)
			if firstErr == nil {
				firstErr = rr.err
			}
		default:
			answered++
			all = append(all, rr.acts...)
			runs = append(runs, rr.runs)
		}
	}
	if answered == 0 && firstErr != nil {
		return nil, firstErr
	}
	feed := MergeForgejoActivities(all)
	feed.Missing = missing
	feed.Failed = failed
	feed.Actions = mergeForgejoRuns(runs)
	return feed, nil
}

// invisibleToViewer reports the answer Forgejo gives for a repository the token
// is not allowed to see. Forgejo hides the repository's existence rather than
// admitting it is there, so this is a 404 (verified against 16.0.5, gt-faml5).
func invisibleToViewer(err error) bool {
	var api *forgejo.APIError
	if !errors.As(err, &api) {
		return false
	}
	return api.StatusCode == http.StatusNotFound || api.StatusCode == http.StatusForbidden
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
