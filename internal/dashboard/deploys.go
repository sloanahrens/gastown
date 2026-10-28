package dashboard

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// The Deploys block: the deploy workflows' runs, one row each, with the stages
// the workflow's needs give, and beneath them the apps' scheduled health
// checks, one row per app and environment. It reads what the Forgejo panel
// reads — the same read-only viewer client and the same repos — and adds
// nothing of its own: the run list, the run's stamps and its jobs are all the
// evidence it has. The runner's own health is not among them. GET
// /repos/{owner}/{repo}/actions/runners is owner-only and answers the viewer
// 403, so a run nothing has picked up is inferred from the run and said as an
// inference.

const (
	// deployWorkflow and stagingWorkflow are the workflows whose runs the block
	// follows: a release, started by a v* tag, and the landing that deploys main
	// to staging. The API names a workflow by its path in the repository
	// (".forgejo/workflows/deploy.yml"), so the names are matched on the base.
	deployWorkflow  = "deploy.yml"
	stagingWorkflow = "staging.yml"

	// deployRunsKept is how many of a repo's newest deploy runs the block
	// lists, both workflows together, and deployJobsKept how many runs' jobs it
	// fetches per refresh, across every repo. Runs cost one call per workflow
	// per repo — the query names the workflow, so the repo's hourly checks
	// cannot crowd the deploys off the page — and jobs cost one call each,
	// which is what the smaller, global cap is for. deployRunsKept of each
	// workflow is enough for the newest deployRunsKept of both: a run of one
	// workflow cannot be in the newest few overall while a newer run of the
	// same workflow is not.
	deployRunsKept = 5
	deployJobsKept = 3

	// deployTextMax caps a stage name and a run's ref, both of which a
	// repository chooses and neither of which the page should spend the cell
	// on.
	deployTextMax = 40

	// healthWorkflow is the scheduled check of a deployed app: an app repo's
	// hourly run of its integration suite against staging and a read-only smoke
	// check against prod (dv-4qtt). It is not a deploy, so the block folds its
	// runs into one row per app and environment rather than listing them.
	healthWorkflow = "health.yml"

	// healthEnvStaging and healthEnvProd are the environments health.yml checks,
	// by the names its jobs carry them, in the order the block draws them: the
	// deployment moves staging first. A job the block does not know is not an
	// environment.
	healthEnvStaging = "staging-integration"
	healthEnvProd    = "prod-smoke"

	// healthRunWindow is how many of an app's newest health runs a row is
	// counted over: the pass count's denominator and how far back the last green
	// is looked for. An environment's result is not on the run — the run's
	// status is the whole workflow's — so this is the runs one repo with checks
	// wants the jobs of, once.
	healthRunWindow = 24

	// healthReadBudget bounds the scheduled checks' own read, which runs last on
	// a deadline of its own: the runs no earlier refresh read the jobs of are
	// the refresh's costliest read, and it must not spend the time the deploy
	// rows need. The deadline is detached from the deploy reads' on purpose, so a
	// refresh can run to forgejoTimeout plus this.
	healthReadBudget = forgejoTimeout / 2

	// deployNoRunnerAfter is how long a deploy run may sit waiting before the
	// block says nothing has picked it up, and deployStuckAfter how long a
	// running run may hold the same stages before the block marks it stuck.
	// Both are inferences from the jobs, with tests, rather than reads of the
	// runners.
	deployNoRunnerAfter = 5 * time.Minute
	deployStuckAfter    = 30 * time.Minute
)

// deployStuck is what a running run that has not moved reads as. It is a
// question because the block cannot see the runner the run is on.
const deployStuck = "stuck?"

// The run statuses the block infers runner trouble from: the API's own words
// for a run (forgejo.ActionRun.Status).
const (
	deployWaiting = "waiting"
	deployRunning = "running"
)

// healthEnvs is the environments the block reads, in the order it draws them,
// and deployWorkflows the workflows whose runs it lists, in the order it asks
// for them.
var (
	healthEnvs      = []string{healthEnvStaging, healthEnvProd}
	deployWorkflows = []string{deployWorkflow, stagingWorkflow}
)

