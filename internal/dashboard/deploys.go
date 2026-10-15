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
// checks, one row per app and environment. It reads the Forgejo panel's own
// viewer client and repo list, and its only evidence is the runs, their stamps
// and their jobs: the runner API is owner-only and answers the viewer 403, so a
// run nothing has picked up is inferred and said as an inference.

const (
	// deployWorkflow and stagingWorkflow are the workflows whose runs the block
	// follows: a release, started by a v* tag, and the landing that deploys main
	// to staging. The API names a workflow by its path in the repository
	// (".forgejo/workflows/deploy.yml"), so the names are matched on the base.
	deployWorkflow  = "deploy.yml"
	stagingWorkflow = "staging.yml"

	// deployRunPage is how many runs one repo's query asks for, so a server
	// that honors the page bounds the window. Forgejo 16.0.5 ignores `limit`
	// unless `page` is given, and answers with the repo's runs whole (verified
	// live) — the better answer here, since a deploy run sits behind every
	// push, landing and probe run a busy repo makes. Either way the block
	// keeps only the newest deployRunsKept of what comes back.
	deployRunPage = 50

	// deployRunsKept is how many of a repo's newest deploy runs the block
	// lists, both workflows together, and deployJobsKept how many runs' jobs it
	// fetches per refresh, across every repo. Runs cost one call per repo; jobs
	// cost one call each, which is what the smaller, global cap is for.
	deployRunsKept = 5
	deployJobsKept = 3

	// deployTextMax caps a stage name and a run's ref, both of which a
	// repository chooses and neither of which the page should spend the cell
	// on.
	deployTextMax = 40

	// healthWorkflow is the scheduled check of a deployed app: an app repo's
	// hourly run of its integration suite against staging and a read-only smoke
	// check against prod (dv-4qtt). It is not a deploy, so the block folds its
	// runs into one row per app and environment rather than listing them
	// (gt-t9w60).
	healthWorkflow = "health.yml"

	// healthEnvStaging and healthEnvProd are the environments health.yml checks,
	// by the names its jobs carry them, in the order the block draws them: the
	// deployment moves staging first. A job the block does not know is not an
	// environment (gt-t9w60).
	healthEnvStaging = "staging-integration"
	healthEnvProd    = "prod-smoke"

	// healthRunWindow is how many of an app's newest health runs a row is
	// counted over: the pass count's denominator and how far back the last green
	// is looked for. An environment's result is not on the run — the run's
	// status is the whole workflow's — so this is also the jobs calls one repo
	// with checks costs (gt-t9w60).
	healthRunWindow = 24

	// healthReadBudget bounds the scheduled checks' own read, which runs last on
	// a deadline of its own: a jobs call per run of every window is the refresh's
	// costliest read, and it must not spend the time the deploy rows need
	// (gt-t9w60).
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

// healthEnvs is the environments the block reads, in the order it draws them.
var healthEnvs = []string{healthEnvStaging, healthEnvProd}

// The job statuses the health fold reads, in the API's own words. Only success
// is a pass, and only failure and the canceled word are a check that finished
// without one: a skipped check is an app that is off on purpose, and waiting,
// running and blocked have not finished at all.
const (
	healthSuccess = "success"
	healthFailure = "failure"
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
	// and App the repo's own name, which the row's first cell and the strip's
	// marker carry it by.
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
	// Runs: the runs the block read that carry the environment's job. A run
	// whose jobs call failed is in neither count.
	Passed int `json:"passed"`
	Runs   int `json:"runs"`
	// URL is the newest run's html_url, absent when the API sent none or sent
	// something that is not http(s).
	URL string `json:"url,omitempty"`
	// Unread marks a row whose newest run's jobs the block could not read. The
	// page says so rather than drawing the run under it as the newest.
	Unread bool `json:"unread,omitempty"`
}

// Deploys is the Cloud section's Deploys block: the newest deploy runs the
// viewer can see, of either workflow. Missing names the repos whose runs could
// not be read, so a repo that failed is named rather than reading as one with
// no deploys; Error is the class of a refresh that read no repo at all, which
// the block's note carries.
type Deploys struct {
	Runs []DeployRun `json:"runs"`
	// Health is the scheduled checks of every repo that has them: one row per
	// app and environment. It is empty until a repo has health.yml runs, so a
	// repo without the workflow shows nothing extra.
	Health []DeployHealthRow `json:"health,omitempty"`
	// HealthFailed names the apps whose newest scheduled check failed — the
	// strip's red marker. HealthUnread names the apps whose newest check the
	// block could not read, which the same marker says rather than leaving an
	// alarm nobody can see. An app that is off is in neither: its run is
	// skipped, which is on purpose, not a failure.
	HealthFailed []string `json:"health_failed,omitempty"`
	HealthUnread []string `json:"health_unread,omitempty"`
	// HealthMissing names the repos whose scheduled checks could not be listed
	// at all. They have no app to name, so the block's note carries them.
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
type DeployReader struct {
	api   deployAPI
	repos []string // owner/name overrides; empty reads the viewer's whole list
	now   func() time.Time

	mu    sync.Mutex
	watch map[string]deployWatch // run key -> the stages last seen and since when
}

// deployWatch is one running run's stages as a read left them and the moment
// they changed to them: a run holding the same stages for deployStuckAfter is
// what the block's "stuck?" is read from.
type deployWatch struct {
	mark  string
	since time.Time
}

// NewDeployReader builds a reader over api. A non-empty repos is the exact
// owner/name list to read instead of every repo the viewer can see.
func NewDeployReader(api deployAPI, repos []string) *DeployReader {
	return &DeployReader{api: api, repos: append([]string(nil), repos...), now: time.Now, watch: map[string]deployWatch{}}
}

// Read returns the deploy runs of the reader's repos: the newest
// deployRunsKept per repo across both workflows, newest first, with jobs for
// the newest deployJobsKept of those.
func (r *DeployReader) Read() *Deploys {
	now := r.now()
	// The whole refresh — the repo list, every run query and every jobs query
	// — shares the panel's deadline, so a slow Forgejo cannot hold the block
	// past it however many repos the viewer sees.
	ctx, cancel := context.WithTimeout(context.Background(), forgejoTimeout)
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

	// The scheduled checks are the refresh's last read, on a budget of their own:
	// a jobs call per run of every window is what they cost, and neither cost may
	// take the deploy rows' time. A check the budget or the wire takes out is
	// named — the repo in HealthMissing, the app in HealthUnread — rather than
	// left to read as a town with no checks (gt-t9w60).
	health := []DeployHealthRow{}
	var healthFailed, healthUnread, healthMissing []string
	hctx, cancelHealth := context.WithTimeout(ctx, healthReadBudget)
	defer cancelHealth()
	for _, name := range readable {
		owner, repo, _ := strings.Cut(name, "/")
		res := r.healthRows(hctx, name, owner, repo)
		health = append(health, res.rows...)
		switch {
		case !res.listed:
			healthMissing = append(healthMissing, name)
		case res.unread:
			healthUnread = append(healthUnread, healthApp(name))
		case res.failed:
			healthFailed = append(healthFailed, healthApp(name))
		}
	}

	// The rows are read a repo at a time, in the viewer's order, and the page
	// is read app by app: sorting by app keeps an app's environments together,
	// in the order the fold drew them.
	sort.SliceStable(health, func(i, j int) bool { return health[i].App < health[j].App })
	sort.Strings(healthFailed)
	sort.Strings(healthUnread)
	sort.Strings(healthMissing)

	return &Deploys{
		Runs:          rows,
		Health:        health,
		HealthFailed:  healthFailed,
		HealthUnread:  healthUnread,
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
func (dr deployRun) key() string { return dr.repo + "#" + strconv.FormatInt(dr.run.ID, 10) }

// runList reads one repo's deploy runs, newest first and at most
// deployRunsKept of them: every run of either deploy workflow whatever its
// event, since a tag push, a manual run and a teardown run are all deploys,
// and a landing's staging deploy is one the operator has to see. The two
// workflows share the cap, so a busy week of releases cannot crowd out the
// staging runs, or the other way round.
func (r *DeployReader) runList(ctx context.Context, name, owner, repo string) ([]deployRun, error) {
	list, err := r.api.ListRuns(ctx, owner, repo, forgejo.RunFilter{Limit: deployRunPage})
	if err != nil {
		return nil, err
	}
	out := make([]deployRun, 0, len(list.Runs))
	for _, run := range list.Runs {
		workflow := deployRunWorkflow(run.WorkflowID)
		if workflow == "" {
			continue
		}
		out = append(out, deployRun{owner: owner, name: repo, repo: name, workflow: workflow, run: run, created: parseRunStamp(run.Created)})
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
// draw, whether the repo's health runs could be listed at all, and the newest
// run's word on the app — unread, or failed.
type healthRead struct {
	rows   []DeployHealthRow
	listed bool
	unread bool
	failed bool
}

// healthRows reads one repo's scheduled health checks and folds them into the
// block's rows: the app's newest healthRunWindow runs of health.yml as one row
// per app and environment, plus the newest run's word on the app. The jobs of
// every run in the window are fetched because an environment's result is not on
// the run, and a run whose jobs were all skipped is green there while nothing
// was tested (gt-t9w60).
//
// A listing that failed says so rather than reading as a repo with no checks:
// its runs are the deploy runs' own listing, so it is a repo the block could not
// ask, not one that answered nothing.
func (r *DeployReader) healthRows(ctx context.Context, name, owner, repo string) healthRead {
	// The workflow filter is the API's own, so the page holds the health runs
	// and not the repo's whole history; the base name is still matched below,
	// the way the deploy list matches it.
	list, err := r.api.ListRuns(ctx, owner, repo, forgejo.RunFilter{WorkflowID: healthWorkflow, Page: 1, Limit: healthRunWindow})
	if err != nil {
		return healthRead{}
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
		jobs, err := r.api.ListRunJobs(ctx, owner, repo, window[i].run.ID)
		if err != nil {
			window[i].unread = true
			continue
		}
		window[i].envs = healthEnvsOf(jobs)
		window[i].failed = healthJobFailed(jobs)
	}
	rows, failed, unread := foldHealth(name, window)
	return healthRead{rows: rows, listed: true, failed: failed, unread: unread}
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
func foldHealth(name string, window []healthRun) (rows []DeployHealthRow, failed, unread bool) {
	app := healthApp(name)
	// The newest run is what the row's result comes from, so a window whose
	// newest run could not be read has no result to report whatever the runs
	// under it hold.
	unread = len(window) > 0 && window[0].unread
	read := len(window) > 0 && !unread
	rows = make([]DeployHealthRow, 0, len(healthEnvs))
	for _, env := range healthWindowEnvs(window) {
		row := DeployHealthRow{Repo: name, App: app, Env: env, Unread: !read}
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
	return rows, healthFailed(window), unread
}

// healthWindowEnvs names the environments a window carries, in the order the
// block draws them. A window whose jobs were read and named none draws no rows
// — the workflow checks no environment the block knows — but one whose newest
// run could not be read could name either, so the two it knows are drawn as
// rows it could not read rather than left blank (gt-t9w60).
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

// healthFailed reports whether an app's newest scheduled check failed: a job of
// the newest run that finished without passing, whatever its name. The row is
// per environment, so a failure in a job the two environments do not name would
// otherwise reach nobody — and the newest run's word is the block's only stand
// on the app (gt-t9w60). A run in flight has not failed, and an app that is off
// was skipped: neither raises a marker off an older run's failure, which would
// claim what the block never saw.
func healthFailed(window []healthRun) bool {
	return len(window) > 0 && !window[0].unread && window[0].failed
}

// healthJobFailed reports whether any job of a run finished without passing.
// Only failure and the canceled status count: a job waiting, running or blocked
// has not finished, and a skipped one is the app being off on purpose.
func healthJobFailed(jobs []forgejo.ActionRunJob) bool {
	for _, j := range jobs {
		if status := cloudText(j.Status, deployTextMax); status == healthFailure || status == forgejoRunCancelled {
			return true
		}
	}
	return false
}

// healthApp is the name an app is known by: the repo's own name, without the
// owner, which is what the strip's marker and the row's first cell carry.
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
