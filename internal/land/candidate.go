package land

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/forgejo"
)

// The candidate gate's timings. Each Forgejo call carries its own deadline
// inside the wait window, so a hung instance takes the infra retry instead of
// stalling the landing worker (slice 1 review note, gt-fn9e6.5).
const (
	// DefaultCandidatePollInterval is the wait between status reads.
	DefaultCandidatePollInterval = 15 * time.Second
	// DefaultCandidateCallTimeout bounds one Forgejo API call.
	DefaultCandidateCallTimeout = 30 * time.Second
	// DefaultCandidateWaitTimeout bounds the whole wait for a verdict: the CI
	// wait. A gate here takes 30 seconds to 3 minutes, so 20 minutes is a hung
	// or lost runner, not slow work. The landing's own deadline has to outlive
	// it (the daemon's landingWorkerLandTimeout), or this window is unreachable
	// (gt-fn9e6.26).
	DefaultCandidateWaitTimeout = 20 * time.Minute
	// candidateTailBytes bounds the job log fetched for a red verdict; the
	// rework note's excerpt is built from it.
	candidateTailBytes = 16 * 1024
	// candidateFullLogBytes bounds the second fetch, made only when the capped
	// tail names no failure at all: the failure can sit far enough from the end
	// that 16 KiB of passing packages hides it (gt-fn9e6.25).
	candidateFullLogBytes = 2 << 20
)

// ErrCISilence is why the candidate gate reports no verdict: nothing posted
// the required context within the wait window, or the run never finished. It
// is infrastructure — a down or hung runner — never a verdict on the work, so
// it takes the infra backoff rather than a rework (design: "CI silence is an
// infra retry").
var ErrCISilence = errors.New("the candidate gate reported no verdict")

// GateWorkflow identifies a rig's gate workflow by what CI reports it under:
// the workflow's name and the job whose verdict is the required commit status.
// Both are read from the workflow file, so renaming either is a one-file
// change and cannot silently orphan the branch-protection rule (design open
// question 1).
type GateWorkflow struct {
	// Name is the workflow's `name:` line, the left half of the context.
	Name string
	// Job is the single job key under `jobs:`.
	Job string
}

// Context is the commit status the gate job reports for a push to a candidate
// branch: "<workflow name> / <job> (push)", e.g. "ci / gate (push)". It is the
// push context, not the PR context a `land/<bead>` PR also gets on its merge
// ref — that one tests a different tree (design, "Where the epic cannot be
// followed exactly").
func (w GateWorkflow) Context() string {
	return fmt.Sprintf("%s / %s (push)", w.Name, w.Job)
}

// GateWorkflowPath is where a rig's gate workflow lives in its tree.
func GateWorkflowPath(workflow string) string {
	return filepath.Join(".forgejo", "workflows", workflow+".yml")
}

