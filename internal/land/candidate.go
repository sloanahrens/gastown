package land

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
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
	// DefaultCandidateWaitTimeout bounds the whole wait for a verdict.
	DefaultCandidateWaitTimeout = 45 * time.Minute
	// candidateTailBytes bounds the job log fetched for a red verdict; the
	// rework note keeps the last gateTailLines of it.
	candidateTailBytes = 16 * 1024
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
// the required context's status, the run that posted it, and that job's log.
// *forgejo.Client implements it; tests pass a fake.
type CandidateStatus interface {
	CombinedStatus(ctx context.Context, owner, repo, ref string) (*forgejo.CombinedStatus, error)
	ListRuns(ctx context.Context, owner, repo string, f forgejo.RunFilter) (*forgejo.RunList, error)
	ListRunJobs(ctx context.Context, owner, repo string, runID int64) ([]forgejo.ActionRunJob, error)
	JobLogsTail(ctx context.Context, owner, repo string, jobID, maxBytes int64) (string, error)
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
	// Context is the required commit status polled.
	Context string
	// Tail is the failing job's log tail, for CandidateFailed.
	Tail string
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
}

// CandidateGate pushes a landing's merge candidate as land/<bead> and reads
// the gate workflow's verdict on that commit from Forgejo. It replaces the
// local Gate once a rig has a merge_queue.forgejo block; LandGate stays built
// because shadow mode uses it (slice 8).
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
	// It is a remote name, not merge_queue.forgejo.remote_url: which remote a
	// cut-over rig pushes to is the repoint slice's (slice 9).
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
	return g.wait(ctx, wf, res)
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
		state, err := g.state(ctx, wf, res)
		switch {
		case err != nil:
			res.Err = err
			return res
		case state == CandidatePassed:
			res.State = CandidatePassed
			return res
		case state == CandidateFailed:
			res.State = CandidateFailed
			res.Tail = g.failureTail(ctx, wf, res.SHA)
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
func (g *CandidateGate) state(parent context.Context, wf GateWorkflow, res CandidateResult) (CandidateState, error) {
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
	// but success is the gate not passing.
	return CandidateFailed, nil
}

// failureTail is the failing job's log tail, what the rework note carries. A
// tail it cannot fetch is empty: the verdict stands without it.
func (g *CandidateGate) failureTail(ctx context.Context, wf GateWorkflow, sha string) string {
	tail, err := g.jobTail(ctx, wf, sha)
	if err != nil {
		g.logf("%s: the candidate gate failed on %s and its job log could not be read: %v", wf.Context(), shortSHA(sha), err)
		return ""
	}
	return tail
}

// jobTail is the tail of the gate job's log in the run that tested sha.
func (g *CandidateGate) jobTail(parent context.Context, wf GateWorkflow, sha string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, g.callTimeout())
	defer cancel()
	runs, err := g.Client.ListRuns(ctx, g.Owner, g.RepoName, forgejo.RunFilter{
		Event: []string{"push"}, HeadSHA: sha, Limit: candidateRunPage,
	})
	if err != nil {
		return "", err
	}
	runID := int64(0)
	for _, run := range runs.Runs {
		if run.CommitSHA == sha {
			runID = run.ID
			break
		}
	}
	if runID == 0 {
		return "", fmt.Errorf("no workflow run tested %s", shortSHA(sha))
	}
	jobs, err := g.Client.ListRunJobs(ctx, g.Owner, g.RepoName, runID)
	if err != nil {
		return "", err
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
		return "", fmt.Errorf("run %d has no job %q and no failed job", runID, wf.Job)
	}
	log, err := g.Client.JobLogsTail(ctx, g.Owner, g.RepoName, jobID, candidateTailBytes)
	if err != nil {
		return "", err
	}
	return lastLines(log, gateTailLines), nil
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
