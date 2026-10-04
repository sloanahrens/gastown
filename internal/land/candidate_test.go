package land

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/internal/forgejo"
)

// gateWorkflowYAML is a gate workflow as slice 10 writes it: the workflow is
// named ci and holds one job, gate, which is what makes the required context
// "ci / gate (push)".
const gateWorkflowYAML = `name: ci
on:
  push:
    branches: [land/**]
jobs:
  gate:
    runs-on: forgejo-runner
    steps:
      - run: make gate
`

// writeGateWorkflow puts the gate workflow in a fresh tree and returns the
// directory, standing in for the candidate worktree the gate is read from.
func writeGateWorkflow(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, GateWorkflowPath("gate"))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// fakeForgejo answers the candidate gate's Forgejo calls from canned data.
// statuses gives one whole status per poll and the last one repeats, so a test
// drives pending -> success without a second clock. runs carries each run's
// status, so a test says whether the run behind a red context ran, was
// cancelled, or was never there.
type fakeForgejo struct {
	mu       sync.Mutex
	statuses []forgejo.CommitStatus
	polls    int
	runs     []forgejo.ActionRun
	runsErr  error
	jobs     []forgejo.ActionRunJob
	log      string
	err      error
	logErr   error
}

func (f *fakeForgejo) CombinedStatus(context.Context, string, string, string) (*forgejo.CombinedStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls++
	if f.err != nil {
		return nil, f.err
	}
	combined := &forgejo.CombinedStatus{}
	if len(f.statuses) > 0 {
		i := min(f.polls, len(f.statuses)) - 1
		combined.Statuses = []forgejo.CommitStatus{f.statuses[i]}
	}
	return combined, nil
}

func (f *fakeForgejo) ListRuns(context.Context, string, string, forgejo.RunFilter) (*forgejo.RunList, error) {
	if f.runsErr != nil {
		return nil, f.runsErr
	}
	return &forgejo.RunList{Runs: f.runs}, nil
}

func (f *fakeForgejo) ListRunJobs(context.Context, string, string, int64) ([]forgejo.ActionRunJob, error) {
	return f.jobs, nil
}

func (f *fakeForgejo) JobLogsTail(context.Context, string, string, int64, int64) (string, error) {
	return f.log, f.logErr
}

func status(state forgejo.CommitState, context string) forgejo.CommitStatus {
	return forgejo.CommitStatus{Status: state, Context: context}
}

// gate polls fast, so a test's wait window is milliseconds rather than the
// production interval.
func fastGate(client CandidateStatus) *CandidateGate {
	return &CandidateGate{
		Client: client, Owner: "gastown", RepoName: "gastown", Workflow: "gate",
		PollInterval: time.Millisecond, CallTimeout: time.Second, WaitTimeout: 40 * time.Millisecond,
	}
}

func TestWorkCandidate(t *testing.T) {
	t.Parallel()
	if got := (Work{BeadID: "gt-abc"}).Candidate(); got != "land/gt-abc" {
		t.Errorf("Candidate() = %q, want land/gt-abc", got)
	}
	if got := (Work{BeadID: "gt-abc", CandidateBranch: "land/other"}).Candidate(); got != "land/other" {
		t.Errorf("Candidate() = %q, want the field to win", got)
	}
}

