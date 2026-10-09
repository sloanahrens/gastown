package dashboard

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// The staging ship reader: the answer to "has this landing reached staging
// yet?". An app rig's landing is deployed when the repository's staging
// workflow — the one that deploys main to staging after every landing — has
// finished a run whose commit contains the landed commit. The daemon restart
// that ships gastown never installs an app rig's commit, so without this the
// Landings table would wait on it forever.
//
// It reads the same runs through the same read-only viewer client as the
// Deploys block (forgejo.ListRuns) and adds the compare that decides
// "contains" (forgejo.CompareCommits), so the app rig's history does not have
// to be on this host.

const (
	// stagingRunPage is how many staging runs one repo's query asks for. A
	// repo deploys staging once per landing, so this is a deep history at the
	// tracker's scale: the tracker only ever asks about landings from the last
	// hour of the daemon log.
	stagingRunPage = 50

	// stagingSkew is how far before a landing a staging run may have been
	// created and still be asked whether it contains it. A run's commit is the
	// tip of main when the run is created, so a run created before the landing
	// cannot contain it; the allowance is only for clock skew between Forgejo
	// and the daemon log the landing time is read from.
	stagingSkew = 5 * time.Minute

	// stagingTimeout bounds one repo's read — its run list and every compare
	// the answer needs. Shorter than the Deploys block's refresh budget
	// because the Landings table asks this on the same poll.
	stagingTimeout = 2 * time.Second

	// stagingTTL is how long a rig's read is reused. A staging deploy takes
	// minutes, so nothing is learned by asking more often than this; a failed
	// read is held for the same time so a Forgejo that is down costs one
	// timeout per window rather than one per row.
	stagingTTL = 30 * time.Second
)

// A staging ship state: what the reader reports for every landing in a rig
// with a staging ship definition. A rig without one reports nothing at all,
// which the Landings table draws as a dash.
const (
	// StagingDeployed is a landing a successful staging run has covered.
	StagingDeployed = "deployed"
	// StagingPending is a landing no successful run has covered yet.
	StagingPending = "pending"
	// StagingFailed is a landing whose newest covering run failed.
	StagingFailed = "failed"
)

// The API's own run states the reader tells apart: a run that ended well, and
// one that ended badly. Every other state — waiting, running, skipped, or
// canceled — is neither shipped nor failed, so the landing stays pending.
const (
	stagingSuccess = "success"
	stagingFailure = "failure"
)

// StagingShip is what a rig's staging workflow has done with one landed
// commit.
//
// At is the covering successful run's finish, set only for StagingDeployed;
// RunState is the failing run's own state, set only for StagingFailed and
// drawn on the cell's title.
type StagingShip struct {
	State    string
	At       time.Time
	RunState string
}

// stagingAPI is the part of the Forgejo client the staging ship reader calls:
// one repo's runs, and the compare that decides whether a run's commit
// contains a landing.
type stagingAPI interface {
	ListRuns(ctx context.Context, owner, repo string, f forgejo.RunFilter) (*forgejo.RunList, error)
	CompareCommits(ctx context.Context, owner, repo, base, head string) (*forgejo.CompareInfo, error)
}

// StagingReader answers a landing's staging state for the rigs whose
// repository it was given, caching both the run list and the answer per
// landing because the Landings table asks once per row on every poll.
type StagingReader struct {
	api   stagingAPI
	repos map[string]string // rig name -> owner/name
	now   func() time.Time
	ttl   time.Duration

	mu    sync.Mutex
	runs  map[string]stagingRepoRuns // owner/name -> the repo's staging runs
	ships map[string]stagingShipRead // rig and commit -> the answer for it
}

// stagingRepoRuns is one repo's read: its staging runs on main, oldest first,
// and whether it has any. has is false both for a repo the viewer cannot read
// and for one that has never run the workflow — the two are the same answer to
// "does this rig ship to staging?" (gt-lqqjj), and the reader does not pretend
// to tell them apart.
type stagingRepoRuns struct {
	has  bool
	runs []forgejo.ActionRun
	at   time.Time
}

// stagingShipRead is one landing's cached answer, keyed by the landing and the
// time it landed, since that time bounds which runs can cover it.
type stagingShipRead struct {
	ship StagingShip
	ok   bool
	at   time.Time
}

// NewStagingReader builds a reader over api for the rigs in repos, each mapped
// to its Forgejo repository as owner/name.
func NewStagingReader(api stagingAPI, repos map[string]string) *StagingReader {
	return &StagingReader{
		api:   api,
		repos: repos,
		now:   time.Now,
		ttl:   stagingTTL,
		runs:  map[string]stagingRepoRuns{},
		ships: map[string]stagingShipRead{},
	}
}

// Ship reports what rig's staging workflow has done with commit, which landed
// at landed. ok is false for a rig with no staging ship definition — one whose
// repository could not be read, or whose repository has no staging runs —
// which the Landings table draws as a dash rather than as a landing waiting to
// ship.
func (r *StagingReader) Ship(rig, commit string, landed time.Time) (StagingShip, bool) {
	if r == nil {
		return StagingShip{}, false
	}
	name, ok := r.repos[rig]
	if !ok || commit == "" {
		return StagingShip{}, false
	}
	key := rig + "\x00" + commit + "\x00" + landed.UTC().Format(time.RFC3339Nano)
	now := r.now()
	if cached, ok := r.cachedShip(key, now); ok {
		return cached.ship, cached.ok
	}
	ship, ok := r.read(name, commit, landed)
	r.store(key, stagingShipRead{ship: ship, ok: ok, at: now})
	return ship, ok
}