// ParseGateWorkflow reads a gate workflow file's identity. The file must name
// the workflow and hold exactly one job: an identity this cannot read is a
// context that cannot be derived, which fails the landing closed rather than
// polling a guess.
func ParseGateWorkflow(data []byte) (GateWorkflow, error) {
	var doc struct {
		Name string         `yaml:"name"`
		Jobs map[string]any `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return GateWorkflow{}, fmt.Errorf("parsing the gate workflow: %w", err)
	}
	name := strings.TrimSpace(doc.Name)
	if name == "" {
		return GateWorkflow{}, errors.New("the gate workflow has no name:, so the status context it reports cannot be derived")
	}
	if len(doc.Jobs) != 1 {
		return GateWorkflow{}, fmt.Errorf("the gate workflow has %d jobs, want exactly one: the required context is <name> / <job> (push)", len(doc.Jobs))
	}
	job := ""
	for key := range doc.Jobs {
		job = strings.TrimSpace(key)
	}
	if job == "" {
		return GateWorkflow{}, errors.New("the gate workflow's job has no name, so the status context it reports cannot be derived")
	}
	return GateWorkflow{Name: name, Job: job}, nil
}

// LoadGateWorkflow reads the gate workflow from dir's tree: the tree CI is
// about to test, which is the one that owns the context.
func LoadGateWorkflow(dir, workflow string) (GateWorkflow, error) {
	path := GateWorkflowPath(workflow)
	data, err := os.ReadFile(filepath.Join(dir, path)) //nolint:gosec // G304: the tree being gated
	if err != nil {
		return GateWorkflow{}, fmt.Errorf("reading the gate workflow %s: %w", path, err)
	}
	wf, err := ParseGateWorkflow(data)
	if err != nil {
		return GateWorkflow{}, fmt.Errorf("%s: %w", path, err)
	}
	return wf, nil
}

// RepoFromRemoteURL splits a Forgejo remote URL into the repository's owner
// and name: "https://forgejo.example/gastown/gastown.git" is
// ("gastown", "gastown").
func RepoFromRemoteURL(raw string) (owner, repo string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", fmt.Errorf("forgejo remote %q: %w", raw, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", "", fmt.Errorf("forgejo remote %q is not a URL with a scheme and host, so the repository it names cannot be addressed", raw)
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[len(parts)-2] == "" || parts[len(parts)-1] == "" {
		return "", "", fmt.Errorf("forgejo remote %q does not name an owner and a repository", raw)
	}
	return parts[len(parts)-2], strings.TrimSuffix(parts[len(parts)-1], ".git"), nil
}

// APIBaseFromRemoteURL is the API root of the instance a Forgejo remote URL
// names: "<scheme>://<host>/api/v1".
func APIBaseFromRemoteURL(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("forgejo remote %q is not a URL with a scheme and host, so its API root cannot be derived", raw)
	}
	return u.Scheme + "://" + u.Host + "/api/v1", nil
}

// CandidateStatus is the part of the Forgejo client the candidate gate uses:
// the required context's status, the run that posted it, that job's log, and
// the delete of the branch a terminal outcome leaves behind. *forgejo.Client
// implements it; tests pass a fake.
type CandidateStatus interface {
	CombinedStatus(ctx context.Context, owner, repo, ref string) (*forgejo.CombinedStatus, error)
	ListRuns(ctx context.Context, owner, repo string, f forgejo.RunFilter) (*forgejo.RunList, error)
	ListRunJobs(ctx context.Context, owner, repo string, runID int64) ([]forgejo.ActionRunJob, error)
	JobLogsTail(ctx context.Context, owner, repo string, jobID, maxBytes int64) (string, error)
	DeleteBranch(ctx context.Context, owner, repo, branch string) error
}

// CandidateState is what the candidate gate saw on the pushed commit.
type CandidateState int

const (
	// CandidateSilent is no verdict: the required context had not reported, or
	// stayed pending, through the whole wait window. Infrastructure.
	CandidateSilent CandidateState = iota
	// CandidatePassed is the required context reporting success.
	CandidatePassed
	// CandidateFailed is the required context reporting anything but success:
	// the work's, and the polecat's to fix.
	CandidateFailed
)

// CandidateResult is one run of the candidate gate.
type CandidateResult struct {
	State CandidateState
	// Branch and SHA are the candidate the verdict is about: land/<bead> and
	// the merged commit pushed on it.
	Branch string
	SHA    string
	// Pushed is set when this run put the branch on the remote and read it back
	// at SHA. A run that failed before the push, or whose push did not take,
	// leaves it false, and the branch that may already be there is some other
	// landing's to delete (gt-k796q).
	Pushed bool
	// Context is the required commit status polled.
	Context string
	// Tail is the failing job's log tail, for CandidateFailed.
	Tail string
	// RunStatus is the status of the run that tested SHA: for a red context,
	// whether the run behind it ran, or ended without judging the work. It is
	// "unknown" when no run could be read, so a red verdict never claims a run
	// it did not see (gt-fn9e6.16).
	RunStatus string
	// Err is why no verdict was reached — the workflow file, the push, the API
	// — or that the wait ended with nothing reported. A result carrying Err is
	// infrastructure whatever State says.
	Err error
}

// Candidate is the CI stand-in for the local gate on a cut-over rig.
// *CandidateGate is the production one; tests pass a fake. A Lander with no
// Candidate keeps today's local Gate.
type Candidate interface {
	// Run pushes head as w's merge candidate from the worktree wt, then returns
	// the gate workflow's verdict on that commit. dir is wt's tree, where the
	// gate workflow is read from.
	Run(ctx context.Context, wt Repo, dir string, w Work, head string) CandidateResult
	// Discard deletes the branch res pushed, for a landing that ends without
	// merging its candidate (gt-k796q). Best-effort.
	Discard(ctx context.Context, w Work, res CandidateResult)
}

// CandidateGate pushes a landing's merge candidate as land/<bead> and reads
// the gate workflow's verdict on that commit from Forgejo. It replaces the
// local Gate once a rig has a merge_queue.forgejo block.
type CandidateGate struct {
	// Client is the Forgejo API.
	Client CandidateStatus
	// Owner and Repo name the Forgejo repository the candidate is pushed to.
	Owner, RepoName string
	// Workflow is the gate workflow's file name — config's
	// merge_queue.forgejo.gate_workflow, e.g. "gate" for
	// .forgejo/workflows/gate.yml. The required context and the job whose log a
	// red verdict carries are read from that file, never typed here. Empty
	// means config.DefaultGateWorkflow.
	Workflow string
	// Remote is the git remote the candidate is pushed to; "" means origin.
	// It is a remote name, not merge_queue.forgejo.remote_url: the daemon
	// resolves the configured URL to the remote that carries it and sets this
	// (gt-fn9e6.9), so a cut-over rig's candidate reaches the CI that gates it.
	Remote string
	// PollInterval, CallTimeout and WaitTimeout bound the poll; a zero means
	// the DefaultCandidate* constant.
	PollInterval, CallTimeout, WaitTimeout time.Duration
	// Out, when set, receives one line per poll, so a long wait is visible
	// beside the rest of the landing's output.
	Out io.Writer
}

const (
	// candidateRunPage bounds the runs listed when a red verdict is looking
	// for the job whose log it carries.
	candidateRunPage = 20
	// candidateJobFailure is the job status Forgejo reports for a job that ran
	// and failed, the fallback when the named job is not in the run.
	candidateJobFailure = "failure"
	// candidateRunFailure is the run status Forgejo reports for a run that ran
	// and came back red: the one status that is a verdict on the work.
	candidateRunFailure = "failure"
	// candidateRunCancelled and candidateRunSkipped are the terminal run
	// statuses a run reaches without judging the work. A cancel is what an
	// operator's cancel, a runner restart or a dind recreate leaves behind
	// (verified on Forgejo 16.0.5, gt-fn9e6.16). The infrastructure signature
	// table carries them (signatures.go).
	candidateRunCancelled = "cancelled" //nolint:misspell // the run status Forgejo reports
	candidateRunSkipped   = "skipped"
	// candidateRunUnknown goes in a result whose red context's run could not be
	// found or read.
	candidateRunUnknown = "unknown"
)

// Run pushes head as w's candidate branch and waits for the gate workflow's
// verdict on it.
func (g *CandidateGate) Run(ctx context.Context, wt Repo, dir string, w Work, head string) CandidateResult {
	res := CandidateResult{Branch: w.Candidate(), SHA: head}
	wf, err := LoadGateWorkflow(dir, g.workflow())
	if err != nil {
		res.Err = err
		return res
	}
	res.Context = wf.Context()
	if err := g.push(wt, res.Branch, head); err != nil {
		res.Err = err
		return res
	}
	res.Pushed = true
	return g.wait(ctx, wf, res)
}

// Discard deletes the candidate branch res pushed, so a landing that ends red
// or infra does not leave it on the remote for good: only a later attempt's
// force-push would replace it, and a bead that is rejected or abandoned never
// gets one (gt-k796q). A result whose run never pushed the branch is left
// alone — a branch already there belongs to whatever put it there. It is
// best-effort: a failed delete is logged and the landing's outcome, already
// decided, does not change.
func (g *CandidateGate) Discard(ctx context.Context, w Work, res CandidateResult) {
	if !res.Pushed {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, g.callTimeout())
	defer cancel()
	if err := g.Client.DeleteBranch(ctx, g.Owner, g.RepoName, res.Branch); err != nil {
		g.logf("%s: could not delete the candidate %s left by the gate: %v", w.BeadID, res.Branch, err)
		return
	}
	g.logf("%s: deleted the candidate %s left by the gate", w.BeadID, res.Branch)
}

// push force-updates the candidate branch to head and reads it back. The
// branch belongs to the landing bot (land/** is push-whitelisted to it), and
// every retry rebuilds it, so the new tip replaces the old rather than racing
// it.
func (g *CandidateGate) push(wt Repo, branch, head string) error {
	remote := g.remote()
	if err := wt.Push(remote, head+":refs/heads/"+branch, true); err != nil {
		return fmt.Errorf("pushing the candidate %s: %w", branch, err)
	}
	tip, err := wt.PushRemoteBranchTip(remote, branch)
	if err != nil {
		return fmt.Errorf("reading the candidate %s back: %w", branch, err)
	}
	if tip != head {
		return fmt.Errorf("the candidate %s is %s after the push, not the pushed %s", branch, shortSHA(tip), shortSHA(head))
	}
	return nil
}

// wait polls the required context until it reports, or the wait window ends
// with nothing reported (silence).
func (g *CandidateGate) wait(parent context.Context, wf GateWorkflow, res CandidateResult) CandidateResult {
	ctx, cancel := context.WithTimeout(parent, g.waitTimeout())
	defer cancel()
	for {
		state, err := g.state(ctx, wf, &res)
		switch {
		case err != nil:
			res.Err = err
			return res
		case state == CandidatePassed:
			res.State = CandidatePassed
			return res
		case state == CandidateFailed:
			// redVerdict read the job log with the signature check; its
			// excerpt for the rework note is already on the result.
			res.State = CandidateFailed
			return res
		}
		g.logf("%s: %s has not reported on %s yet; checking again in %s", res.Branch, wf.Context(), shortSHA(res.SHA), g.pollInterval())
		timer := time.NewTimer(g.pollInterval())
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			if parent.Err() != nil {
				// The landing was canceled or ran out of time: its error, not
				// a verdict about CI.
				res.Err = parent.Err()
				return res
			}
			res.Err = fmt.Errorf("%w: %s reported nothing on %s within %s", ErrCISilence, wf.Context(), shortSHA(res.SHA), g.waitTimeout())
			return res
		}
	}
}

// state reads the required context's status on the candidate commit. A context
// that has not reported and one still pending are both "no verdict yet": the
// wait window decides when the second becomes silence.
func (g *CandidateGate) state(parent context.Context, wf GateWorkflow, res *CandidateResult) (CandidateState, error) {
	ctx, cancel := context.WithTimeout(parent, g.callTimeout())
	defer cancel()
	combined, err := g.Client.CombinedStatus(ctx, g.Owner, g.RepoName, res.SHA)
	if err != nil {
		return CandidateSilent, fmt.Errorf("reading %s on %s: %w", wf.Context(), shortSHA(res.SHA), err)
	}
	status := combined.StatusFor(wf.Context())
	if status == nil || status.Status == forgejo.StatePending {
		return CandidateSilent, nil
	}
	if status.Status == forgejo.StateSuccess {
		return CandidatePassed, nil
	}
	// failure, error, warning, skipped: the context is required, so anything
	// but success is the gate not passing, unless a signature in the
	// infrastructure table claims the red (signatures.go).
	return g.redVerdict(ctx, wf, res, status.Status)
}

// redVerdict reads the run behind a red required context and consults the
// infrastructure signature table (signatures.go). A signature match is
// infrastructure -- the runner or the environment failed before the work was
// judged -- and returns ErrCISilence, so the worker takes the infra backoff
// instead of sending the polecat a rework for a run it did not fail
// (gt-fn9e6.16, gt-fn9e6.30). No match leaves the red a verdict on the work,
// and the result carries the job's log for the rework note.
func (g *CandidateGate) redVerdict(ctx context.Context, wf GateWorkflow, res *CandidateResult, contextState forgejo.CommitState) (CandidateState, error) {
	run, err := g.runFor(ctx, res.SHA)
	if err != nil {
		// No run to read: the red stands, and the result records that the run
		// behind it is unknown rather than reporting a failure it never saw.
		res.RunStatus = candidateRunUnknown
		g.logf("%s is %s on %s and the run that tested it could not be read: %v; the red stands", wf.Context(), contextState, shortSHA(res.SHA), err)
		return CandidateFailed, nil
	}
	res.RunStatus = run.Status
	facts := gateFacts{RunStatus: run.Status}
	log, whole, logErr := g.jobLog(ctx, wf, run)
	if logErr != nil {
		// No log to read: the status-only signatures still apply, and the red
		// stands without a note when none matches.
		g.logf("%s: the gate job log on %s could not be read: %v", wf.Context(), shortSHA(res.SHA), logErr)
	} else {
		facts.Log, facts.LogWhole = log, whole
	}
	if sig, ok := matchInfraSignature(facts); ok {
		return CandidateSilent, fmt.Errorf("%w: %s is %s on %s; the run is %s: %s",
			ErrCISilence, wf.Context(), contextState, shortSHA(res.SHA), run.Status, sig.name)
	}
	if logErr == nil {
		res.Tail = failureExcerpt(log, gateTailLines)
	}
	return CandidateFailed, nil
}

// runFor is the run that tested sha: the one whose job log carries a red
// verdict, and whose status says whether that verdict was ever reached.
func (g *CandidateGate) runFor(parent context.Context, sha string) (forgejo.ActionRun, error) {
	ctx, cancel := context.WithTimeout(parent, g.callTimeout())
	defer cancel()
	runs, err := g.Client.ListRuns(ctx, g.Owner, g.RepoName, forgejo.RunFilter{
		Event: []string{"push"}, HeadSHA: sha, Limit: candidateRunPage,
	})
	if err != nil {
		return forgejo.ActionRun{}, err
	}
	for _, run := range runs.Runs {
		if run.CommitSHA == sha {
			return run, nil
		}
	}
	return forgejo.ActionRun{}, fmt.Errorf("no workflow run tested %s", shortSHA(sha))
}

// jobLog reads the gate job's log in run. It returns the log and whether the
// fetch came back whole: the server returned less than the tail cap, so the
// head -- where the runner prints a step marker -- is in hand. A tail that names
// no failure is widened to the whole log, still bounded, so the rework note
// names the failure even when it sits far from the end (gt-fn9e6.25).
func (g *CandidateGate) jobLog(parent context.Context, wf GateWorkflow, run forgejo.ActionRun) (string, bool, error) {
	ctx, cancel := context.WithTimeout(parent, g.callTimeout())
	defer cancel()
	jobs, err := g.Client.ListRunJobs(ctx, g.Owner, g.RepoName, run.ID)
	if err != nil {
		return "", false, err
	}
	jobID := int64(0)
	for _, job := range jobs {
		if job.Name == wf.Job {
			jobID = job.ID
			break
		}
	}
	if jobID == 0 {
		for _, job := range jobs {
			if job.Status == candidateJobFailure {
				jobID = job.ID
				break
			}
		}
	}
	if jobID == 0 {
		return "", false, fmt.Errorf("run %d has no job %q and no failed job", run.ID, wf.Job)
	}
	log, err := g.Client.JobLogsTail(ctx, g.Owner, g.RepoName, jobID, candidateTailBytes)
	if err != nil {
		return "", false, err
	}
	whole := len(log) < candidateTailBytes
	if !logHasFailure(log) {
		// The failure can be outside the byte cap. go test prints its packages
		// in order, so the failing package is usually not the last one, and a
		// tail of passing packages hides the reason for the red entirely: fetch
		// the whole log once, still bounded, before falling back to the tail
		// (gt-fn9e6.25).
		full, ferr := g.Client.JobLogsTail(ctx, g.Owner, g.RepoName, jobID, candidateFullLogBytes)
		switch {
		case ferr != nil:
			g.logf("%s: the tail of the gate job log on %s names no failure and the whole log could not be read: %v", wf.Context(), shortSHA(run.CommitSHA), ferr)
		case logHasFailure(full):
			log = full
		}
	}
	return log, whole, nil
}

// failureExcerpt is the rework note's excerpt of a gate job log: the failure
// itself, wherever it sits in the log, not just the last lines. Every failure
// marker keeps the output printed with it -- for a "--- FAIL" line, the lines
// the test printed under it up to the next block -- plus a few lines of
// context, de-duplicated and in log order. A log with no marker at all falls
// back to its last lines. The excerpt is at most budget lines, each with the
// Forgejo timestamp prefix stripped (gt-fn9e6.25).
func failureExcerpt(log string, budget int) string {
	lines := logLines(log)
	if len(lines) == 0 || budget <= 0 {
		return ""
	}
	kept := failureLines(lines)
	if len(kept) == 0 {
		return lastLines(strings.Join(lines, "\n")+"\n", budget)
	}
	kept = trimToBudget(kept, budget)
	out := make([]string, len(kept))
	for i, at := range kept {
		out[i] = lines[at]
	}
	return strings.Join(out, "\n") + "\n"
}

// logHasFailure reports whether a job log names a failure at all: what decides
// whether a byte-capped tail is enough or the whole log has to be fetched.
func logHasFailure(log string) bool {
	for _, line := range logLines(log) {
		if isFailureMarker(line) {
			return true
		}
	}
	return false
}

// failureLines is the excerpt's line indices: every failure marker with the
// range it carries, de-duplicated and in log order.
func failureLines(lines []string) []int {
	keep := map[int]bool{}
	for i, line := range lines {
		if !isFailureMarker(line) {
			continue
		}
		lo, hi := failureSpan(lines, i)
		for j := lo; j <= hi; j++ {
			keep[j] = true
		}
	}
	if len(keep) == 0 {
		return nil
	}
	idx := make([]int, 0, len(keep))
	for i := range keep {
		idx = append(idx, i)
	}
	slices.Sort(idx)
	return idx
}

// failureSpan is the line range one failure marker carries. A "--- FAIL" block
// runs to the next block boundary, so the test's own output comes with it; any
// other marker takes failureContextLines either side of itself.
func failureSpan(lines []string, at int) (int, int) {
	lo := max(at-failureContextLines, 0)
	if !failedTestRE.MatchString(lines[at]) {
		return lo, min(at+failureContextLines, len(lines)-1)
	}
	hi := at
	for hi+1 < len(lines) && !startsLogBlock(lines[hi+1]) {
		hi++
	}
	return lo, hi
}

// startsLogBlock reports whether line begins a fresh go test or make block,
// where a "--- FAIL" block's output ends.
func startsLogBlock(line string) bool {
	return failedTestRE.MatchString(line) || failLineRE.MatchString(line) ||
		packageDoneRE.MatchString(line) || panicLineRE.MatchString(line) ||
		makeErrorRE.MatchString(line) || strings.HasPrefix(line, "=== ") ||
		strings.Contains(line, buildFailedMarker)
}

// trimToBudget brings an over-long excerpt back to budget lines, keeping the
// earliest failures: it drops the last run of kept lines whole (a later
// failure is usually the earlier one's cascade), and a single run still over
// budget keeps its head, where the failure and the output under it are.
func trimToBudget(idx []int, budget int) []int {
	for len(idx) > budget {
		if start := lastRunStart(idx); start > 0 {
			idx = idx[:start]
			continue
		}
		return idx[:budget]
	}
	return idx
}

// lastRunStart is where the last contiguous run of idx begins. An excerpt of
// one failure block plus the closing make error is two runs; the gap between
// them is the log the excerpt left out.
func lastRunStart(idx []int) int {
	start := len(idx) - 1
	for start > 0 && idx[start-1]+1 == idx[start] {
		start--
	}
	return start
}

// The markers that say a gate log went red: a "--- FAIL" per test and a "FAIL"
// per package (go test, or "[build failed]" on the package line when the build
// never ran), a bare "panic:" (a panicking test prints no "--- FAIL" line), and
// make's closing "*** ... Error N".
var (
	failLineRE    = regexp.MustCompile(`^FAIL\b`)
	packageDoneRE = regexp.MustCompile(`^(ok|PASS|SKIP)\b`)
	panicLineRE   = regexp.MustCompile(`^panic: `)
	makeErrorRE   = regexp.MustCompile(`^make(\[\d+\])?: \*\*\*.*Error \d+`)
)

// buildFailedMarker is what go test writes on the package line of a package
// that never compiled.
const buildFailedMarker = "[build failed]"

// failureContextLines is how much context either side of a marker the excerpt
// keeps without one: enough to see what the gate was doing when it stopped.
const failureContextLines = 2

func isFailureMarker(line string) bool {
	return failedTestRE.MatchString(line) || failLineRE.MatchString(line) ||
		panicLineRE.MatchString(line) || makeErrorRE.MatchString(line) ||
		strings.Contains(line, buildFailedMarker)
}

// forgejoTimestampRE is the timestamp Forgejo prefixes every line of a job log
// with.
var forgejoTimestampRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z `)

// logLines splits a job log into lines with the per-line timestamp stripped.
func logLines(log string) []string {
	log = strings.TrimRight(log, "\n")
	if strings.TrimSpace(log) == "" {
		return nil
	}
	lines := strings.Split(log, "\n")
	for i, line := range lines {
		lines[i] = forgejoTimestampRE.ReplaceAllString(line, "")
	}
	return lines
}

// VerifyReported reports whether the required context has reported on a
// commit this rig landed, the startup check that catches a renamed workflow or
// job before a landing waits on a context that never arrives (design open
// question 1).
func (g *CandidateGate) VerifyReported(parent context.Context, commit, statusContext string) error {
	ctx, cancel := context.WithTimeout(parent, g.callTimeout())
	defer cancel()
	combined, err := g.Client.CombinedStatus(ctx, g.Owner, g.RepoName, commit)
	if err != nil {
		return fmt.Errorf("reading statuses on %s: %w", shortSHA(commit), err)
	}
	if combined.StatusFor(statusContext) == nil {
		return fmt.Errorf("no %s status has reported on %s; the workflow or its job may have been renamed", statusContext, shortSHA(commit))
	}
	return nil
}

func (g *CandidateGate) workflow() string {
	if g.Workflow == "" {
		return config.DefaultGateWorkflow
	}
	return g.Workflow
}

func (g *CandidateGate) remote() string {
	if g.Remote == "" {
		return "origin"
	}
	return g.Remote
}

func (g *CandidateGate) pollInterval() time.Duration {
	return nonZero(g.PollInterval, DefaultCandidatePollInterval)
}

func (g *CandidateGate) callTimeout() time.Duration {
	return nonZero(g.CallTimeout, DefaultCandidateCallTimeout)
}

func (g *CandidateGate) waitTimeout() time.Duration {
	return nonZero(g.WaitTimeout, DefaultCandidateWaitTimeout)
}

func (g *CandidateGate) logf(format string, args ...any) {
	if g.Out != nil {
		_, _ = fmt.Fprintf(g.Out, "[land] "+format+"\n", args...)
	}
}

func nonZero(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}
