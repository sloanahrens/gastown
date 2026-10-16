package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/forgejo"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/rig"
)

// The deploy watch (gt-hcbft): the landing worker's land/* gate sees only the
// candidate it merges, so the workflow that ships an app afterwards —
// staging.yml on main, deploy.yml or teardown.yml on a release tag,
// health.yml's staging check and its prod smoke — can fail with nobody
// watching.
//
// It reads through the read-only viewer client and never runs anything. One
// escalation per repository, workflow and ref class is raised by the newest
// failed run of that class and closed by the newest green one.

const (
	// deployWatchInterval is the gap between polls. A deploy run takes
	// minutes, so a shorter clock would re-read the same runs.
	deployWatchInterval = 5 * time.Minute

	// deployWatchTimeout bounds one poll and deployWatchCallTimeout one
	// Forgejo call inside it.
	deployWatchTimeout     = 60 * time.Second
	deployWatchCallTimeout = 20 * time.Second

	// deployWatchWindow is how far back a run counts. This watch is about a
	// deploy that has just failed, not the repository's history.
	deployWatchWindow = 24 * time.Hour

	// deployWatchRunLimit is runs per page and deployWatchRunPages the most
	// pages one poll reads per repository. The run list has no ref filter that
	// answers for a branch deploy: the route's ref parameter returns nothing
	// for ref=main against a repository whose staging.yml has run on main
	// (Forgejo 16.0.5, read live while building gt-hcbft), so runs are listed
	// unfiltered and a busy repository can hold more of them inside the window
	// than one page returns. A page is followed by the next while its oldest
	// run is still inside the window, so a quiet repository costs one call and
	// a busy one is read until the window is covered.
	deployWatchRunLimit = 20
	deployWatchRunPages = 5

	// deployWatchBlindPolls is how many polls in a row a rig's repository may
	// go unread before the watch escalates that it is blind to it: one failed
	// poll is a Forgejo restart, and a run of them is a repository the viewer
	// cannot read at all.
	deployWatchBlindPolls = 3

	// deployWatchBrokenKeyPrefix keys that escalation:
	// deploy-watch-broken:<rig>. It is low severity because a blind watch has
	// no failure to report, only the loss of the reads that would report one.
	deployWatchBrokenKeyPrefix = "deploy-watch-broken:"

	// deployWatchSource is the source every escalation this watch raises
	// carries.
	deployWatchSource = "deploy-watch"

	// deployFailedKeyPrefix keys one alert per repository, workflow and ref
	// class: deploy-failed:<owner/repo>:<workflow>:<class>, the class suffix
	// only for the one workflow that deploys two of them (deployAlertKey).
	deployFailedKeyPrefix = "deploy-failed:"

	// deployMessageMax caps a commit title in an escalation message and
	// deploySummaryMax the item's one line, both of which a repository
	// chooses.
	deployMessageMax = 120
	deploySummaryMax = 200

	// deployWatchStateFile is the watch's memory, under <town>/.runtime.
	deployWatchStateFile = "deploy-watch.json"
)

// The workflows the watch follows, and the ref classes each deploys from: a
// landing deploys main to staging, a release deploys a version tag, and
// health.yml checks both the staging deploy and the released artifact. Each
// class carries its own alert, because a green staging check says nothing
// about the released artifact (gt-hcbft).
var deployWorkflows = []deployWorkflow{
	{File: deployWorkflowStaging, Classes: []string{deployRefMain}},
	{File: deployWorkflowDeploy, Classes: []string{deployRefTag}},
	{File: deployWorkflowTeardown, Classes: []string{deployRefTag}},
	{File: deployWorkflowHealth, Classes: []string{deployRefMain, deployRefTag}},
}

// The workflow file names, as the API's workflow_id names them.
const (
	deployWorkflowStaging  = "staging.yml"
	deployWorkflowDeploy   = "deploy.yml"
	deployWorkflowTeardown = "teardown.yml"
	deployWorkflowHealth   = "health.yml"
)

// health.yml runs two jobs with different meanings (dv-4qtt): the post-landing
// staging check, and the released artifact's smoke test. A named prod smoke
// raises the alert's severity wherever the run happened; no job name lowers it
// (deploySeverity).
const (
	deployHealthProdJob = "prod-smoke"
)

// The run statuses the watch reads: one that ended badly, one that ended well.
// Every other status — waiting, running, skipped, blocked — is neither, and
// leaves the alert as it is rather than clearing it.
const (
	deployRunFailed    = "failure"
	deployRunCancelled = "cancelled" //nolint:misspell // the status the Forgejo API reports
	deployRunSucceeded = "success"
)

// The ref classes a deploy run starts from: the main a landing moved, and a
// release's version tag.
const (
	deployRefMain = "main"
	deployRefTag  = "tag"
)