// The job statuses the health fold reads, in the API's own words. Only success
// is a pass and only failure raises the marker: a skipped check is an app that
// is off on purpose, a canceled one is nobody's verdict, and waiting, running
// and blocked have not finished.
const (
	healthSuccess = "success"
	healthFailure = "failure"

	// forgejoRunBlocked is Forgejo's word for a run between one job finishing
	// and the next being picked up.
	forgejoRunBlocked = "blocked"
)

// DeployStage is one stage of a deploy run: one job, its name and the state
// the API reports for it. A stage a failed earlier stage never let run reads
// as blocked or skipped, which is the API saying it did not run.
type DeployStage struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// DeployRun is one deploy run as the block draws it.
type DeployRun struct {
	// Repo is the owner/name, the same value the Forgejo panel's rows carry,
	// so one pane names a repo one way.
	Repo string `json:"repo"`
	// Ref is the run's ref as the API names it — a tag like v0.1.0, or a
	// branch — stripped and capped.
	Ref string `json:"ref,omitempty"`
	// Workflow is the workflow the run belongs to, by its base name:
	// "deploy.yml" for a release, "staging.yml" for the landing that deploys
	// main to staging. The page tells the two apart by it, since the ref of a
	// release is a tag and the ref of a staging run is the branch it deployed.
	Workflow string `json:"workflow"`
	// Hash is the commit the run tested, the eight characters the Actions
	// page shows.
	Hash string `json:"hash,omitempty"`
	// Status is the run's own state.
	Status string `json:"status"`
	// At is when the run was created; the page ages the row from there. It is
	// absent for a created stamp the API never sent or sent unparseable, which
	// the page draws as no age rather than as one measured from year 1.
	At time.Time `json:"at,omitzero"`
	// URL is the run's html_url, absent when the API sent none or sent
	// something that is not http(s): the page links a row only when it has a
	// place to send it.
	URL string `json:"url,omitempty"`
	// Stages are the run's jobs in the order their needs give.
	Stages []DeployStage `json:"stages"`
	// StagesSkipped marks a run past the deployJobsKept cap, whose jobs the
	// block never asked for. The page draws no stage cell for it, so the row
	// reads on one line; StagesUnread is the other reason a run has no stages,
	// and the page draws that one.
	StagesSkipped bool `json:"stages_skipped,omitempty"`
	// StagesUnread marks a run whose jobs call failed. The page says the stages
	// were not read rather than drawing an empty list, which would read as a
	// run with no stages at all.
	StagesUnread bool `json:"stages_unread,omitempty"`
	// Warn is the runner trouble the run's own stamps and stages say, in the
	// page's warning style: a run nothing has picked up, or one that has not
	// moved.
	Warn string `json:"warn,omitempty"`
}

// DeployHealthRow is one app's scheduled health check in one environment as the
// block draws it. It is not a run: it is the app's newest healthRunWindow runs
// folded together, which is what keeps an hourly check from being an hourly row
// (gt-t9w60).
type DeployHealthRow struct {
	// Repo is the owner/name the page names the app's repo by everywhere else,
	// and App the repo's own name, which the row's first cell carries it by.
	Repo string `json:"repo"`
	App  string `json:"app"`
	// Env is the environment the row is about: staging-integration or
	// prod-smoke, the names health.yml's jobs carry.
	Env string `json:"env"`
	// Status is the newest health run's own word for this environment's job,
	// skipped meaning the app is off on purpose. It is empty for a row whose
	// newest run could not be read, which Unread says.
	Status string `json:"status,omitempty"`
	// At is when that run was created: the age the page shows beside the
	// result. It is absent with the status.
	At time.Time `json:"at,omitzero"`
	// LastGreen is when this environment last passed, absent when it has not
	// passed in the window. A skipped run is not a pass, so an app that is off
	// keeps the green it earned, and one that has never passed says so rather
	// than reading as green.
	LastGreen time.Time `json:"last_green,omitzero"`
	// Passed is how many of the window's runs this environment passed, out of
	// Runs: the runs the block read that carry the environment's job.
	Passed int `json:"passed"`
	Runs   int `json:"runs"`
	// NotRead is how many runs of the window the block could not read: the
	// block does not know whether they carry this environment's job, so they
	// are in neither Passed nor Runs and are counted here instead of leaving
	// the two to read as the whole window.
	NotRead int `json:"not_read,omitempty"`
	// URL is the newest run's html_url, absent when the API sent none or sent
	// something that is not http(s).
	URL string `json:"url,omitempty"`
	// Unread marks a row whose newest run's jobs the block could not read. The
	// page says so rather than drawing the run under it as the newest.
	Unread bool `json:"unread,omitempty"`
	// NoChecks marks a row of an app that deploys and has no scheduled run yet:
	// there is no result to draw, and nothing ran, so the row is neither a pass
	// nor a failure.
	NoChecks bool `json:"no_checks,omitempty"`
}

