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
// the workflow's needs give. It reads what the Forgejo panel reads — the same
// read-only viewer client and the same repos — and adds nothing of its own:
// the run list, the run's stamps and its jobs are all the evidence it has. The
// runner's own health is not among them. GET
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

// Deploys is the Cloud section's Deploys block: the newest deploy runs the
// viewer can see, of either workflow. Missing names the repos whose runs could
// not be read, so a repo that failed is named rather than reading as a repo
// with no deploys; Error is the class of a refresh that read no repo at all,
// which the block's note carries.
type Deploys struct {
	Runs    []DeployRun `json:"runs"`
	Missing []string    `json:"missing,omitempty"`
	Error   string      `json:"error,omitempty"`
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

	return &Deploys{Runs: rows, Missing: missing}
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