// deployVersionTagRe is a release tag: a v, then a dotted version (v0.1.3, or
// v0.1.3-rc1 for a candidate). It is what tells a deploy tag from a branch a
// person named — "vendor" and "v2-migration" are branches — because a run
// carries no tag flag and its pretty ref is all the API gives.
var deployVersionTagRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+`)

// deployRunAPI is the part of the Forgejo client the watch calls: one
// repository's runs, and the jobs of a run it is about to alert on.
type deployRunAPI interface {
	ListRuns(ctx context.Context, owner, repo string, f forgejo.RunFilter) (*forgejo.RunList, error)
	ListRunJobs(ctx context.Context, owner, repo string, runID int64) ([]forgejo.ActionRunJob, error)
}

// deployRepo is one rig's Forgejo repository, as the rig's own config names it.
type deployRepo struct {
	// Rig is the rig that lands and deploys here, the name a reader knows.
	Rig string
	// Owner and Name are the repository's two halves.
	Owner string
	Name  string
	// APIBase is the API root of the instance that hosts it, derived from the
	// rig's own remote_url: a town whose rigs sit on two Forgejo instances
	// reads each one where it lives, and a same-named repository on the other
	// instance is never read as this rig's.
	APIBase string
}

// repo is owner/name, how the alert key and the API name the repository.
func (r deployRepo) repo() string { return r.Owner + "/" + r.Name }

// deployWorkflow is one followed workflow and the ref classes it deploys from.
type deployWorkflow struct {
	File    string
	Classes []string
}

// deployFailure is one workflow whose newest run of one ref class in the
// window failed: what the escalation says and what the attention item shows.
type deployFailure struct {
	Rig      string
	Repo     string // owner/name
	Workflow string
	Class    string // a deployRef* class
	Key      string // deploy-failed:<repo>:<workflow>[:<class>]
	Severity string // a config.Severity*
	RunID    int64
	Summary  string // the item's one line
	Message  string // the escalation's text, first line the title
}

// deployOpenRun is what the watch remembers about one raised alert: the run
// the escalation names, the severity it was raised at, and whether the
// high-severity push went out for it. All of it is persisted, so a daemon
// restart neither re-raises a standing failure nor pushes it again.
//
// The rest is what the attention item shows. A failed run ages out of the
// window well before anyone fixes the deploy, and the escalation outlives it,
// so the item has to outlive the run.
type deployOpenRun struct {
	Run      int64  `json:"run"`
	Pushed   bool   `json:"pushed,omitempty"`
	Severity string `json:"severity,omitempty"`
	Rig      string `json:"rig,omitempty"`
	Repo     string `json:"repo,omitempty"`
	Workflow string `json:"workflow,omitempty"`
	Class    string `json:"class,omitempty"`
	Summary  string `json:"summary,omitempty"`
}

// deployWatchState is that memory as the state file holds it.
type deployWatchState struct {
	Open map[string]deployOpenRun `json:"open,omitempty"`
	// Blind names the rigs whose blind escalation is open. That bead outlives
	// the daemon, so the watch that raised it has to remember it across a
	// restart: a fresh watch that reads the rig again would otherwise take it
	// for one it was never blind to and leave the bead claiming a rig whose
	// deploys go unseen (blindRead).
	Blind []string `json:"blind,omitempty"`
}

// deployWatch polls the app rigs' deploy workflows. Every seam is a function
// value so the unit tier drives it with a fake Forgejo and a recorded
// escalation, and no test opens a socket or writes a bead.
type deployWatch struct {
	// API is the read-only client for one Forgejo instance, built on first
	// use and cached by the implementation (see deployClients). The watch
	// calls it once per repository, with the host that repository's own
	// config names, rather than reading every rig through one instance.
	API func(apiBase string) (deployRunAPI, error)
	// Repos is the repositories to poll, resolved once per poll so a rig
	// added to the town is watched without a restart.
	Repos func() []deployRepo
	// Raise files one escalation under key at severity. Its error is the
	// delivery: a raise that did not land is retried by the next poll.
	Raise func(severity, key, message string) error
	// Clear closes the alert keyed key, because a newer run of that workflow
	// succeeded.
	Clear func(key, reason string) error
	// Push sends the high-severity push, nil pushing nothing (a town whose
	// operational.overseer.silent_push names no command).
	Push func(title, body string) error
	// State is the file the open alerts are remembered in; empty keeps them
	// in memory, which is what a caller with no town root wants.
	State string
	Logf  func(format string, args ...any)
	Now   func() time.Time

	// picMu guards failing alone: the watch's own goroutine writes the picture
	// in poll and the attention collector reads it from the heartbeat
	// goroutine, which must never wait on a retrying escalation. Everything
	// else here is that one goroutine's, read and written by no other — the
	// invariant note and heldOpen read open on without this lock.
	picMu sync.RWMutex
	// open is the raised alerts by key, and failing the last poll's picture
	// per rig. A rig whose list failed keeps the picture it had, and one the
	// town no longer lists loses it (poll).
	open    map[string]deployOpenRun
	failing map[string][]deployFailure
	// blind is the rigs whose reads are failing, by rig, as the read a poll
	// could not make (blindRead).
	blind map[string]deployBlindWatch
	// jobs is the failing job names of a run, cached by run id (failingJobs),
	// and settled the alerts already closed or already offered a clear.
	jobs    map[int64][]string
	settled map[string]bool
	// stateLost is true when the state file was missing or unreadable, so the
	// watch cannot know which alerts it raised. It clears a green workflow
	// once anyway then: a town whose state file was deleted but whose
	// escalation beads are open still closes them (see clearIfOpen).
	stateLost bool
	loaded    bool
	dirty     bool
	// lastLog is the line each key last wrote, so one that stands still is
	// said once (logOnce).
	lastLog map[string]string
}

// deployBlindWatch is one rig's unread repository: the polls in a row that
// read nothing from it (polls), and whether the blind escalation was raised
// for them (raised). The count lives in memory alone, so a restarted daemon
// counts from zero; raised is what the state file keeps, so the daemon that
// restarts while the bead is open still closes it when the read comes back
// (blindRead).
type deployBlindWatch struct {
	polls  int
	raised bool
}

// newDeployWatch returns a watch with its state initialized.
func newDeployWatch() *deployWatch {
	return &deployWatch{
		open:    map[string]deployOpenRun{},
		failing: map[string][]deployFailure{},
		blind:   map[string]deployBlindWatch{},
		jobs:    map[int64][]string{},
		settled: map[string]bool{},
		lastLog: map[string]string{},
	}
}

// now returns the watch's clock, defaulted so a caller that sets no Now still
// gets a real one.
func (w *deployWatch) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

// logOnce writes one line per key and message: a Forgejo that is down would
// otherwise repeat the same line every interval for as long as it stays down.
// The key is what the line is about — a rig, or the alert it names — because a
// single remembered message let the last writer silence every other, so two
// rigs answering nothing logged as one.
func (w *deployWatch) logOnce(key, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if w.lastLog[key] == msg {
		return
	}
	w.lastLog[key] = msg
	if w.Logf != nil {
		w.Logf("deploy_watch: %s", msg)
	}
}

// run polls until ctx ends. The first poll runs at once, so a daemon restarted
// for an upgrade reads the deploys that failed while it was down.
func (w *deployWatch) run(ctx context.Context) {
	for {
		w.pollOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(deployWatchInterval):
		}
	}
}

// pollOnce bounds one poll's Forgejo reads. The escalations the poll raises
// carry their own retry budget (gt escalate retries for up to a few minutes
// under load) and are not bounded by this: a poll that is slow to alert
// delays the next one, and two polls never overlap.
func (w *deployWatch) pollOnce(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, deployWatchTimeout)
	defer cancel()
	w.poll(ctx)
}

// poll reads every rig's repository and acts on what it finds. A repository
// whose read failed is left out of the picture rather than counted as green:
// an unanswered question must not close a real alert or raise a new one.
func (w *deployWatch) poll(ctx context.Context) {
	w.load()

	now := w.now()
	repos := w.Repos()
	fresh := map[string][]deployFailure{}
	watched := make(map[string]bool, len(repos))
	for _, repo := range repos {
		watched[repo.Rig] = true
		runs, ok := w.runs(ctx, repo, now)
		w.blindRead(repo, ok)
		if !ok {
			continue
		}
		fresh[repo.Rig] = w.verdicts(ctx, repo, runs, now)
	}
	w.picMu.Lock()
	// A rig the watch no longer polls leaves the picture with its alerts: no
	// later poll would ever drop them.
	for rig := range w.failing {
		if !watched[rig] {
			delete(w.failing, rig)
		}
	}
	for rig, failures := range fresh {
		w.failing[rig] = failures
	}
	w.picMu.Unlock()
	// A rig the town no longer lists is read by nobody and watched by nothing,
	// so its count goes with the picture: the escalation it raised stays open
	// with this rig's other ones.
	for rig, b := range w.blind {
		if !watched[rig] {
			// The bead stays open — a repository that vanished is not a green
			// deploy — but this watch is no longer the one holding it, so the
			// state file drops it with the rig.
			if b.raised {
				w.dirty = true
			}
			delete(w.blind, rig)
		}
	}
	w.pruneJobs()
	if w.dirty {
		w.save()
	}
}

// pruneJobs drops the cached job names of runs that no longer decide a
// workflow, so the cache holds this poll's failures and nothing older.
func (w *deployWatch) pruneJobs() {
	live := map[int64]bool{}
	w.picMu.RLock()
	for _, failures := range w.failing {
		for _, f := range failures {
			live[f.RunID] = true
		}
	}
	w.picMu.RUnlock()
	for id := range w.jobs {
		if !live[id] {
			delete(w.jobs, id)
		}
	}
}

// runs lists one repository's runs, a page at a time until the window is
// covered. ok is false for a read this poll cannot judge: the repository
// answers nothing, or the read failed. Neither is a green deploy.
func (w *deployWatch) runs(ctx context.Context, repo deployRepo, now time.Time) ([]forgejo.ActionRun, bool) {
	api, err := w.API(repo.APIBase)
	if err != nil {
		w.logOnce(repo.Rig, "%s: no client for %s: %v", repo.Rig, repo.APIBase, err)
		return nil, false
	}
	var out []forgejo.ActionRun
	for page := 1; page <= deployWatchRunPages; page++ {
		runs, ok := w.runPage(ctx, api, repo, page)
		if !ok {
			return nil, false
		}
		out = append(out, runs...)
		if len(runs) < deployWatchRunLimit || !deployPageInsideWindow(runs, now) {
			return out, true
		}
	}
	w.logOnce(repo.Rig, "%s %s: %d full pages reach only %s back, so a deploy that failed before them is not watched",
		repo.Rig, repo.repo(), deployWatchRunPages, deployWatchWindow)
	return out, true
}

// runPage lists one page of a repository's runs. ok is false for a page the
// poll cannot judge, which ends the repository's read: half a list must not be
// judged as a whole one.
func (w *deployWatch) runPage(ctx context.Context, api deployRunAPI, repo deployRepo, page int) ([]forgejo.ActionRun, bool) {
	ctx, cancel := context.WithTimeout(ctx, deployWatchCallTimeout)
	defer cancel()
	list, err := api.ListRuns(ctx, repo.Owner, repo.Name, forgejo.RunFilter{Limit: deployWatchRunLimit, Page: page})
	if err != nil {
		var apiErr *forgejo.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == 404 {
			// The rig's own config names this repository, so a 404 is a
			// repository the viewer cannot read — a blind watch, which says so
			// rather than passing for a rig that watches nothing.
			w.logOnce(repo.Rig, "%s: %s answers 404 on %s: the viewer cannot read the repository the rig names",
				repo.Rig, repo.repo(), repo.APIBase)
			return nil, false
		}
		w.logOnce(repo.Rig, "%s %s: reading runs: %v", repo.Rig, repo.repo(), err)
		return nil, false
	}
	return list.Runs, true
}

// blindRead records how this poll's read of repo went, and escalates a
// repository the watch has gone deployWatchBlindPolls polls without reading.
// The run list reports the failures it read and nothing at all about the reads
// that never happened, so a repository that always answers nothing holds the
// picture of its last good poll standing — which reads exactly like a green
// deploy. The escalation is keyed per rig and dropped by the first poll that
// reads the repository again.
func (w *deployWatch) blindRead(repo deployRepo, ok bool) {
	cur := w.blind[repo.Rig]
	key := deployWatchBrokenKeyPrefix + repo.Rig
	if ok {
		if !cur.raised {
			delete(w.blind, repo.Rig)
			return
		}
		reason := fmt.Sprintf("%s: %s answers again, so this rig's deploys are watched",
			repo.Rig, repo.repo())
		if err := w.Clear(key, reason); err != nil {
			// The alert is open and the clear did not land, so the count goes
			// and the raised flag stays: a read that fails again now reports no
			// second blindness while that alert nobody closed is still open.
			w.blind[repo.Rig] = deployBlindWatch{raised: true}
			w.logOnce(key, "clearing %s: %v", key, err)
			return
		}
		delete(w.blind, repo.Rig)
		w.dirty = true
		w.logOnce(key, "%s: cleared (%s)", key, reason)
		return
	}
	cur.polls++
	if cur.raised || cur.polls < deployWatchBlindPolls {
		w.blind[repo.Rig] = cur
		return
	}
	message := fmt.Sprintf("%s: %d polls in a row read no runs from %s, so a deploy of this rig that fails now goes unseen.\nThe read error is in the deploy_watch log.\nThe first poll that reads %s again closes this.",
		repo.Rig, cur.polls, repo.repo(), repo.repo())
	if err := w.Raise(config.SeverityLow, key, message); err != nil {
		w.logOnce(key, "%s: raising %s: %v", key, config.SeverityLow, err)
		w.blind[repo.Rig] = cur
		return
	}
	cur.raised = true
	w.blind[repo.Rig] = cur
	// The state file carries the bead this poll just raised, so the daemon
	// that replaces this one closes it rather than starting out blind to
	// nothing.
	w.dirty = true
	w.logOnce(key, "%s: blind for %d polls on %s", repo.Rig, cur.polls, repo.repo())
}

// deployPageInsideWindow is whether a page's oldest run still falls inside the
// window, so a page behind it could hold a run that counts. The route lists
// runs newest first, which is what makes that oldest run the edge the rest of
// the window lies behind. A page whose stamps none parse is not followed:
// newestDeployRun reads no run without one, so such a page is the end of what
// the watch can judge either way.
func deployPageInsideWindow(runs []forgejo.ActionRun, now time.Time) bool {
	var oldest time.Time
	for _, run := range runs {
		created := parseDeployStamp(run.Created)
		if created.IsZero() {
			continue
		}
		if oldest.IsZero() || created.Before(oldest) {
			oldest = created
		}
	}
	return !oldest.IsZero() && !oldest.Before(now.Add(-deployWatchWindow))
}

// verdicts turns one repository's runs into the failures that hold now, and
// raises or clears the alert each workflow and ref class's newest run decides.
// A pair with no run in the window says nothing either way: an alert it raised
// stays open until a run answers it.
func (w *deployWatch) verdicts(ctx context.Context, repo deployRepo, runs []forgejo.ActionRun, now time.Time) []deployFailure {
	var out []deployFailure
	// present is the keys this poll found a failed run for. Everything else
	// the watch holds open keeps the item it was raised with (heldOpen).
	present := map[string]bool{}
	for _, wf := range deployWorkflows {
		for _, class := range wf.Classes {
			run, ok := newestDeployRun(runs, wf, class, now)
			if !ok {
				continue
			}
			key := deployAlertKey(repo, wf, class)
			switch run.Status {
			case deployRunFailed, deployRunCancelled:
				jobs, jobsRead := w.failingJobs(ctx, repo, run)
				severity := deploySeverity(wf, class, jobs)
				if severity == "" {
					continue
				}
				if !jobsRead {
					severity = w.standingSeverity(key, run.ID, severity)
				}
				f := deployFailure{
					Rig:      repo.Rig,
					Repo:     repo.repo(),
					Workflow: wf.File,
					Class:    class,
					Key:      key,
					Severity: severity,
					RunID:    run.ID,
					Summary:  deploySummary(repo.repo(), wf.File, run),
					Message:  deployMessage(repo.repo(), wf.File, run, class, jobs),
				}
				w.note(f)
				present[key] = true
				out = append(out, f)
			case deployRunSucceeded:
				w.clearIfOpen(key, fmt.Sprintf("%s: run %d of %s succeeded on %s, so the deploy is green again",
					repo.repo(), run.ID, wf.File, run.PrettyRef))
			}
		}
	}
	return append(out, w.heldOpen(repo, present)...)
}

// heldOpen is the alerts of this repository that this poll found no failed run
// for: the run aged out of the window, a newer one is still in flight, or the
// clear that would close it did not land. The escalation outlives all three,
// so the item stays in the picture while the alert is open: an alert still
// waiting on a human must not go quiet in the dashboard when its run scrolls
// off the list.
func (w *deployWatch) heldOpen(repo deployRepo, present map[string]bool) []deployFailure {
	var out []deployFailure
	for _, key := range sortedKeys(w.open) {
		open := w.open[key]
		// A record from a state file written before this field existed has no
		// summary to show, and one this poll found is already in the picture.
		if present[key] || open.Repo != repo.repo() || open.Summary == "" {
			continue
		}
		out = append(out, deployFailure{
			Rig:      open.Rig,
			Repo:     open.Repo,
			Workflow: open.Workflow,
			Class:    open.Class,
			Key:      key,
			Severity: open.Severity,
			RunID:    open.Run,
			Summary:  open.Summary,
		})
	}
	return out
}

// failingJobs names the jobs of run that failed, in the API's order, the
// first of which the escalation names. A read that fails yields no names: the
// run is still a failure, and the alert is worth raising without them.
//
// The names are cached per run and pruned once the run is no longer the one
// deciding its workflow, so a failure that stands — which is the common case,
// since it stands until someone fixes the deploy — costs one jobs call, not
// one per poll. An unread answer is not cached, so the next poll retries it.
// read reports whether the watch actually read the list: an empty one an API
// answered is an answer, and a failed call is not (standingSeverity).
func (w *deployWatch) failingJobs(ctx context.Context, repo deployRepo, run forgejo.ActionRun) (names []string, read bool) {
	if names, ok := w.jobs[run.ID]; ok {
		return names, true
	}
	api, err := w.API(repo.APIBase)
	if err != nil {
		w.logOnce(repo.Rig, "%s: no client for %s: %v", repo.Rig, repo.APIBase, err)
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, deployWatchCallTimeout)
	defer cancel()
	jobs, err := api.ListRunJobs(ctx, repo.Owner, repo.Name, run.ID)
	if err != nil {
		w.logOnce(repo.Rig, "%s %s: run %d jobs: %v", repo.Rig, repo.repo(), run.ID, err)
		return nil, false
	}
	var out []string
	for _, job := range jobs {
		if job.Status == deployRunFailed || job.Status == deployRunCancelled {
			out = append(out, job.Name)
		}
	}
	if w.jobs == nil {
		w.jobs = map[int64][]string{}
	}
	w.jobs[run.ID] = out
	return out, true
}

// standingSeverity is the level to alert key at when this poll could not read
// the run's jobs. deploySeverity raises a health.yml failure to high only when
// the prod smoke is named, so an unread list is no reading at all: reported as
// the medium of a class without one, it corrects a standing high alert — and
// the bead the key names — down on the strength of a daemon restart (gt-xkikw).
// The standing high is carried only onto the run that earned it; a key with
// nothing open, or open for another run, keeps what the workflow and the class
// imply (gt-t2u66).
func (w *deployWatch) standingSeverity(key string, runID int64, severity string) string {
	if open, known := w.open[key]; known && open.Run == runID && open.Severity == config.SeverityHigh && severity != config.SeverityHigh {
		return config.SeverityHigh
	}
	return severity
}

// note raises or re-raises the alert for f, and pushes it if it is high. A
// repeated failure of the same run at the same severity is not a new alert:
// gt escalate records the repeat on the bead the key already names (gt-vwry),
// and the push goes out once per run.
//
// A run whose severity is read differently than last poll re-raises, so the
// bead the key names is corrected rather than left at what a first partial
// reading said. The state records a delivery, not an attempt: a raise or a
// push that failed is left unwritten, so the next poll retries it instead of
// suppressing it.
func (w *deployWatch) note(f deployFailure) {
	open, known := w.open[f.Key]
	if !known || open.Run != f.RunID || open.Severity != f.Severity {
		if err := w.Raise(f.Severity, f.Key, f.Message); err != nil {
			w.logOnce(f.Key, "%s: raising %s: %v", f.Key, f.Severity, err)
			return
		}
		if open.Run != f.RunID {
			// The push is owed once per run, so this run's failure owes its
			// own delivery: kept, the first run's would silence every failure
			// behind the same key. A severity the same run is read at
			// differently leaves that run's delivery where it is (gt-xkikw).
			open.Pushed = false
		}
		open.Run = f.RunID
		open.Severity = f.Severity
		w.dirty = true
		w.logOnce(f.Key, "%s: %s run %d failed on %s at %s", f.Rig, f.Workflow, f.RunID, f.Repo, f.Severity)
	}
	if open.Rig != f.Rig || open.Repo != f.Repo || open.Workflow != f.Workflow ||
		open.Class != f.Class || open.Summary != f.Summary {
		// What the item shows is part of the record, so a change to it is
		// written: a state file from a daemon that recorded no summary would
		// otherwise lose the item at the next restart.
		open.Rig, open.Repo, open.Workflow, open.Class, open.Summary =
			f.Rig, f.Repo, f.Workflow, f.Class, f.Summary
		w.dirty = true
	}
	// A town with no push command owes no push, so nothing is recorded as
	// delivered: if the operator configures one while this failure stands, the
	// next poll pushes it.
	if f.Severity == config.SeverityHigh && !open.Pushed && w.Push != nil {
		if err := w.push(f); err != nil {
			w.logOnce(f.Key, "%s: push for run %d: %v", f.Key, f.RunID, err)
		} else {
			open.Pushed = true
			w.dirty = true
		}
	}
	w.open[f.Key] = open
}

// push sends the high-severity push through the command the silent-overseer
// rule already uses. A watch with no command pushes nothing, and owes
// nothing: the escalation is the alert either way.
func (w *deployWatch) push(f deployFailure) error {
	if w.Push == nil {
		return nil
	}
	return w.Push(fmt.Sprintf("Deploy failed: %s %s", f.Repo, f.Workflow), f.Message)
}

// clearIfOpen closes the alert keyed key, and forgets it only once the clear
// landed, so a dropped clear is retried by the next poll.
//
// A key this watch does not remember as open is cleared once anyway while the
// state file was lost: an escalation bead raised by a daemon whose state file
// is gone must still be closable by the run that fixes the deploy.
func (w *deployWatch) clearIfOpen(key, reason string) {
	if _, open := w.open[key]; !open && (!w.stateLost || w.settled[key]) {
		return
	}
	if err := w.Clear(key, reason); err != nil {
		w.logOnce(key, "clearing %s: %v", key, err)
		return
	}
	delete(w.open, key)
	if w.settled == nil {
		w.settled = map[string]bool{}
	}
	w.settled[key] = true
	w.dirty = true
	w.logOnce(key, "%s: cleared (%s)", key, reason)
}

// failures is the picture the last poll left, for the attention queue: the
// failures that hold now, all rigs together.
func (w *deployWatch) failures() []deployFailure {
	w.picMu.RLock()
	defer w.picMu.RUnlock()
	var out []deployFailure
	for _, rig := range sortedKeys(w.failing) {
		out = append(out, w.failing[rig]...)
	}
	return out
}

// load reads the state file once. A missing file is a watch that has never
// run, and an unreadable one is logged and treated as empty: a state the
// watch cannot read must not stop it from watching. Both set stateLost, which
// is what lets a green run close an alert this process does not know about.
func (w *deployWatch) load() {
	if w.loaded {
		return
	}
	w.loaded = true
	if w.open == nil {
		w.open = map[string]deployOpenRun{}
	}
	if w.failing == nil {
		w.failing = map[string][]deployFailure{}
	}
	if w.settled == nil {
		w.settled = map[string]bool{}
	}
	if w.blind == nil {
		w.blind = map[string]deployBlindWatch{}
	}
	if w.State == "" {
		return
	}
	data, err := os.ReadFile(w.State)
	if err != nil {
		w.stateLost = true
		if !os.IsNotExist(err) {
			w.logOnce(w.State, "reading %s: %v", w.State, err)
		}
		return
	}
	var st deployWatchState
	if err := json.Unmarshal(data, &st); err != nil {
		w.stateLost = true
		w.logOnce(w.State, "reading %s: %v", w.State, err)
		return
	}
	if st.Open != nil {
		w.open = st.Open
	}
	// A rig the state file holds open starts back at no failed polls: the
	// count is this process's, and the flag is all a restart needs to close
	// the bead once the read returns.
	for _, rig := range st.Blind {
		w.blind[rig] = deployBlindWatch{raised: true}
	}
}

// blindRigs is the rigs whose blind escalation this watch is holding open, for
// the state file: the raised entries, in a stable order so a rewrite of the
// same memory is the same bytes.
func (w *deployWatch) blindRigs() []string {
	out := make([]string, 0, len(w.blind))
	for rig, b := range w.blind {
		if b.raised {
			out = append(out, rig)
		}
	}
	slices.Sort(out)
	return out
}

// save writes the state file. A write that fails is logged and retried on the
// next poll that changes something.
func (w *deployWatch) save() {
	w.dirty = false
	if w.State == "" {
		return
	}
	if err := atomicfile.EnsureDirAndWriteJSON(w.State, deployWatchState{Open: w.open, Blind: w.blindRigs()}); err != nil {
		w.logOnce(w.State, "writing %s: %v", w.State, err)
	}
}

// deployWatchStatePath is <town>/.runtime/deploy-watch.json.
func deployWatchStatePath(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", deployWatchStateFile)
}

// deployAlertKey is the alert's stable identity: one per repository, workflow
// and ref class, so a workflow that fails twice names one escalation. The
// class suffix appears only when the workflow deploys from more than one
// class, and only health.yml does: its staging check and its prod smoke are
// two deploys, and a green one of either is no answer about the other.
func deployAlertKey(repo deployRepo, wf deployWorkflow, class string) string {
	key := deployFailedKeyPrefix + repo.repo() + ":" + wf.File
	if len(wf.Classes) > 1 {
		key += ":" + class
	}
	return key
}

// newestDeployRun is the newest run of wf on the ref class class, inside the
// window. A run of the workflow on another ref — a land/<bead> candidate, a
// feature branch, health.yml's other environment — is not this deploy.
func newestDeployRun(runs []forgejo.ActionRun, wf deployWorkflow, class string, now time.Time) (forgejo.ActionRun, bool) {
	var best forgejo.ActionRun
	found := false
	for _, run := range runs {
		if deployRunWorkflow(run.WorkflowID) != wf.File {
			continue
		}
		if deployRefClass(run.PrettyRef) != class {
			continue
		}
		created := parseDeployStamp(run.Created)
		if created.IsZero() || created.Before(now.Add(-deployWatchWindow)) {
			continue
		}
		if !found || deployRunNewer(run, best) {
			best, found = run, true
		}
	}
	return best, found
}

// deployRunNewer orders two runs of one workflow by creation, the run id
// breaking an equal pair of stamps.
func deployRunNewer(a, b forgejo.ActionRun) bool {
	aCreated, bCreated := parseDeployStamp(a.Created), parseDeployStamp(b.Created)
	if !aCreated.Equal(bCreated) {
		return aCreated.After(bCreated)
	}
	return a.ID > b.ID
}

// workflowFileBase is the file name a run's workflow id carries, which the API
// reports as the workflow's path in the repository: the base name answers for
// ".forgejo/workflows/staging.yml" and "staging.yml" alike.
func workflowFileBase(workflowID string) string {
	if idx := strings.LastIndex(workflowID, "/"); idx >= 0 {
		return workflowID[idx+1:]
	}
	return workflowID
}

// deployRunWorkflow names the workflow a run belongs to by the base name the
// API reports for it, or "" for a workflow the watch does not follow.
func deployRunWorkflow(workflowID string) string {
	base := workflowFileBase(workflowID)
	if slices.ContainsFunc(deployWorkflows, func(wf deployWorkflow) bool { return wf.File == base }) {
		return base
	}
	return ""
}

// deployRefClass is the class of a run's ref: the main a landing moved, a
// version tag, or "" for a ref no deploy starts from.
func deployRefClass(ref string) string {
	switch {
	case ref == deployRefMain:
		return deployRefMain
	case deployVersionTagRe.MatchString(ref):
		return deployRefTag
	}
	return ""
}

// deploySeverity is what a failed run of wf on ref class class is worth, or ""
// for a failure this watch does not alert on: a workflow on a ref class it
// does not deploy from (staging does not run on a tag, a release does not run
// on main), or a workflow the watch does not follow.
//
// The severity comes from the workflow and the class alone, never from the
// jobs read, which is best-effort: an alarm whose level depends on a read that
// may fail is an alarm that can go out too quietly. A job name can only raise
// it — a prod smoke is a broken release wherever it ran — so the read can
// never talk the watch down.
func deploySeverity(wf deployWorkflow, class string, failingJobs []string) string {
	if !slices.Contains(wf.Classes, class) {
		return ""
	}
	switch wf.File {
	case deployWorkflowStaging:
		return config.SeverityMedium
	case deployWorkflowDeploy, deployWorkflowTeardown:
		return config.SeverityHigh
	case deployWorkflowHealth:
		if class == deployRefTag || slices.Contains(failingJobs, deployHealthProdJob) {
			return config.SeverityHigh
		}
		return config.SeverityMedium
	}
	return ""
}

// deploySummary is the attention item's one line, and the escalation's title:
// the repository, the workflow, the run and the ref it went red on.
func deploySummary(repo, workflow string, run forgejo.ActionRun) string {
	return deployLine(fmt.Sprintf("%s %s run %d %s on %s", repo, workflow, run.ID, deployStatusWord(run.Status), run.PrettyRef), deploySummaryMax)
}

// deployStatusWord is a run's own status as a sentence says it. A run in
// deployRunCancelled is one of the two statuses this watch alerts on, and
// calling it failed would name a failure the run never had.
func deployStatusWord(status string) string {
	if status == deployRunFailed {
		return "failed"
	}
	return status
}

// deployMessage is the escalation's text. Its first line becomes the bead's
// title, so it carries the repository, the workflow and the run; the body
// names the commit, the first failing job and the run's URL, which is what a
// reader needs to open the failure. Its last line names the run that closes
// it, which is a green run of the same workflow on the same ref class.
func deployMessage(repo, workflow string, run forgejo.ActionRun, class string, failingJobs []string) string {
	lines := []string{
		deploySummary(repo, workflow, run),
		fmt.Sprintf("commit: %s", deployLine(run.Title, deployMessageMax)),
	}
	if len(failingJobs) > 0 {
		lines = append(lines, "first failing job: "+deployLine(failingJobs[0], deployMessageMax))
	}
	if run.HTMLURL != "" {
		lines = append(lines, "run: "+deployLine(run.HTMLURL, deployMessageMax))
	}
	lines = append(lines, fmt.Sprintf("The next green run of %s on %s in %s closes this.", workflow, deployClassWords(class), repo))
	return strings.Join(lines, "\n")
}

// deployClassWords is a ref class as a sentence says it: a run happens on main
// or on a release tag.
func deployClassWords(class string) string {
	if class == deployRefTag {
		return "a release tag"
	}
	return class
}

// deployLine is what a repository's own text may contribute to an alert: one
// line, no control characters, capped, because the text becomes a bead title
// and a commit title is whatever its author wrote.
func deployLine(s string, max int) string {
	s = strings.Join(strings.Fields(deployPrintable(s)), " ")
	if runes := []rune(s); len(runes) > max {
		return string(runes[:max-1]) + "…"
	}
	return s
}

// deployPrintable drops the control characters a title may carry.
func deployPrintable(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// parseDeployStamp is a run's RFC3339 creation time, or the zero time for one
// the API did not send.
func parseDeployStamp(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// deployItemSeverity is an escalation severity as the attention queue's two
// levels: only a release breaking is worth the loud one.
func deployItemSeverity(severity string) attention.Severity {
	if severity == config.SeverityHigh {
		return attention.SeverityHigh
	}
	return attention.SeverityLow
}

// sortedKeys is a string-keyed map's keys in order, so a picture built from
// one is the same list every poll.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// collectDeployFailures is the deploy.failed collector: one item per workflow
// whose newest run has failed, taken from the watch's last poll. It reads no
// repository itself — the watch already did, and a collector that called
// Forgejo on every heartbeat would poll it every few seconds.
func (s *attentionSources) collectDeployFailures(context.Context) ([]attention.Item, error) {
	w := s.d.watchDeploy()
	if w == nil {
		return nil, nil
	}
	var out []attention.Item
	for _, f := range w.failures() {
		out = append(out, attention.Item{
			Key:      f.Key,
			Kind:     attention.KindDeployFailed,
			Severity: deployItemSeverity(f.Severity),
			Rig:      f.Rig,
			Summary:  f.Summary,
		})
	}
	return out, nil
}

// startDeployWatch starts the watch once per daemon, from the landing worker
// manager: the rigs it polls are the ones that land through Forgejo, so a town
// whose landing worker is off deploys nothing for it to watch.
//
// The manager calls this on every pass, and a pass that cannot build one is
// not the last: the first minutes of a daemon are exactly when a rig or the
// viewer token may be missing, and a watch that gave up there would be an
// alarm that is off. The hold is logged once per distinct reason.
func (d *Daemon) startDeployWatch() {
	if d.ctx == nil {
		return
	}
	d.deployWatchMu.Lock()
	defer d.deployWatchMu.Unlock()
	if d.deployW.Load() != nil {
		return
	}
	build := d.productionDeployWatch
	if d.deployWatchBuild != nil {
		build = d.deployWatchBuild
	}
	w, hold := build()
	if w == nil {
		if hold != d.deployWatchHold {
			d.deployWatchHold = hold
			d.logger.Printf("deploy_watch: not started: %s", hold)
		}
		return
	}
	d.deployWatchHold = ""
	d.deployW.Store(w)
	go w.run(d.ctx)
}

// watchDeploy is the watch the attention collector reads, nil until it starts.
func (d *Daemon) watchDeploy() *deployWatch {
	return d.deployW.Load()
}

// deployClients builds and caches one read-only viewer client per Forgejo
// instance the rigs name. A daemon whose rigs all sit on the local instance
// builds one client; a second instance gets its own, so no rig is read from
// another instance's repository of the same name.
type deployClients struct {
	// tokenDir holds the role's token file (default ~/.config/gt).
	tokenDir string

	mu     sync.Mutex
	byBase map[string]*forgejo.Client
}

// client is the viewer's client for apiBase, built on first use.
func (c *deployClients) client(apiBase string) (deployRunAPI, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cl, ok := c.byBase[apiBase]; ok {
		return cl, nil
	}
	opts := []forgejo.Option{forgejo.WithBaseURL(apiBase)}
	if c.tokenDir != "" {
		opts = append(opts, forgejo.WithTokenFile(filepath.Join(c.tokenDir, "forgejo-"+config.ForgejoRoleViewer+".env")))
	}
	cl, err := forgejo.NewClient(config.ForgejoRoleViewer, opts...)
	if err != nil {
		return nil, err
	}
	if c.byBase == nil {
		c.byBase = map[string]*forgejo.Client{}
	}
	c.byBase[apiBase] = cl
	return cl, nil
}

// productionDeployWatch wires the watch's collaborators: the read-only viewer
// client each rig's repository is read through, the daemon's own escalations
// and clears, and the push command the silent-overseer rule already uses. A
// watch it cannot build comes back nil with the reason, which the caller logs
// once and retries on (startDeployWatch).
//
// A town with no rig repository has nothing to watch, and one whose viewer
// token cannot be read could read no run: both are answered here, before a
// poll, rather than failing a read per repository per interval.
func (d *Daemon) productionDeployWatch() (*deployWatch, string) {
	townRoot := d.config.TownRoot
	clients := &deployClients{}
	if cfg := landingWorkerConfig(d.patrolConfig); cfg != nil {
		dir, err := cfg.ForgejoTokenDir()
		if err != nil {
			return nil, fmt.Sprintf("the configured Forgejo token dir: %v", err)
		}
		clients.tokenDir = dir
	} else if dir, err := config.DefaultForgejoTokenDir(); err == nil {
		clients.tokenDir = dir
	}
	repos := d.deployRepos()
	if len(repos) == 0 {
		return nil, "no rig names a Forgejo repository"
	}
	if _, err := clients.client(repos[0].APIBase); err != nil {
		return nil, fmt.Sprintf("no %s Forgejo token: %v", config.ForgejoRoleViewer, err)
	}
	w := newDeployWatch()
	w.API = clients.client
	w.Repos = d.deployRepos
	w.Raise = func(severity, key, message string) error {
		return d.escalateAlertSeverity(severity, key, deployWatchSource, message)
	}
	w.Clear = func(key, reason string) error { return d.clearAlertsErr(reason, key) }
	w.State = deployWatchStatePath(townRoot)
	w.Logf = d.logger.Printf
	w.Now = func() time.Time { return d.clk().Now() }
	if push := d.loadOperationalConfig().GetOverseerConfig().SilentPushName(); push != "" {
		w.Push = func(title, body string) error { return notify.PushIn(townRoot, push, title, body) }
	}
	return w, ""
}

// deployRepos is every rig the landing worker serves, with the Forgejo
// repository its config names. A rig with no block, or one whose remote URL
// names no repository, is left out and nothing is logged for it: the landing
// path reads the same block, and an unusable one is already escalated as
// landing-rig-config:<rig> by the worker that cannot land through it.
func (d *Daemon) deployRepos() []deployRepo {
	townRoot := d.config.TownRoot
	var out []deployRepo
	for _, name := range landingWorkerRigs(d.patrolConfig, d.getKnownRigs()) {
		fc := rig.ResolveForgejoConfig(townRoot, name)
		if fc == nil || fc.RemoteURL == "" {
			continue
		}
		owner, repo, err := land.RepoFromRemoteURL(fc.RemoteURL)
		if err != nil {
			continue
		}
		apiBase, err := land.APIBaseFromRemoteURL(fc.RemoteURL)
		if err != nil {
			continue
		}
		out = append(out, deployRepo{Rig: name, Owner: owner, Name: repo, APIBase: apiBase})
	}
	return out
}