// HealthMark is the strip's health pill: State is "red" while any app's newest
// check failed and "degraded" when only unread ones remain; Text names them.
type HealthMark struct {
	State string `json:"state"`
	Text  string `json:"text"`
}

// healthMark decides the pill: red for failures, degraded for unread checks
// alone, nil when neither list has a name.
func healthMark(failed, unread []string) *HealthMark {
	var parts []string
	if len(failed) > 0 {
		parts = append(parts, "health: "+strings.Join(failed, ", "))
	}
	if len(unread) > 0 {
		parts = append(parts, "not read: "+strings.Join(unread, ", "))
	}
	if len(parts) == 0 {
		return nil
	}
	state := "degraded"
	if len(failed) > 0 {
		state = "red"
	}
	return &HealthMark{State: state, Text: strings.Join(parts, " · ")}
}

// Deploys is the Cloud section's Deploys block: the newest deploy runs the
// viewer can see, of either workflow. Missing names the repos whose runs could
// not be read, so a repo that failed is named rather than reading as a repo
// with no deploys; Error is the class of a refresh that read no repo at all,
// which the block's note carries.
type Deploys struct {
	Runs []DeployRun `json:"runs"`
	// Health is the scheduled checks, one row per app and environment, and is
	// empty until a repo deploys or has health.yml runs.
	Health []DeployHealthRow `json:"health,omitempty"`
	// HealthFailed names the repos whose newest scheduled check failed — the
	// strip's red marker. HealthUnread names the repos whose newest check the
	// block could not read, which the same marker says rather than leaving an
	// alarm nobody can see. Both carry owner/name, so two repos with the same
	// base name are two entries rather than one name twice. An app that is off
	// is in neither: its run is skipped, which is on purpose, not a failure.
	HealthFailed []string `json:"health_failed,omitempty"`
	HealthUnread []string `json:"health_unread,omitempty"`
	// HealthMark is the strip's pill, decided here from the two lists above so
	// the page only draws it; nil when neither list has a name.
	HealthMark *HealthMark `json:"health_mark,omitempty"`
	// HealthMissing names the repos whose runs name no environment the block
	// knows, so there is no row to draw them in. The block's note carries them.
	HealthMissing []string `json:"health_missing,omitempty"`
	Missing       []string `json:"missing,omitempty"`
	Error         string   `json:"error,omitempty"`
}

// deployAPI is the part of the Forgejo client the deploy reader calls.
type deployAPI interface {
	ListUserRepos(ctx context.Context) ([]forgejo.Repository, error)
	ListRuns(ctx context.Context, owner, repo string, f forgejo.RunFilter) (*forgejo.RunList, error)
	ListRunJobs(ctx context.Context, owner, repo string, runID int64) ([]forgejo.ActionRunJob, error)
}

// DeployReader reads the deploy runs of the repos the Forgejo panel reads.
//
// It is the one reader here that compares a read against the one before it:
// "a run that has not moved" is not in any stamp the API sends, so the reader
// holds the stages each running run last showed and when they changed to them.
// The scheduled checks are read the same way for a different reason: a run that
// has ended does not run again, so its jobs are read once and held.
type DeployReader struct {
	api   deployAPI
	repos []string // owner/name overrides; empty reads the viewer's whole list
	now   func() time.Time
	// timeout is the deadline of the refresh's deploy reads; forgejoTimeout
	// outside tests. The checks' read has healthReadBudget on top of it.
	timeout time.Duration

	mu     sync.Mutex
	watch  map[string]deployWatch // run key -> the stages last seen and since when
	checks map[string]cachedCheck // run key -> the jobs of a check that has ended
}

// deployWatch is one running run's stages as a read left them and the moment
// they changed to them: a run holding the same stages for deployStuckAfter is
// what the block's "stuck?" is read from.
type deployWatch struct {
	mark  string
	since time.Time
}