func TestParseGateWorkflow(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		body    string
		want    string
		wantErr string
	}{
		{name: "slice 10's gate workflow", body: gateWorkflowYAML, want: "ci / gate (push)"},
		{name: "missing name", body: "jobs:\n  gate:\n    runs-on: x\n", wantErr: "no name:"},
		{name: "two jobs", body: "name: ci\njobs:\n  gate:\n    runs-on: x\n  other:\n    runs-on: x\n", wantErr: "2 jobs"},
		{name: "no jobs", body: "name: ci\n", wantErr: "0 jobs"},
		{name: "unparsable", body: "name: [\n", wantErr: "parsing the gate workflow"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			wf, err := ParseGateWorkflow([]byte(tt.body))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseGateWorkflow: %v", err)
			}
			if got := wf.Context(); got != tt.want {
				t.Fatalf("Context() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestGastownGateWorkflowFixesTheRequiredContext pins this repo's committed
// gate workflow to the contract the candidate gate derives from it. The file
// is the single source of the required context "ci / gate (push)"; if its name
// or job key moves without the branch-protection rule moving too, the landing
// waits on a context nobody posts and every candidate goes silently red
// (gt-fn9e6.10).
func TestGastownGateWorkflowFixesTheRequiredContext(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	wf, err := LoadGateWorkflow(root, "gate")
	if err != nil {
		t.Fatalf("this repo's gate workflow: %v", err)
	}
	if wf.Name != "ci" || wf.Job != "gate" {
		t.Fatalf("gate workflow = %+v, want the ci workflow's gate job", wf)
	}
	if got := wf.Context(); got != "ci / gate (push)" {
		t.Fatalf("Context() = %q, want %q", got, "ci / gate (push)")
	}
}

// TestGastownGateWorkflowRunsTheMergeQueueGate holds the workflow to the same
// command the rig's merge_queue.gate names, so the local gate and the CI job
// cannot drift into testing different things (design: "the same targets").
func TestGastownGateWorkflowRunsTheMergeQueueGate(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, GateWorkflowPath("gate")))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Jobs map[string]struct {
			Steps []struct {
				Run string `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parsing this repo's gate workflow: %v", err)
	}
	var runs []string
	for _, step := range doc.Jobs["gate"].Steps {
		if step.Run != "" {
			runs = append(runs, step.Run)
		}
	}
	if len(runs) != 1 || runs[0] != "make gate" {
		t.Fatalf("gate job runs %q, want exactly [%q] — gastown's merge_queue.gate", runs, "make gate")
	}
}

func TestRepoFromRemoteURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		url        string
		owner, rep string
		wantErr    bool
	}{
		{url: "https://forgejo.example/gastown/gastown", owner: "gastown", rep: "gastown"},
		{url: "https://forgejo.example/gastown/gastown.git", owner: "gastown", rep: "gastown"},
		{url: "http://127.0.0.1:3000/sloan/gastown.git/", owner: "sloan", rep: "gastown"},
		{url: "git@forgejo.example:gastown/gastown.git", wantErr: true},
		{url: "https://forgejo.example/gastown", wantErr: true},
		{url: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			t.Parallel()
			owner, repo, err := RepoFromRemoteURL(tt.url)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("RepoFromRemoteURL(%q) = %q/%q, want an error", tt.url, owner, repo)
				}
				return
			}
			if err != nil {
				t.Fatalf("RepoFromRemoteURL(%q): %v", tt.url, err)
			}
			if owner != tt.owner || repo != tt.rep {
				t.Fatalf("RepoFromRemoteURL(%q) = %q/%q, want %q/%q", tt.url, owner, repo, tt.owner, tt.rep)
			}
		})
	}
}

func TestAPIBaseFromRemoteURL(t *testing.T) {
	t.Parallel()
	got, err := APIBaseFromRemoteURL("https://forgejo.example/gastown/gastown.git")
	if err != nil || got != "https://forgejo.example/api/v1" {
		t.Fatalf("APIBaseFromRemoteURL = %q, %v; want https://forgejo.example/api/v1", got, err)
	}
	if _, err := APIBaseFromRemoteURL("git@forgejo.example:gastown/gastown.git"); err == nil {
		t.Fatal("an scp-style remote has no API root; want an error")
	}
}

// TestCandidateGatePushesTheCandidateAndTakesTheVerdict is the slice's happy
// path: the merged commit goes up as land/<bead>, and the required context's
// success on that commit is the gate passing.
func TestCandidateGatePushesTheCandidateAndTakesTheVerdict(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	sha := f.git.Commit(t, f.repo, "main", "candidate", map[string]string{"c.txt": "x\n"})
	client := &fakeForgejo{statuses: []forgejo.CommitStatus{status(forgejo.StateSuccess, "ci / gate (push)")}}
	gate := fastGate(client)

	res := gate.Run(context.Background(), f.git.Open(f.repo), writeGateWorkflow(t, gateWorkflowYAML), f.work, sha)
	if res.Err != nil {
		t.Fatalf("Run: %v", res.Err)
	}
	if res.State != CandidatePassed {
		t.Fatalf("state = %v, want passed", res.State)
	}
	if res.Branch != "land/gt-abc" || res.SHA != sha || res.Context != "ci / gate (push)" {
		t.Fatalf("result %+v; want the candidate land/gt-abc@%s polled as ci / gate (push)", res, sha)
	}
	if got := f.git.Ref(f.origin, "refs/heads/land/gt-abc"); got != sha {
		t.Fatalf("origin land/gt-abc = %s, want the merged %s", got, sha)
	}
}

// TestCandidateGateRedCarriesTheJobLogTail: a failure verdict is the work's,
// and the rework note carries the failing job's log.
func TestCandidateGateRedCarriesTheJobLogTail(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	sha := f.git.Commit(t, f.repo, "main", "candidate", map[string]string{"c.txt": "x\n"})
	client := &fakeForgejo{
		statuses: []forgejo.CommitStatus{status(forgejo.StateFailure, "ci / gate (push)")},
		runs:     []forgejo.ActionRun{{ID: 7, CommitSHA: sha, Status: "failure"}},
		jobs:     []forgejo.ActionRunJob{{ID: 9, RunID: 7, Name: "gate", Status: "failure"}},
		log:      "make gate\n--- FAIL: TestThing\n",
	}
	res := fastGate(client).Run(context.Background(), f.git.Open(f.repo), writeGateWorkflow(t, gateWorkflowYAML), f.work, sha)
	if res.Err != nil {
		t.Fatalf("Run: %v", res.Err)
	}
	if res.State != CandidateFailed {
		t.Fatalf("state = %v, want failed", res.State)
	}
	if res.RunStatus != "failure" {
		t.Fatalf("RunStatus = %q, want the failed run's status", res.RunStatus)
	}
	if !strings.Contains(res.Tail, "--- FAIL: TestThing") {
		t.Fatalf("tail %q; want the failing job's log", res.Tail)
	}
}

// TestCandidateGateRunStatusDecidesTheRed: the run behind a red context decides
// whether the red reaches the polecat. Only a run that ran and failed is the
// work's; a run an operator cancelled — or a runner restart, or a dind recreate,
// which Forgejo reports the same way — never judged the work, so it takes the
// infra backoff instead of a rework (gt-fn9e6.16).
func TestCandidateGateRunStatusDecidesTheRed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		run       *forgejo.ActionRun
		runsErr   error
		wantState CandidateState
		// wantErr is the run status the infra result must name; "" means the red
		// verdict stands with no error.
		wantErr string
		wantRun string
	}{
		{name: "a failed run is the work's red", run: &forgejo.ActionRun{ID: 7, Status: "failure"},
			wantState: CandidateFailed, wantRun: "failure"},
		{name: "a cancelled run is infrastructure", run: &forgejo.ActionRun{ID: 7, Status: "cancelled"},
			wantState: CandidateSilent, wantErr: "cancelled", wantRun: "cancelled"},
		{name: "a skipped run is infrastructure", run: &forgejo.ActionRun{ID: 7, Status: "skipped"},
			wantState: CandidateSilent, wantErr: "skipped", wantRun: "skipped"},
		{name: "a run that has not finished keeps today's red", run: &forgejo.ActionRun{ID: 7, Status: "running"},
			wantState: CandidateFailed, wantRun: "running"},
		{name: "no run leaves the red unverified", wantState: CandidateFailed, wantRun: "unknown"},
		{name: "a run list that cannot be read leaves the red unverified",
			runsErr: errors.New("connection refused"), wantState: CandidateFailed, wantRun: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			sha := f.git.Commit(t, f.repo, "main", "candidate", map[string]string{"c.txt": "x\n"})
			client := &fakeForgejo{
				statuses: []forgejo.CommitStatus{status(forgejo.StateFailure, "ci / gate (push)")},
				runsErr:  tt.runsErr,
			}
			if tt.run != nil {
				tt.run.CommitSHA = sha
				client.runs = []forgejo.ActionRun{*tt.run}
			}
			res := fastGate(client).Run(context.Background(), f.git.Open(f.repo), writeGateWorkflow(t, gateWorkflowYAML), f.work, sha)
			if res.State != tt.wantState {
				t.Fatalf("state = %v, want %v (err %v)", res.State, tt.wantState, res.Err)
			}
			if tt.wantErr == "" {
				if res.Err != nil {
					t.Fatalf("err = %v; want the red verdict to stand with no error", res.Err)
				}
			} else if !errors.Is(res.Err, ErrCISilence) || !strings.Contains(res.Err.Error(), tt.wantErr) {
				t.Fatalf("err = %v; want one wrapping ErrCISilence and naming the %q run", res.Err, tt.wantErr)
			}
			if res.RunStatus != tt.wantRun {
				t.Fatalf("RunStatus = %q, want %q", res.RunStatus, tt.wantRun)
			}
		})
	}
}

// TestCandidateGateSilenceIsNoVerdict: a context that never reports inside the
// wait window is ErrCISilence — the infra retry — and never a rework.
func TestCandidateGateSilenceIsNoVerdict(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	sha := f.git.Commit(t, f.repo, "main", "candidate", map[string]string{"c.txt": "x\n"})
	client := &fakeForgejo{statuses: []forgejo.CommitStatus{status(forgejo.StatePending, "ci / gate (push)")}}
	res := fastGate(client).Run(context.Background(), f.git.Open(f.repo), writeGateWorkflow(t, gateWorkflowYAML), f.work, sha)
	if res.State != CandidateSilent {
		t.Fatalf("state = %v, want silent", res.State)
	}
	if !errors.Is(res.Err, ErrCISilence) {
		t.Fatalf("err = %v, want one wrapping ErrCISilence", res.Err)
	}
	if client.polls < 2 {
		t.Fatalf("polled %d times; want the wait to keep asking", client.polls)
	}
}

// TestCandidateGateAPIFailureIsNotAVerdict: a Forgejo that answers with an
// error fails the landing into the infra path rather than stalling it.
func TestCandidateGateAPIFailureIsNotAVerdict(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	sha := f.git.Commit(t, f.repo, "main", "candidate", map[string]string{"c.txt": "x\n"})
	res := fastGate(&fakeForgejo{err: errors.New("connection refused")}).
		Run(context.Background(), f.git.Open(f.repo), writeGateWorkflow(t, gateWorkflowYAML), f.work, sha)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "connection refused") {
		t.Fatalf("err = %v, want the API failure", res.Err)
	}
	if res.State != CandidateSilent {
		t.Fatalf("state = %v, want no verdict", res.State)
	}
}

// TestCandidateGateWithoutTheWorkflowFailsClosed: the context cannot be
// derived from a tree with no gate workflow, and a guess would poll a status
// nobody posts.
func TestCandidateGateWithoutTheWorkflowFailsClosed(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	sha := f.git.Commit(t, f.repo, "main", "candidate", map[string]string{"c.txt": "x\n"})
	res := fastGate(&fakeForgejo{}).Run(context.Background(), f.git.Open(f.repo), t.TempDir(), f.work, sha)
	if res.Err == nil || !strings.Contains(res.Err.Error(), "gate workflow") {
		t.Fatalf("err = %v, want the missing workflow", res.Err)
	}
	if got := f.git.Ref(f.origin, "refs/heads/land/gt-abc"); got != "" {
		t.Fatalf("origin land/gt-abc = %s; nothing should be pushed without a derivable context", got)
	}
}

// TestCandidateGateVerifyReported is the startup check: a required context
// that has never reported is caught before a landing waits on it.
func TestCandidateGateVerifyReported(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	missing := fastGate(&fakeForgejo{statuses: []forgejo.CommitStatus{status(forgejo.StateSuccess, "ci / other (push)")}})
	if err := missing.VerifyReported(ctx, "abc", "ci / gate (push)"); err == nil {
		t.Fatal("a context absent from the commit's statuses must be reported")
	}
	present := fastGate(&fakeForgejo{statuses: []forgejo.CommitStatus{status(forgejo.StateSuccess, "ci / gate (push)")}})
	if err := present.VerifyReported(ctx, "abc", "ci / gate (push)"); err != nil {
		t.Fatalf("VerifyReported: %v", err)
	}
}

// fakeCandidate stands in for the Forgejo gate inside Land, so a test drives a
// verdict without a repository or an HTTP server.
type fakeCandidate struct {
	calls []Work
	dirs  []string
	res   CandidateResult
}

func (c *fakeCandidate) Run(_ context.Context, _ Repo, dir string, w Work, head string) CandidateResult {
	c.calls = append(c.calls, w)
	c.dirs = append(c.dirs, dir)
	res := c.res
	if res.Branch == "" {
		res.Branch = w.Candidate()
	}
	if res.SHA == "" {
		res.SHA = head
	}
	return res
}

// TestLandLandsThroughTheCandidateGate: a rig with a candidate gate pushes the
// merge candidate and takes its CI verdict instead of running the local gate.
func TestLandLandsThroughTheCandidateGate(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	cand := &fakeCandidate{res: CandidateResult{State: CandidatePassed, Context: "ci / gate (push)"}}
	l.Candidate = cand

	res, err := l.Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	if len(f.gate.dirs) != 0 {
		t.Fatalf("the local gate ran %d time(s) on a rig landing through CI", len(f.gate.dirs))
	}
	if len(cand.calls) != 1 {
		t.Fatalf("the candidate gate ran %d time(s), want once", len(cand.calls))
	}
	if got := cand.calls[0]; got.CandidateBranch != "land/gt-abc" || got.CandidateHead != res.LandedCommit {
		t.Fatalf("Work handed to the gate = %+v; want the candidate land/gt-abc@%s", got, res.LandedCommit)
	}
	if got := f.originMain(); got != res.LandedCommit {
		t.Fatalf("origin/main = %s, want the landed %s", got, res.LandedCommit)
	}
	if res.Gate.Passed != true || len(res.Gate.Steps) != 1 || res.Gate.Steps[0].Name != StageCI {
		t.Fatalf("result gate = %+v; want one passing ci step", res.Gate)
	}
}

// TestLandCandidateRedIsReworkWithTheLogTail: a red CI verdict comes back as
// a gate rework the polecat resumes from, carrying the job log.
func TestLandCandidateRedIsReworkWithTheLogTail(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	cand := &fakeCandidate{res: CandidateResult{
		State: CandidateFailed, Context: "ci / gate (push)", Tail: "--- FAIL: TestThing\n"}}
	l.Candidate = cand

	_, err := l.Land(context.Background(), f.work)
	rej := f.assertRejected(t, err, RejectGate, LabelRework)
	if !strings.Contains(rej.Reason, "ci / gate (push)") {
		t.Fatalf("reason %q; want it to name the failing context", rej.Reason)
	}
	if !strings.Contains(f.bead().Notes, "--- FAIL: TestThing") {
		t.Fatalf("the rejection note carries no job log tail:\n%s", f.bead().Notes)
	}
}

// TestLandCandidateSilenceIsInfrastructure: CI reporting nothing is the infra
// retry, not a rejection the polecat would be sent back to fix.
func TestLandCandidateSilenceIsInfrastructure(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	cand := &fakeCandidate{res: CandidateResult{
		State: CandidateSilent, Context: "ci / gate (push)", Err: ErrCISilence}}
	l.Candidate = cand

	_, err := l.Land(context.Background(), f.work)
	var infra *InfraError
	if !errors.As(err, &infra) || !errors.Is(err, ErrCISilence) {
		t.Fatalf("Land error = %T %v, want an *InfraError wrapping ErrCISilence", err, err)
	}
	if infra.Stage != StageCI {
		t.Fatalf("stage = %q, want %q", infra.Stage, StageCI)
	}
	f.assertUntouched(t)
}

// shadowLandingRecord is the one landing record f's file holds.
func shadowLandingRecord(t *testing.T, f *landFixture) LandingRecord {
	t.Helper()
	lines := f.landingLines()
	if len(lines) != 1 {
		t.Fatalf("landings file has %d records, want one", len(lines))
	}
	var rec LandingRecord
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("unmarshal landing record: %v", err)
	}
	return rec
}

// TestLandShadowModeRecordsBothVerdicts: a shadow-mode rig pushes the candidate
// and records the Forgejo verdict, but the local gate decides and the
// force-push still writes the target, so the record carries the pair the
// flip/no-flip call reads (slice 8).
func TestLandShadowModeRecordsBothVerdicts(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	cand := &fakeCandidate{res: CandidateResult{State: CandidatePassed, Context: "ci / gate (push)", RunStatus: "success"}}
	l.Candidate = cand
	l.Shadow = true

	res, err := l.Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	if len(cand.calls) != 1 {
		t.Fatalf("the candidate gate ran %d time(s), want once", len(cand.calls))
	}
	if got := cand.calls[0].CandidateBranch; got != "land/gt-abc" {
		t.Fatalf("candidate branch = %q, want land/gt-abc", got)
	}
	if len(f.gate.dirs) != 1 {
		t.Fatalf("the local gate ran %d time(s), want once: shadow mode lands on it", len(f.gate.dirs))
	}
	if got := f.originMain(); got != res.LandedCommit {
		t.Fatalf("origin/main = %s, want the landed %s: a shadow rig writes the target by force-push", got, res.LandedCommit)
	}
	if res.CI == nil || res.CI.State != CandidatePassed || res.CI.Context != "ci / gate (push)" {
		t.Fatalf("Result.CI = %+v; want the candidate's verdict", res.CI)
	}
	rec := shadowLandingRecord(t, f)
	if rec.CIContext != "ci / gate (push)" || rec.CIVerdict != CIVerdictSuccess || rec.CIDetail != "success" {
		t.Fatalf("record ci fields = %q/%q/%q; want the candidate's success", rec.CIContext, rec.CIVerdict, rec.CIDetail)
	}
	if rec.GateResult == "" {
		t.Fatal("record carries no local gate result beside the CI verdict")
	}
	for _, want := range []string{"ci_context: ci / gate (push)", "ci_verdict: success"} {
		if !strings.Contains(f.bead().Notes, want) {
			t.Errorf("bead note lacks %q:\n%s", want, f.bead().Notes)
		}
	}
}

// TestLandShadowModeLandsOnTheLocalGateNotCI: a red Forgejo verdict is not a
// verdict on the work in shadow mode; the local gate that passed lands it, and
// the disagreement lands in the record.
func TestLandShadowModeLandsOnTheLocalGateNotCI(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	l.Candidate = &fakeCandidate{res: CandidateResult{
		State: CandidateFailed, Context: "ci / gate (push)", RunStatus: "failure", Tail: "--- FAIL: TestThing\n"}}
	l.Shadow = true

	res, err := l.Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	if res.Gate.Passed != true {
		t.Fatalf("result gate = %+v; want the local gate's pass", res.Gate)
	}
	if got := f.originMain(); got != res.LandedCommit {
		t.Fatalf("origin/main = %s, want the landed %s", got, res.LandedCommit)
	}
	rec := shadowLandingRecord(t, f)
	if rec.CIVerdict != CIVerdictFailure || rec.CIDetail != "failure" {
		t.Fatalf("record ci fields = %q/%q; want the candidate's failure recorded beside the local pass", rec.CIVerdict, rec.CIDetail)
	}
}

// TestLandShadowModeReworksForTheLocalGate: a red local gate is the rework,
// and its reason reads as the merged tree's failure, never as the candidate
// context's.
func TestLandShadowModeReworksForTheLocalGate(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	l.Candidate = &fakeCandidate{res: CandidateResult{State: CandidatePassed, Context: "ci / gate (push)"}}
	l.Shadow = true
	f.gate.fn = func(string) GateResult {
		return GateResult{Steps: []StepResult{{Name: "test", Command: "make gate", ExitCode: 1, Tail: "local red"}}}
	}

	_, err := l.Land(context.Background(), f.work)
	rej := f.assertRejected(t, err, RejectGate, LabelRework)
	if strings.Contains(rej.Reason, "ci / gate (push)") {
		t.Fatalf("reason %q blames the candidate context for the local gate's red", rej.Reason)
	}
	if !strings.Contains(f.bead().Notes, "local red") {
		t.Fatalf("rejection note carries no local gate tail:\n%s", f.bead().Notes)
	}
}

// TestLandShadowModeSilenceIsRecordedNotFatal: CI reporting nothing is
// evidence the flip decision wants, not infrastructure the local gate's
// landing pays for.
func TestLandShadowModeSilenceIsRecordedNotFatal(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	l.Candidate = &fakeCandidate{res: CandidateResult{
		State: CandidateSilent, Context: "ci / gate (push)", Err: ErrCISilence}}
	l.Shadow = true

	res, err := l.Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	if len(f.gate.dirs) != 1 {
		t.Fatalf("the local gate ran %d time(s), want once: a silent candidate does not stop the landing", len(f.gate.dirs))
	}
	rec := shadowLandingRecord(t, f)
	if rec.CIVerdict != CIVerdictNone || !strings.Contains(rec.CIDetail, "no verdict") {
		t.Fatalf("record ci fields = %q/%q; want the silence recorded as no verdict", rec.CIVerdict, rec.CIDetail)
	}
	if got := f.originMain(); got != res.LandedCommit {
		t.Fatalf("origin/main = %s, want the landed %s", got, res.LandedCommit)
	}
}

// TestLandShadowModeNeverMergesThroughThePR: a rig that has not cut over must
// not write the target through Forgejo, whatever the two are configured with.
func TestLandShadowModeNeverMergesThroughThePR(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	l.Candidate = &fakeCandidate{res: CandidateResult{State: CandidatePassed, Context: "ci / gate (push)"}}
	l.Shadow = true
	merger := &fakeMerger{fn: func(_ Work, head string, _ Verdict) error {
		f.git.SetRef(t, f.origin, "refs/heads/main", head)
		return nil
	}}
	l.Merger = merger

	res, err := l.Land(context.Background(), f.work)
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	if len(merger.calls) != 0 {
		t.Fatalf("the PR merge ran %d time(s) on a shadow rig, want none", len(merger.calls))
	}
	if got := f.originMain(); got != res.LandedCommit {
		t.Fatalf("origin/main = %s, want the landed %s", got, res.LandedCommit)
	}
}
