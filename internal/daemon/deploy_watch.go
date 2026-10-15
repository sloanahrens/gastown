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

// The deploy watch (gt-hcbft): the landing worker waits only on the land/*
// gate, so the run that actually ships an app after the merge — staging.yml on
// main, deploy.yml or teardown.yml on a release tag, health.yml's staging
// check and prod smoke — fails with nobody watching it. A failed staging run
// on fractals-nextjs (run 1153) waited two days for a human to notice.
//
// The watch reads each rig's repository through the read-only viewer client,
// one run list per repository per poll, and keeps one escalation per
// repository and workflow: raised by the newest failed run, closed by the
// newest green one. It reads and never runs anything, and it writes no bead
// of its own.

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

	// deployWatchRunLimit is how many runs one repository's list asks for.
	// The API names a workflow by its path in the repository and deploys no
	// branch filter (ref=main answers nothing — verified live), so one list
	// per repository is filtered here rather than one call per workflow.
	deployWatchRunLimit = 20

	// deployWatchSource is the source every escalation this watch raises
	// carries.
	deployWatchSource = "deploy-watch"

	// deployFailedKeyPrefix keys one alert per repository and workflow:
	// deploy-failed:<owner/repo>:<workflow>.
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
// health.yml checks both the staging deploy and the released artifact. That
// last one is the only workflow with two classes, and its alert is keyed by
// the workflow rather than the class, so the newest run of it decides either
// way: the next green health run closes it, which is what dv-4qtt asks for.
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
// staging check, and the released artifact's smoke test. The failing job's
// name decides the alert's severity, because a broken release is worth more
// than a broken staging.
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

// deployFailure is one workflow whose newest run in the window failed: what
// the escalation says and what the attention item shows.
type deployFailure struct {
	Rig      string
	Repo     string // owner/name
	Workflow string
	Key      string // deploy-failed:<repo>:<workflow>
	Severity string // a config.Severity*
	RunID    int64
	Summary  string // the item's one line
	Message  string // the escalation's text, first line the title
}

// deployOpenRun is what the watch remembers about one raised alert: the run
// the escalation names, and whether the high-severity push went out for it.
// Both are persisted, so a daemon restart neither re-raises a standing
// failure nor pushes it again.
type deployOpenRun struct {
	Run    int64 `json:"run"`
	Pushed bool  `json:"pushed,omitempty"`
}

// deployWatchState is that memory as the state file holds it.
type deployWatchState struct {
	Open map[string]deployOpenRun `json:"open,omitempty"`
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

	// picMu guards failing alone. The watch's own goroutine is the only
	// writer of everything else here — the state file, and the alerts it has
	// raised — while the attention collector reads this picture from the
	// heartbeat goroutine, which must never wait on a retrying escalation.
	picMu sync.RWMutex
	// open is the raised alerts by key, and failing the last poll's picture
	// per rig. A rig whose list failed keeps the picture it had (see poll).
	open    map[string]deployOpenRun
	failing map[string][]deployFailure
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
	lastLog   string
}