// cachedShip returns a landing's answer while it is fresh.
func (r *StagingReader) cachedShip(key string, now time.Time) (stagingShipRead, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.ships[key]
	if !ok || now.Sub(entry.at) >= r.ttl {
		return stagingShipRead{}, false
	}
	return entry, true
}

func (r *StagingReader) store(key string, entry stagingShipRead) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ships[key] = entry
}

// read answers one landing's staging state off the repo's runs. The whole
// answer — the run list and every compare — shares one deadline, so a slow
// Forgejo cannot hold the poll past it.
func (r *StagingReader) read(name, commit string, landed time.Time) (StagingShip, bool) {
	runs, has := r.repoRuns(name)
	if !has {
		// No staging ship definition: the repo could not be read, or it has
		// never run the workflow. The landing shows no ship time at all.
		return StagingShip{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), stagingTimeout)
	defer cancel()

	// Oldest first, so the earliest successful covering run is the first
	// success the walk meets, and the last covering run it meets is the newest.
	cutoff := landed.Add(-stagingSkew)
	var (
		deployed bool
		at       time.Time
		newest   *forgejo.ActionRun
	)
	for i := range runs {
		run := &runs[i]
		if parseRunStamp(run.Created).Before(cutoff) {
			continue
		}
		covers, ok := r.covers(ctx, name, run.CommitSHA, commit)
		if !ok || !covers {
			// An unanswered compare is not coverage, the same way an
			// unanswered ancestry leaves an install's landing waiting.
			continue
		}
		newest = run
		if !deployed && run.Status == stagingSuccess {
			deployed, at = true, stagingFinish(*run)
		}
	}
	switch {
	case deployed:
		return StagingShip{State: StagingDeployed, At: at}, true
	case newest != nil && newest.Status == stagingFailure:
		return StagingShip{State: StagingFailed, RunState: newest.Status}, true
	default:
		return StagingShip{State: StagingPending}, true
	}
}

// repoRuns returns a repo's staging runs on main, oldest first, and whether it
// has any, reusing a read within the TTL.
func (r *StagingReader) repoRuns(name string) ([]forgejo.ActionRun, bool) {
	now := r.now()
	r.mu.Lock()
	entry, ok := r.runs[name]
	r.mu.Unlock()
	if ok && now.Sub(entry.at) < r.ttl {
		return entry.runs, entry.has
	}
	runs, readable := r.fetchRuns(name)
	// The ship definition is the repository's staging runs, not just a
	// readable repository: a repo that answered with none — devops today —
	// has no staging deploy to date a landing by, so its landings show no
	// ship time at all rather than reading as pending forever (gt-lqqjj).
	has := readable && len(runs) > 0
	r.mu.Lock()
	r.runs[name] = stagingRepoRuns{has: has, runs: runs, at: now}
	r.mu.Unlock()
	return runs, has
}

// fetchRuns reads one repo's staging workflow runs and keeps the ones on main,
// oldest first. readable is false for a read that failed: a repository the
// viewer cannot see is a rig with no ship definition here, which is what the
// Landings table shows for it today.
func (r *StagingReader) fetchRuns(name string) (runs []forgejo.ActionRun, readable bool) {
	owner, repo, ok := strings.Cut(name, "/")
	if !ok || owner == "" || repo == "" {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), stagingTimeout)
	defer cancel()
	list, err := r.api.ListRuns(ctx, owner, repo, forgejo.RunFilter{
		WorkflowID: stagingWorkflow,
		Limit:      stagingRunPage,
	})
	if err != nil {
		return nil, false
	}
	out := make([]forgejo.ActionRun, 0, len(list.Runs))
	for _, run := range list.Runs {
		// The workflow deploys main; a run of it on another ref is not the
		// landing deploy, and the API has no branch filter that says so here
		// (ref=main answers nothing — verified live), so it is filtered here.
		if run.PrettyRef != stagingRef {
			continue
		}
		out = append(out, run)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return parseRunStamp(out[i].Created).Before(parseRunStamp(out[j].Created))
	})
	return out, true
}

// covers reports whether the landed commit is contained in the commit a run
// built. The compare is asked backwards — base is the run's commit, head is
// the landing — because the answer's count is the commits the head has that
// the base does not: zero of them means the landing is an ancestor of the
// run's commit, which is exactly "this run built a main that contains it".
// ok is false for a compare that could not be answered.
func (r *StagingReader) covers(ctx context.Context, name, runCommit, landing string) (bool, bool) {
	owner, repo, ok := strings.Cut(name, "/")
	if !ok || runCommit == "" {
		return false, false
	}
	info, err := r.api.CompareCommits(ctx, owner, repo, runCommit, landing)
	if err != nil {
		return false, false
	}
	return info.TotalCommits == 0, true
}

// stagingRef is the branch the staging workflow deploys, as the API names it
// in a run's pretty ref.
const stagingRef = "main"

// stagingFinish is when a run ended: its stopped stamp, or its created stamp
// when the API sent no usable one, since a run the API calls successful has a
// finish and a date measured from the landing is worse than one measured from
// the start. A run that has not ended carries the zero stamp as Forgejo's own
// epoch ("1970-01-01T00:00:00Z"), which is not a finish — that is caught by
// comparing against the created stamp rather than by testing for zero.
func stagingFinish(run forgejo.ActionRun) time.Time {
	created := parseRunStamp(run.Created)
	if at := parseRunStamp(run.Stopped); at.After(created) {
		return at
	}
	return created
}