// cachedCheck is one scheduled run's jobs as the fold reads them, held only for
// a run that ended with every job final. status and stopped are what the run
// showed when it was read: a re-run changes them, and its jobs are read again.
type cachedCheck struct {
	envs    map[string]string
	failed  bool
	status  string
	stopped string
}

// NewDeployReader builds a reader over api. A non-empty repos is the exact
// owner/name list to read instead of every repo the viewer can see.
func NewDeployReader(api deployAPI, repos []string) *DeployReader {
	return &DeployReader{
		api: api, repos: append([]string(nil), repos...), now: time.Now, timeout: forgejoTimeout,
		watch: map[string]deployWatch{}, checks: map[string]cachedCheck{},
	}
}

// Read returns the deploy runs of the reader's repos: the newest
// deployRunsKept per repo across both workflows, newest first, with jobs for
// the newest deployJobsKept of those.
func (r *DeployReader) Read() *Deploys {
	now := r.now()
	// The repo list, every run query and every jobs query share the panel's
	// deadline, so a slow Forgejo cannot hold the deploy rows past it however
	// many repos the viewer sees. The scheduled checks are read after them under
	// a budget of their own (healthReadBudget).
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
	defer cancel()

	// A copy, because the viewer's whole list is appended to it, and the
	// reader's own list must not grow a repo per read.
	names := append([]string(nil), r.repos...)
	if len(names) == 0 {
		repos, err := r.api.ListUserRepos(ctx)
		if err != nil {
			return &Deploys{Runs: []DeployRun{}, Error: forgejoErrorClass(err)}
		}
		for _, repo := range repos {
			names = append(names, repo.FullName)
		}
	}

	var (
		runs     []deployRun
		readable []string // the repos whose runs answered, whose checks are read below
		missing  []string
		firstErr error
	)
	for _, name := range names {
		owner, repo, ok := strings.Cut(name, "/")
		if !ok || owner == "" || repo == "" {
			return &Deploys{Runs: []DeployRun{}, Error: forgejoErrorClass(fmt.Errorf("%w: %s", errRepoName, strconv.Quote(name)))}
		}
		// One repo the viewer cannot read must not cost the block the repos
		// that answered: its runs are simply absent and it is named.
		found, err := r.runList(ctx, name, owner, repo)
		if err != nil {
			firstErr = err
			missing = append(missing, name)
			continue
		}
		runs = append(runs, found...)
		readable = append(readable, name)
	}
	if len(missing) == len(names) && len(missing) > 0 {
		// Every repo failed, so there is no fresh value at all. That is the
		// viewer not answering, and the note says so rather than the block
		// drawing an empty list that reads as a town with no deploys.
		return &Deploys{Runs: []DeployRun{}, Error: forgejoErrorClass(firstErr)}
	}

	sort.SliceStable(runs, func(i, j int) bool { return runs[i].created.After(runs[j].created) })

	r.mu.Lock()
	prev := r.watch
	r.mu.Unlock()
	next := make(map[string]deployWatch, deployJobsKept)

	rows := make([]DeployRun, 0, len(runs))
	for i, dr := range runs {
		row := DeployRun{
			Repo:     dr.repo,
			Ref:      cloudText(dr.run.PrettyRef, deployTextMax),
			Workflow: dr.workflow,
			Hash:     shortSHA(dr.run.CommitSHA),
			Status:   dr.run.Status,
			At:       dr.created,
			URL:      deployURL(dr.run.HTMLURL),
			Stages:   []DeployStage{},
		}
		if i < deployJobsKept {
			jobs, err := r.api.ListRunJobs(ctx, dr.owner, dr.name, dr.run.ID)
			if err != nil {
				row.StagesUnread = true
			} else {
				row.Stages = deployStages(jobs)
			}
		} else {
			row.StagesSkipped = true
		}
		row.Warn = r.inferWarn(dr, row, prev, next, now)
		rows = append(rows, row)
	}

	r.mu.Lock()
	r.watch = next
	r.mu.Unlock()

	// The scheduled checks are the refresh's last read, on a budget of their
	// own: the runs no earlier refresh has the jobs of are what they cost, and
	// neither that nor the deploy rows may take the other's time. A check the
	// budget or the wire takes out is named in HealthMissing or HealthUnread
	// rather than left to read as a town with no checks.
	health := []DeployHealthRow{}
	var healthFailed, healthUnread, healthMissing []string
	deployed := make(map[string]bool, len(runs))
	for _, dr := range runs {
		deployed[dr.repo] = true
	}
	r.mu.Lock()
	prevChecks := r.checks
	r.mu.Unlock()
	// Built fresh every refresh, so an entry goes the moment its run leaves the
	// window and the map cannot grow with a town's history.
	checks := make(map[string]cachedCheck, len(prevChecks))
	// Derived from a fresh context, not ctx: the deploy reads above may have spent
	// all of ctx's timeout, and the health read would then start already expired.
	hctx, cancelHealth := context.WithTimeout(context.Background(), healthReadBudget)
	defer cancelHealth()
	for _, name := range readable {
		owner, repo, _ := strings.Cut(name, "/")
		res := r.healthRows(hctx, name, owner, repo, prevChecks, checks)
		health = append(health, res.rows...)
		if len(res.rows) == 0 && !res.listFailed && !res.unknown && deployed[name] {
			health = append(health, noChecksRows(name)...)
		}
		// A listing that failed is an unread check, which the strip says; only
		// runs that name no known environment are missing.
		switch {
		case res.listFailed, res.unread:
			healthUnread = append(healthUnread, name)
		case res.failed:
			healthFailed = append(healthFailed, name)
		case res.unknown:
			healthMissing = append(healthMissing, name)
		}
	}
	r.mu.Lock()
	r.checks = checks
	r.mu.Unlock()

	// The rows are read a repo at a time, in the viewer's order, and the page is
	// read app by app: sorting by repo keeps an app's environments together, in
	// the order the fold drew them, and two repos with the same base name are
	// two groups of rows rather than one app's.
	sort.SliceStable(health, func(i, j int) bool { return health[i].Repo < health[j].Repo })
	sort.Strings(healthFailed)
	sort.Strings(healthUnread)
	sort.Strings(healthMissing)

	return &Deploys{
		Runs:          rows,
		Health:        health,
		HealthFailed:  healthFailed,
		HealthUnread:  healthUnread,
		HealthMark:    healthMark(healthFailed, healthUnread),
		HealthMissing: healthMissing,
		Missing:       missing,
	}
}