// newDeployWatch returns a watch with its state initialized.
func newDeployWatch() *deployWatch {
	return &deployWatch{
		open:    map[string]deployOpenRun{},
		failing: map[string][]deployFailure{},
		jobs:    map[int64][]string{},
		settled: map[string]bool{},
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

// logOnce writes one line per distinct message: a Forgejo that is down would
// otherwise repeat the same line every interval for as long as it stays down.
func (w *deployWatch) logOnce(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if msg == w.lastLog {
		return
	}
	w.lastLog = msg
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
	fresh := map[string][]deployFailure{}
	for _, repo := range w.Repos() {
		runs, ok := w.runs(ctx, repo)
		if !ok {
			continue
		}
		fresh[repo.Rig] = w.verdicts(ctx, repo, runs, now)
	}
	w.picMu.Lock()
	for rig, failures := range fresh {
		w.failing[rig] = failures
	}
	w.picMu.Unlock()
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

// runs lists one repository's runs. ok is false for a read this poll cannot
// judge: the repository is not on its Forgejo instance (404, which is how a
// rig that does not deploy here reads), or the read failed.
func (w *deployWatch) runs(ctx context.Context, repo deployRepo) ([]forgejo.ActionRun, bool) {
	api, err := w.API(repo.APIBase)
	if err != nil {
		w.logOnce("%s: no client for %s: %v", repo.Rig, repo.APIBase, err)
		return nil, false
	}
	ctx, cancel := context.WithTimeout(ctx, deployWatchCallTimeout)
	defer cancel()
	list, err := api.ListRuns(ctx, repo.Owner, repo.Name, forgejo.RunFilter{Limit: deployWatchRunLimit})
	if err != nil {
		var apiErr *forgejo.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == 404 {
			return nil, false
		}
		w.logOnce("%s %s: reading runs: %v", repo.Rig, repo.repo(), err)
		return nil, false
	}
	return list.Runs, true
}

// verdicts turns one repository's runs into the failures that hold now, and
// raises or clears the alert each workflow's newest run decides. A workflow
// with no run in the window says nothing either way: an alert it raised stays
// open until a run answers it.
func (w *deployWatch) verdicts(ctx context.Context, repo deployRepo, runs []forgejo.ActionRun, now time.Time) []deployFailure {
	var out []deployFailure
	for _, wf := range deployWorkflows {
		run, ok := newestDeployRun(runs, wf, now)
		if !ok {
			continue
		}
		key := deployAlertKey(repo, wf.File)
		switch run.Status {
		case deployRunFailed, deployRunCancelled:
			jobs := w.failingJobs(ctx, repo, run)
			severity := deploySeverity(wf, run.PrettyRef, jobs)
			if severity == "" {
				continue
			}
			f := deployFailure{
				Rig:      repo.Rig,
				Repo:     repo.repo(),
				Workflow: wf.File,
				Key:      key,
				Severity: severity,
				RunID:    run.ID,
				Summary:  deploySummary(repo.repo(), wf.File, run),
				Message:  deployMessage(repo.repo(), wf.File, run, jobs),
			}
			w.note(f)
			out = append(out, f)
		case deployRunSucceeded:
			w.clearIfOpen(key, fmt.Sprintf("%s: run %d of %s succeeded on %s, so the deploy is green again",
				repo.repo(), run.ID, wf.File, run.PrettyRef))
		}
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
func (w *deployWatch) failingJobs(ctx context.Context, repo deployRepo, run forgejo.ActionRun) []string {
	if names, ok := w.jobs[run.ID]; ok {
		return names
	}
	api, err := w.API(repo.APIBase)
	if err != nil {
		w.logOnce("%s: no client for %s: %v", repo.Rig, repo.APIBase, err)
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, deployWatchCallTimeout)
	defer cancel()
	jobs, err := api.ListRunJobs(ctx, repo.Owner, repo.Name, run.ID)
	if err != nil {
		w.logOnce("%s %s: run %d jobs: %v", repo.Rig, repo.repo(), run.ID, err)
		return nil
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
	return out
}

// note raises or re-raises the alert for f, and pushes it if it is high. A
// repeated failure of the same run is not a new alert: gt escalate records
// the repeat on the bead the key already names (gt-vwry), and the push goes
// out once per run.
//
// The state records a delivery, not an attempt: a raise or a push that failed
// is left unwritten, so the next poll retries it instead of suppressing it.
func (w *deployWatch) note(f deployFailure) {
	open, known := w.open[f.Key]
	if !known || open.Run != f.RunID {
		if err := w.Raise(f.Severity, f.Key, f.Message); err != nil {
			w.logOnce("%s: raising %s: %v", f.Key, f.Severity, err)
			return
		}
		open = deployOpenRun{Run: f.RunID}
		w.dirty = true
		w.logOnce("%s: %s run %d failed on %s", f.Rig, f.Workflow, f.RunID, f.Repo)
	}
	// A town with no push command owes no push, so nothing is recorded as
	// delivered: if the operator configures one while this failure stands, the
	// next poll pushes it.
	if f.Severity == config.SeverityHigh && !open.Pushed && w.Push != nil {
		if err := w.push(f); err != nil {
			w.logOnce("%s: push for run %d: %v", f.Key, f.RunID, err)
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
		w.logOnce("clearing %s: %v", key, err)
		return
	}
	delete(w.open, key)
	if w.settled == nil {
		w.settled = map[string]bool{}
	}
	w.settled[key] = true
	w.dirty = true
	w.logOnce("%s: cleared (%s)", key, reason)
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
	if w.State == "" {
		return
	}
	data, err := os.ReadFile(w.State)
	if err != nil {
		w.stateLost = true
		if !os.IsNotExist(err) {
			w.logOnce("reading %s: %v", w.State, err)
		}
		return
	}
	var st deployWatchState
	if err := json.Unmarshal(data, &st); err != nil {
		w.stateLost = true
		w.logOnce("reading %s: %v", w.State, err)
		return
	}
	if st.Open != nil {
		w.open = st.Open
	}
}

// save writes the state file. A write that fails is logged and retried on the
// next poll that changes something.
func (w *deployWatch) save() {
	w.dirty = false
	if w.State == "" {
		return
	}
	if err := atomicfile.EnsureDirAndWriteJSON(w.State, deployWatchState{Open: w.open}); err != nil {
		w.logOnce("writing %s: %v", w.State, err)
	}
}

// deployWatchStatePath is <town>/.runtime/deploy-watch.json.
func deployWatchStatePath(townRoot string) string {
	return filepath.Join(townRoot, ".runtime", deployWatchStateFile)
}

// deployAlertKey is the alert's stable identity: one per repository and
// workflow, so a workflow that fails twice names one escalation.
func deployAlertKey(repo deployRepo, workflow string) string {
	return deployFailedKeyPrefix + repo.repo() + ":" + workflow
}

// newestDeployRun is the newest run of wf on one of the ref classes it
// deploys from, inside the window. A run of the workflow on another ref — a
// land/<bead> candidate, a feature branch — is not that workflow's deploy.
func newestDeployRun(runs []forgejo.ActionRun, wf deployWorkflow, now time.Time) (forgejo.ActionRun, bool) {
	var best forgejo.ActionRun
	found := false
	for _, run := range runs {
		if deployRunWorkflow(run.WorkflowID) != wf.File {
			continue
		}
		if !slices.Contains(wf.Classes, deployRefClass(run.PrettyRef)) {
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

// deployRunWorkflow names the workflow a run belongs to by the base name the
// API reports for it, or "" for a workflow the watch does not follow.
func deployRunWorkflow(workflowID string) string {
	base := workflowID
	if idx := strings.LastIndex(base, "/"); idx >= 0 {
		base = base[idx+1:]
	}
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

// deploySeverity is what a failed run of wf on ref is worth, or "" for a
// failure this watch does not alert on: a workflow on a ref class it does not
// deploy from (staging does not run on a tag, a release does not run on main),
// or a workflow the watch does not follow. A health failure is judged by the
// job that failed, because a broken prod smoke is a broken release where the
// staging check is the staging deploy.
func deploySeverity(wf deployWorkflow, ref string, failingJobs []string) string {
	if !slices.Contains(wf.Classes, deployRefClass(ref)) {
		return ""
	}
	switch wf.File {
	case deployWorkflowStaging:
		return config.SeverityMedium
	case deployWorkflowDeploy, deployWorkflowTeardown:
		return config.SeverityHigh
	case deployWorkflowHealth:
		if slices.Contains(failingJobs, deployHealthProdJob) {
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
// reader needs to open the failure and what the next green run clears.
func deployMessage(repo, workflow string, run forgejo.ActionRun, failingJobs []string) string {
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
	lines = append(lines, fmt.Sprintf("The next green run of %s on %s closes this.", workflow, repo))
	return strings.Join(lines, "\n")
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
func sortedKeys(m map[string][]deployFailure) []string {
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
func (d *Daemon) startDeployWatch() {
	if d.ctx == nil {
		return
	}
	d.deployWatchOnce.Do(func() {
		w := d.productionDeployWatch()
		if w == nil {
			return
		}
		d.deployW.Store(w)
		go w.run(d.ctx)
	})
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
// and clears, and the push command the silent-overseer rule already uses.
func (d *Daemon) productionDeployWatch() *deployWatch {
	townRoot := d.config.TownRoot
	clients := &deployClients{}
	if cfg := landingWorkerConfig(d.patrolConfig); cfg != nil {
		dir, err := cfg.ForgejoTokenDir()
		if err != nil {
			d.logger.Printf("deploy_watch: the configured Forgejo token dir: %v", err)
			return nil
		}
		clients.tokenDir = dir
	} else if dir, err := config.DefaultForgejoTokenDir(); err == nil {
		clients.tokenDir = dir
	}
	// A town with no rig repository has nothing to watch, and one whose
	// viewer token cannot be read could read no run: both are said once here
	// rather than failing a read per repository per poll.
	repos := d.deployRepos()
	if len(repos) == 0 {
		d.logger.Printf("deploy_watch: no rig names a Forgejo repository, so no deploy is watched")
		return nil
	}
	if _, err := clients.client(repos[0].APIBase); err != nil {
		d.logger.Printf("deploy_watch: no %s Forgejo token, so no deploy is watched: %v", config.ForgejoRoleViewer, err)
		return nil
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
	return w
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