// inferWarn reads the runner trouble off one run and records what this read
// saw of it, so the next read can tell a run that is moving from one that is
// not. Only a running run with stages read is watched: a run whose jobs were
// not read has nothing to change, and reading its stillness as its own would
// be reporting the block's own blind spot as the runner's.
func (r *DeployReader) inferWarn(dr deployRun, row DeployRun, prev, next map[string]deployWatch, now time.Time) string {
	switch {
	case row.Status == deployWaiting && !dr.created.IsZero() && now.Sub(dr.created) > deployNoRunnerAfter:
		return fmt.Sprintf("no runner picked this up for %d min", int(now.Sub(dr.created).Minutes()))
	case row.Status == deployRunning && len(row.Stages) > 0:
		key := dr.key()
		mark := stageMark(row.Stages)
		w, ok := prev[key]
		if !ok || w.mark != mark {
			w = deployWatch{mark: mark, since: now}
		}
		next[key] = w
		if now.Sub(w.since) >= deployStuckAfter {
			return deployStuck
		}
	}
	return ""
}

// deployRun is one run with the repo it belongs to, the workflow it came from
// and its created stamp parsed once, so the ordering, the age and the
// inference all agree on what the API sent.
type deployRun struct {
	owner, name string // the repo's two halves, which the jobs call needs
	repo        string // owner/name, as the panel names it
	workflow    string // the workflow's base name, which the page tags the row with
	run         forgejo.ActionRun
	created     time.Time
}

// key names the run for the watch. The repo is part of it because an id is
// only unique within the instance the reader happens to be pointed at.
func (dr deployRun) key() string { return runKey(dr.repo, dr.run.ID) }

// runKey names one run for the state the reader holds between refreshes: the
// watch's stages and a check's jobs are both kept under it.
func runKey(repo string, id int64) string { return repo + "#" + strconv.FormatInt(id, 10) }

// runList reads one repo's deploy runs, newest first and at most
// deployRunsKept of them: every run of either deploy workflow whatever its
// event, since a tag push, a manual run and a teardown run are all deploys,
// and a landing's staging deploy is one the operator has to see. The two
// workflows share the cap, so a busy week of releases cannot crowd out the
// staging runs, or the other way round.
//
// Each workflow is asked for by name rather than the repo's runs being read
// whole: an app repo runs health.yml hourly, so an unfiltered page of its runs
// is the checks and the deploy runs behind them fall off it. Forgejo 16.0.5
// honors `limit` once `page` is given, so the page is always sent.
func (r *DeployReader) runList(ctx context.Context, name, owner, repo string) ([]deployRun, error) {
	var out []deployRun
	for _, workflow := range deployWorkflows {
		list, err := r.api.ListRuns(ctx, owner, repo, forgejo.RunFilter{WorkflowID: workflow, Page: 1, Limit: deployRunsKept})
		if err != nil {
			return nil, err
		}
		for _, run := range list.Runs {
			// The API matches the workflow on its path, so the base name is the
			// same query; a server that answers loosely still cannot put another
			// workflow's run in a deploy row.
			if deployRunWorkflow(run.WorkflowID) != workflow {
				continue
			}
			out = append(out, deployRun{owner: owner, name: repo, repo: name, workflow: workflow, run: run, created: parseRunStamp(run.Created)})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].created.After(out[j].created) })
	if len(out) > deployRunsKept {
		out = out[:deployRunsKept]
	}
	return out, nil
}

// healthRun is one scheduled run of health.yml as the fold reads it: the run
// and its created stamp parsed once, each known environment job's status, and
// whether a job of it finished without passing. Envs is nil for a run whose
// jobs call failed, which is a row the block must say it could not read rather
// than a check this environment is not part of.
type healthRun struct {
	run     forgejo.ActionRun
	created time.Time
	envs    map[string]string
	failed  bool
	unread  bool
}

// healthRead is one repo's scheduled checks as the block reads them: the rows to
// draw and the newest run's word on the app — unread, or failed. unread and
// failed are read only where a row was drawn, except that failed also holds for
// an unknown repo whose newest run failed; listFailed (the runs could not be
// listed) and unknown (no environment the block knows) mean no row was.
type healthRead struct {
	rows       []DeployHealthRow
	listFailed bool
	unknown    bool
	unread     bool
	failed     bool
}

// healthRows reads one repo's scheduled health checks and folds them into the
// block's rows: the app's newest healthRunWindow runs of health.yml as one row
// per app and environment, plus the newest run's word on the app. The jobs of
// every run in the window are read because an environment's result is not on
// the run, and a run whose jobs were all skipped is green there while nothing
// was tested.
//
// prev holds the jobs of runs that ended with every job final as of the last
// refresh and seen collects this one's, so a settled run is read once. The
// window is read newest first, so an exhausted budget costs the oldest runs
// (NotRead says them), not the newest run the marker stands on.
//
// A listing that failed is listFailed, never a repo with no checks.
func (r *DeployReader) healthRows(ctx context.Context, name, owner, repo string, prev, seen map[string]cachedCheck) healthRead {
	list, err := r.api.ListRuns(ctx, owner, repo, forgejo.RunFilter{WorkflowID: healthWorkflow, Page: 1, Limit: healthRunWindow})
	if err != nil {
		return healthRead{listFailed: true}
	}
	var window []healthRun
	for _, run := range list.Runs {
		if path.Base(run.WorkflowID) != healthWorkflow {
			continue
		}
		window = append(window, healthRun{run: run, created: parseRunStamp(run.Created)})
	}
	sort.SliceStable(window, func(i, j int) bool { return window[i].created.After(window[j].created) })
	if len(window) > healthRunWindow {
		window = window[:healthRunWindow]
	}
	for i := range window {
		key := runKey(name, window[i].run.ID)
		run := window[i].run
		if held, ok := prev[key]; ok && held.status == run.Status && held.stopped == run.Stopped {
			window[i].envs, window[i].failed = held.envs, held.failed
			seen[key] = held
			continue
		}
		jobs, err := r.api.ListRunJobs(ctx, owner, repo, run.ID)
		// A failed call and an empty answer are both a check the block could not
		// read; neither is held, so the next refresh asks again.
		if err != nil || len(jobs) == 0 {
			window[i].unread = true
			continue
		}
		window[i].envs = healthEnvsOf(jobs)
		window[i].failed = healthRunFailed(jobs, run.Status)
		if healthRunEnded(run.Status) && healthJobsFinal(jobs) {
			seen[key] = cachedCheck{envs: window[i].envs, failed: window[i].failed, status: run.Status, stopped: run.Stopped}
		}
	}
	return foldHealth(name, window)
}

// noChecksRows is the rows of a deployed app with no health run yet: one per
// environment the block draws, marked NoChecks and carrying nothing else.
func noChecksRows(name string) []DeployHealthRow {
	rows := make([]DeployHealthRow, 0, len(healthEnvs))
	for _, env := range healthEnvs {
		rows = append(rows, DeployHealthRow{Repo: name, App: healthApp(name), Env: env, NoChecks: true})
	}
	return rows
}

// healthRunEnded reports whether a scheduled run has finished: one waiting,
// running, or between two of its jobs has not.
func healthRunEnded(status string) bool {
	switch status {
	case deployWaiting, deployRunning, forgejoRunBlocked:
		return false
	}
	return true
}

// healthJobsFinal reports whether every job has a final status, the only state
// a run's jobs are held in; a job still waiting, blocked or running is read again.
func healthJobsFinal(jobs []forgejo.ActionRunJob) bool {
	for _, j := range jobs {
		switch cloudText(j.Status, deployTextMax) {
		case healthSuccess, healthFailure, forgejoRunCancelled, "skipped":
		default:
			return false
		}
	}
	return true
}

// healthEnvsOf reads one run's jobs as the environments it checked: the status
// the API gave each known environment job. A job the block does not know is not
// an environment, and neither is a job that only held the ones it needs.
func healthEnvsOf(jobs []forgejo.ActionRunJob) map[string]string {
	out := make(map[string]string, len(healthEnvs))
	for _, j := range jobs {
		name := cloudText(j.Name, deployTextMax)
		for _, env := range healthEnvs {
			if name == env {
				out[env] = cloudText(j.Status, deployTextMax)
			}
		}
	}
	return out
}

// foldHealth folds one app's window into the block's rows, one per environment,
// with the newest run's result — never an older run's drawn as the newest — and
// the newest run's word on the app: a run the block could not read leaves both
// saying so, with the passes it did read still counted beside them.
func foldHealth(name string, window []healthRun) healthRead {
	envs := healthWindowEnvs(window)
	if len(window) > 0 && len(envs) == 0 {
		// Every run was read and none named an environment the block knows — a
		// job the workflow renamed, say — so there is no row to fold them into
		// and no result to report. That is a repo whose checks the block cannot
		// read, not a repo with no checks.
		// A failing newest run is still a failure: the run's own status or any
		// job, whatever its name, says so.
		return healthRead{unknown: true, failed: healthFailed(window)}
	}
	// The newest run is what the row's result comes from, so a window whose
	// newest run could not be read has no result to report whatever the runs
	// under it hold.
	unread := len(window) > 0 && window[0].unread
	read := len(window) > 0 && !unread
	gone := 0
	for _, hr := range window {
		if hr.unread {
			gone++
		}
	}
	rows := make([]DeployHealthRow, 0, len(envs))
	for _, env := range envs {
		row := DeployHealthRow{Repo: name, App: healthApp(name), Env: env, Unread: !read, NotRead: gone}
		have := false
		for _, hr := range window {
			status, ok := hr.envs[env]
			if !ok {
				continue
			}
			row.Runs++
			if status == healthSuccess {
				row.Passed++
				if row.LastGreen.IsZero() {
					row.LastGreen = hr.created
				}
			}
			if read && !have {
				// The first run that carries this environment is the newest one:
				// the window is newest first, and a run whose jobs were not read
				// carries no environment at all.
				have = true
				row.Status, row.At, row.URL = status, hr.created, deployURL(hr.run.HTMLURL)
			}
		}
		rows = append(rows, row)
	}
	return healthRead{rows: rows, unread: unread, failed: healthFailed(window)}
}

// healthWindowEnvs names the environments a window carries, in the order the
// block draws them. A window whose runs were read and named none returns
// nothing, which the fold says as a repo it could not read — the workflow
// checks no environment the block knows — but one whose newest run could not be
// read could name either, so the two it knows are drawn as rows it could not
// read rather than left blank.
func healthWindowEnvs(window []healthRun) []string {
	var named []string
	read := false
	for _, env := range healthEnvs {
		for _, hr := range window {
			if hr.unread {
				continue
			}
			read = true
			if _, ok := hr.envs[env]; ok {
				named = append(named, env)
				break
			}
		}
	}
	if len(named) == 0 && len(window) > 0 && !read {
		return append([]string(nil), healthEnvs...)
	}
	return named
}

// healthFailed reports whether an app's newest scheduled check failed, in any
// job. A run in flight has not failed, a skipped or canceled one is not a
// failure, and an older run's failure is never claimed for the newest.
func healthFailed(window []healthRun) bool {
	if len(window) == 0 || window[0].unread {
		return false
	}
	return healthRunEnded(window[0].run.Status) && window[0].failed
}

// healthRunFailed reports whether the run's own status, which is the whole
// workflow's, or any job of it, whatever its name, says failure. A canceled
// run is not a failure.
func healthRunFailed(jobs []forgejo.ActionRunJob, status string) bool {
	if cloudText(status, deployTextMax) == healthFailure {
		return true
	}
	for _, j := range jobs {
		if cloudText(j.Status, deployTextMax) == healthFailure {
			return true
		}
	}
	return false
}

// healthApp is the name an app is known by: the repo's own name, without the
// owner, which is what the row's first cell carries.
func healthApp(repo string) string { return path.Base(repo) }

// deployRunWorkflow names the workflow a run belongs to, or "" for a run of
// one the block does not follow. It reads the workflow by its base name, the
// way the API names it: a path in the repository
// (".forgejo/workflows/deploy.yml") or, for a run the API sent no path for,
// the name alone.
func deployRunWorkflow(id string) string {
	switch base := path.Base(id); base {
	case deployWorkflow, stagingWorkflow:
		return base
	}
	return ""
}

// deployStages renders a run's jobs as the block's stages: in the order their
// needs give, with the name and the state stripped and capped the way the page
// needs text from a repository to be.
func deployStages(jobs []forgejo.ActionRunJob) []DeployStage {
	ordered := orderJobs(jobs)
	out := make([]DeployStage, 0, len(ordered))
	for _, j := range ordered {
		out = append(out, DeployStage{Name: cloudText(j.Name, deployTextMax), Status: cloudText(j.Status, deployTextMax)})
	}
	return out
}

// orderJobs returns jobs in the order their needs give: a job after every job
// it needs, ties keeping the API's order. A job whose needs name something
// that is not in the list cannot wait on it, so it is placed with the rest.
func orderJobs(in []forgejo.ActionRunJob) []forgejo.ActionRunJob {
	known := make(map[string]bool, len(in))
	for _, j := range in {
		known[j.Name] = true
	}
	done := make(map[string]bool, len(in))
	rest := append([]forgejo.ActionRunJob(nil), in...)
	out := make([]forgejo.ActionRunJob, 0, len(in))
	for len(rest) > 0 {
		var next []forgejo.ActionRunJob
		moved := false
		for _, j := range rest {
			if needsPending(j.Needs, known, done) {
				next = append(next, j)
				continue
			}
			out = append(out, j)
			done[j.Name] = true
			moved = true
		}
		if !moved {
			// A needs cycle, which a workflow file cannot express, would leave
			// the rest forever pending; the API's order ends the walk rather
			// than looping on it.
			return append(out, next...)
		}
		rest = next
	}
	return out
}

// needsPending reports whether any job j needs is still unplaced. A need that
// names no job in the list is already satisfied: nothing here can wait on it.
func needsPending(needs []string, known, done map[string]bool) bool {
	for _, n := range needs {
		if known[n] && !done[n] {
			return true
		}
	}
	return false
}

// stageMark is a run's stages as one string: what the block holds to tell a
// run that is moving from one that is not. Only the name and the state are in
// it, because those are what the page draws, and a change the page cannot show
// is not one worth reporting.
func stageMark(stages []DeployStage) string {
	var b strings.Builder
	for _, s := range stages {
		b.WriteString(s.Name)
		b.WriteByte('=')
		b.WriteString(s.Status)
		b.WriteByte(',')
	}
	return b.String()
}

// deployURL is the run's html_url as the page may link it: http(s) only, so a
// URL that is not one — an empty field, or text the API never meant as a link
// — leaves the row unlinked rather than becoming its target.
func deployURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return ""
	}
	return raw
}
