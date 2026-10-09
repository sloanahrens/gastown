package land

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	// fullLog answers a fetch wider than the tail cap; empty means the server
	// returns log whatever the cap.
	fullLog string
	// limits is the maxBytes of every log fetch, in order: what the gate asked
	// the server for.
	limits []int64
	// deleted is the branch of every DeleteBranch call, in order.
	deleted   []string
	deleteErr error
	err       error
	logErr    error
	// statusErrs is the error of the first len(statusErrs) status reads, in
	// order; reads past it fall back to err. It stands in for a Forgejo that
	// blips a few times before it answers (gt-394h5).
	statusErrs []error
}

func (f *fakeForgejo) CombinedStatus(context.Context, string, string, string) (*forgejo.CombinedStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.polls++
	if len(f.statusErrs) > 0 {
		err := f.statusErrs[0]
		f.statusErrs = f.statusErrs[1:]
		return nil, err
	}
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

func (f *fakeForgejo) JobLogsTail(_ context.Context, _, _ string, _ int64, maxBytes int64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.limits = append(f.limits, maxBytes)
	if maxBytes > candidateTailBytes && f.fullLog != "" {
		return f.fullLog, f.logErr
	}
	return f.log, f.logErr
}

func (f *fakeForgejo) DeleteBranch(_ context.Context, _, _, branch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, branch)
	return f.deleteErr
}

func status(state forgejo.CommitState, context string) forgejo.CommitStatus {
	return forgejo.CommitStatus{Status: state, Context: context}
}

// gate polls fast, so a test's wait window is milliseconds rather than the
// production interval. The window leaves room for the status-retry tolerance's
// few reads even when parallel tests load the scheduler: a window that only
// fits one read turns every blip test into a race with the silence window.
func fastGate(client CandidateStatus) *CandidateGate {
	return &CandidateGate{
		Client: client, Owner: "gastown", RepoName: "gastown", Workflow: "gate",
		PollInterval: time.Millisecond, CallTimeout: time.Second, WaitTimeout: 200 * time.Millisecond,
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

// TestGastownGateWorkflowRunsTheMergeQueueGate holds the workflow to the
// targets the rig's merge_queue.gate names, so the local gate and the CI job
// cannot drift into testing different things (design: "the same targets").
// lint-tools runs first because the runner image carries no lint tools: without
// it the gate's lint stage is red with "golangci-lint missing" (gt-fn9e6.17).
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
	if want := []string{"make lint-tools", "make gate"}; !slices.Equal(runs, want) {
		t.Fatalf("gate job runs %q, want exactly %q in that order — the lint tools, then gastown's merge_queue.gate", runs, want)
	}
	wf, err := ParseGateWorkflow(data)
	if err != nil {
		t.Fatalf("this repo's gate workflow: %v", err)
	}
	if got := wf.Context(); got != "ci / gate (push)" {
		t.Fatalf("Context() = %q, want %q", got, "ci / gate (push)")
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
// and the rework note carries the failing job's log. The log carries the step
// marker a started job prints, so no infrastructure signature claims it.
func TestCandidateGateRedCarriesTheJobLogTail(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	sha := f.git.Commit(t, f.repo, "main", "candidate", map[string]string{"c.txt": "x\n"})
	client := &fakeForgejo{
		statuses: []forgejo.CommitStatus{status(forgejo.StateFailure, "ci / gate (push)")},
		runs:     []forgejo.ActionRun{{ID: 7, CommitSHA: sha, Status: "failure"}},
		jobs:     []forgejo.ActionRunJob{{ID: 9, RunID: 7, Name: "gate", Status: "failure"}},
		log:      gateJobLog("⭐ Run Main actions/checkout@v4", "make gate", "--- FAIL: TestThing"),
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
	if len(client.limits) != 1 || client.limits[0] != candidateTailBytes {
		t.Fatalf("log fetches %v; a tail that names the failure needs no second fetch", client.limits)
	}
}

// TestCandidateGateDiscardDeletesTheBranchTheRunPushed: a landing that ends red
// or infra leaves no land/<bead> behind — the gate deletes the branch its own
// run pushed (gt-k796q).
func TestCandidateGateDiscardDeletesTheBranchTheRunPushed(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	sha := f.git.Commit(t, f.repo, "main", "candidate", map[string]string{"c.txt": "x\n"})
	client := &fakeForgejo{
		statuses: []forgejo.CommitStatus{status(forgejo.StateFailure, "ci / gate (push)")},
		runs:     []forgejo.ActionRun{{ID: 7, CommitSHA: sha, Status: "failure"}},
		jobs:     []forgejo.ActionRunJob{{ID: 9, RunID: 7, Name: "gate", Status: "failure"}},
		log:      gateJobLog("⭐ Run Main actions/checkout@v4", "make gate", "--- FAIL: TestThing"),
	}
	gate := fastGate(client)
	res := gate.Run(context.Background(), f.git.Open(f.repo), writeGateWorkflow(t, gateWorkflowYAML), f.work, sha)
	if res.State != CandidateFailed || !res.Pushed {
		t.Fatalf("result %+v; want a failed verdict on a branch the run pushed", res)
	}
	gate.Discard(context.Background(), f.work, res)
	if !slices.Equal(client.deleted, []string{"land/gt-abc"}) {
		t.Fatalf("deleted %v; want the candidate branch the run pushed", client.deleted)
	}
}

// TestCandidateGateDiscardLeavesABranchTheRunDidNotPush: a run that never got
// the candidate onto the remote — here the gate workflow is not in the tree it
// reads — deletes nothing, because a branch already there is some other
// landing's (gt-k796q).
func TestCandidateGateDiscardLeavesABranchTheRunDidNotPush(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	sha := f.git.Commit(t, f.repo, "main", "candidate", map[string]string{"c.txt": "x\n"})
	client := &fakeForgejo{}
	gate := fastGate(client)
	res := gate.Run(context.Background(), f.git.Open(f.repo), t.TempDir(), f.work, sha)
	if res.Err == nil || res.Pushed {
		t.Fatalf("result %+v; want a run that failed before the push", res)
	}
	gate.Discard(context.Background(), f.work, res)
	if len(client.deleted) != 0 {
		t.Fatalf("deleted %v; want a branch the run did not push left alone", client.deleted)
	}
}

// TestCandidateGateDiscardLogsAFailedDelete: the delete is best-effort. A
// failure is logged and nothing else changes — the landing's outcome is
// already decided when it runs (gt-k796q).
func TestCandidateGateDiscardLogsAFailedDelete(t *testing.T) {
	t.Parallel()
	client := &fakeForgejo{deleteErr: errors.New("forgejo: DELETE returned 500")}
	gate := fastGate(client)
	var out strings.Builder
	gate.Out = &out

	gate.Discard(context.Background(), Work{BeadID: "gt-abc"}, CandidateResult{Branch: "land/gt-abc", Pushed: true})

	if !slices.Equal(client.deleted, []string{"land/gt-abc"}) {
		t.Fatalf("deleted %v; want the delete attempted", client.deleted)
	}
	if !strings.Contains(out.String(), "land/gt-abc") {
		t.Fatalf("output %q; want the failed delete logged with the branch", out.String())
	}
}

// TestCandidateGateFetchesTheWholeLogWhenTheTailHidesTheFailure: the failure
// goes in the note even when it sits far enough from the end that the byte cap
// cuts it out. go test prints its packages in order, so a 16 KiB tail is
// passing packages and the note would name no failure at all (gt-fn9e6.25).
func TestCandidateGateFetchesTheWholeLogWhenTheTailHidesTheFailure(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		fullLog string
		want    string
	}{
		{
			name:    "the whole log names the failure",
			fullLog: gateJobLog("⭐ Run Main actions/checkout@v4", "--- FAIL: TestRunSync_KillsDescendants (0.03s)", "FAIL\tgithub.com/x/hooks\t0.4s", "ok  \tgithub.com/x/zzz\t0.1s"),
			want:    "TestRunSync_KillsDescendants",
		},
		{
			name:    "a whole log that names none leaves the tail standing",
			fullLog: gateJobLog("⭐ Run Main actions/checkout@v4", "ok  \tgithub.com/x/zzz\t0.1s"),
			want:    "ok  \tgithub.com/x/zzz\t0.1s",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newLandFixture(t)
			sha := f.git.Commit(t, f.repo, "main", "candidate", map[string]string{"c.txt": "x\n"})
			client := &fakeForgejo{
				statuses: []forgejo.CommitStatus{status(forgejo.StateFailure, "ci / gate (push)")},
				runs:     []forgejo.ActionRun{{ID: 7, CommitSHA: sha, Status: "failure"}},
				jobs:     []forgejo.ActionRunJob{{ID: 9, RunID: 7, Name: "gate", Status: "failure"}},
				log:      gateJobLog("⭐ Run Main actions/checkout@v4", "ok  \tgithub.com/x/zzz\t0.1s"),
				fullLog:  tt.fullLog,
			}
			res := fastGate(client).Run(context.Background(), f.git.Open(f.repo), writeGateWorkflow(t, gateWorkflowYAML), f.work, sha)
			if res.State != CandidateFailed {
				t.Fatalf("state = %v, want failed (err %v)", res.State, res.Err)
			}
			if !strings.Contains(res.Tail, tt.want) {
				t.Fatalf("tail %q; want it to carry %q", res.Tail, tt.want)
			}
			if len(client.limits) != 2 || client.limits[0] != candidateTailBytes || client.limits[1] != candidateFullLogBytes {
				t.Fatalf("log fetches %v; want the capped tail then the whole log", client.limits)
			}
		})
	}
}

// gateJobLogLine timestamps a line the way Forgejo prefixes every line of an
// action job log, the noise the note's excerpt strips.
func gateJobLogLine(line string) string {
	return "2026-10-04T23:49:26.0641253Z " + line
}

// gateJobLog stamps lines and joins them into a job log body.
func gateJobLog(lines ...string) string {
	stamped := make([]string, len(lines))
	for i, line := range lines {
		stamped[i] = gateJobLogLine(line)
	}
	return strings.Join(stamped, "\n") + "\n"
}

// TestFailureExcerptShowsTheFailureNotTheEndOfTheLog is the be-bl8 trial
// landing: the red was TestRunSync_KillsDescendants in internal/hooks, but the
// note carried only the passing packages go test printed after it, so the
// polecat reading it could not see what failed (gt-fn9e6.25).
func TestFailureExcerptShowsTheFailureNotTheEndOfTheLog(t *testing.T) {
	t.Parallel()
	log := gateJobLog(
		"ok  \tgithub.com/x/aaa\t0.1s",
		"=== RUN   TestRunSync_KillsDescendants",
		"    sync_test.go:42: want 3 descendants, got 1",
		"--- FAIL: TestRunSync_KillsDescendants (0.03s)",
		"FAIL\tgithub.com/x/hooks\t0.4s",
		"ok  \tgithub.com/x/bbb\t0.2s",
		"ok  \tgithub.com/x/ccc\t0.3s",
		"ok  \tgithub.com/x/ddd\t0.2s",
		"ok  \tgithub.com/x/eee\t0.1s",
		"ok  \tgithub.com/x/fff\t0.2s",
		"ok  \tgithub.com/x/ggg\t0.4s",
		"make: *** [Makefile:290: gate] Error 1",
	)
	got := failureExcerpt(log, gateTailLines)
	for _, want := range []string{
		"TestRunSync_KillsDescendants",
		"sync_test.go:42: want 3 descendants, got 1",
		"make: *** [Makefile:290: gate] Error 1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("excerpt does not carry %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "2026-10-04T") {
		t.Errorf("excerpt keeps the Forgejo timestamps:\n%s", got)
	}
	if strings.Contains(got, "github.com/x/ddd") {
		t.Errorf("excerpt keeps passing packages that explain nothing:\n%s", got)
	}
	if n := strings.Count(got, "\n"); n > gateTailLines {
		t.Errorf("excerpt is %d lines, over the %d-line budget:\n%s", n, gateTailLines, got)
	}
	if strings.Index(got, "TestRunSync") > strings.Index(got, "make: ***") {
		t.Errorf("excerpt is not in log order:\n%s", got)
	}
}

// TestFailureExcerptCarriesEveryShapeOfRed: each marker a gate log can go red
// on keeps the output that explains it (gt-fn9e6.25).
func TestFailureExcerptCarriesEveryShapeOfRed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		log  string
		want []string
	}{
		{
			name: "a go test failure prints its output before the --- FAIL line",
			log:  gateJobLog("=== RUN   TestAlpha", "    a_test.go:9: want 2, got 3", "--- FAIL: TestAlpha (0.00s)", "FAIL\tgithub.com/x/a\t0.1s"),
			want: []string{"TestAlpha", "a_test.go:9: want 2, got 3", "FAIL\tgithub.com/x/a"},
		},
		{
			name: "a --- FAIL block keeps the output printed under it",
			log:  gateJobLog("--- FAIL: TestBeta (0.01s)", "    b_test.go:5: first", "    b_test.go:6: second", "FAIL\tgithub.com/x/b\t0.1s"),
			want: []string{"TestBeta", "b_test.go:5: first", "b_test.go:6: second"},
		},
		{
			name: "a build failure keeps the compiler errors",
			log:  gateJobLog("# github.com/x/c", "./c.go:3:2: undefined: zzz", "FAIL\tgithub.com/x/c [build failed]", "make: *** [Makefile:290: gate] Error 1"),
			want: []string{"[build failed]", "undefined: zzz", "make: *** [Makefile:290: gate] Error 1"},
		},
		{
			name: "a panic, which prints no --- FAIL line of its own",
			log:  gateJobLog("=== RUN   TestGamma", "panic: runtime error: index out of range [3] with length 2", "goroutine 12 [running]:", "FAIL\tgithub.com/x/d\t0.2s"),
			want: []string{"panic: runtime error: index out of range [3] with length 2", "goroutine 12 [running]:", "FAIL\tgithub.com/x/d"},
		},
		{
			name: "a make error with no test output at all",
			log:  gateJobLog("go build ./...", "make: *** [Makefile:106: build] Error 1"),
			want: []string{"go build ./...", "make: *** [Makefile:106: build] Error 1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := failureExcerpt(tt.log, gateTailLines)
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("excerpt does not carry %q:\n%s", want, got)
				}
			}
			if strings.Contains(got, "2026-10-04T") {
				t.Errorf("excerpt keeps the Forgejo timestamps:\n%s", got)
			}
			if n := strings.Count(got, "\n"); n > gateTailLines {
				t.Errorf("excerpt is %d lines, over the %d-line budget:\n%s", n, gateTailLines, got)
			}
		})
	}
}

// TestFailureExcerptWithoutAMarkerIsTheTail: a log that names no failure --
// the gate dying before it printed one -- still yields the last lines, and
// that fallback is bounded too.
func TestFailureExcerptWithoutAMarkerIsTheTail(t *testing.T) {
	t.Parallel()
	lines := make([]string, 0, 3*gateTailLines)
	for i := range 3 * gateTailLines {
		lines = append(lines, fmt.Sprintf("ok  \tgithub.com/x/p%03d\t0.1s", i))
	}
	got := failureExcerpt(gateJobLog(lines...), gateTailLines)
	if n := strings.Count(got, "\n"); n != gateTailLines {
		t.Errorf("excerpt is %d lines, want the %d-line tail:\n%s", n, gateTailLines, got)
	}
	if !strings.Contains(got, "github.com/x/p119") {
		t.Errorf("excerpt does not end at the log's last line:\n%s", got)
	}
	if strings.Contains(got, "github.com/x/p079") || strings.Contains(got, "2026-10-04T") {
		t.Errorf("excerpt is not the last %d stripped lines:\n%s", gateTailLines, got)
	}
}

// TestFailureExcerptStaysInBudget: a log with more failures than the budget
// holds keeps the earliest ones whole rather than every failure's name.
func TestFailureExcerptStaysInBudget(t *testing.T) {
	t.Parallel()
	var lines []string
	for i := range 2 * gateTailLines {
		lines = append(lines,
			fmt.Sprintf("--- FAIL: TestMany%d (0.00s)", i),
			fmt.Sprintf("    many_test.go:1: boom %d", i),
			fmt.Sprintf("    many_test.go:2: more %d", i),
			"FAIL\tgithub.com/x/many\t0.5s")
	}
	got := failureExcerpt(gateJobLog(lines...), gateTailLines)
	if n := strings.Count(got, "\n"); n > gateTailLines {
		t.Errorf("excerpt is %d lines, over the %d-line budget:\n%s", n, gateTailLines, got)
	}
	for _, want := range []string{"TestMany0", "many_test.go:1: boom 0", "many_test.go:2: more 0"} {
		if !strings.Contains(got, want) {
			t.Errorf("excerpt dropped the earliest failure's %q:\n%s", want, got)
		}
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

// TestCandidateGateWaitIs20Minutes: the CI wait is 20 minutes, a gate with no
// wait of its own takes it, and a context that never reports inside the window
// is ErrCISilence — the infrastructure outcome — never a verdict on the work
// (gt-fn9e6.26). The fake client and a millisecond-scale window stand in for
// the 20 minutes the constant names.
func TestCandidateGateWaitIs20Minutes(t *testing.T) {
	t.Parallel()
	if DefaultCandidateWaitTimeout != 20*time.Minute {
		t.Fatalf("DefaultCandidateWaitTimeout = %s, want 20m", DefaultCandidateWaitTimeout)
	}
	if got := (&CandidateGate{}).waitTimeout(); got != DefaultCandidateWaitTimeout {
		t.Fatalf("waitTimeout() = %s, want the default %s", got, DefaultCandidateWaitTimeout)
	}
	f := newLandFixture(t)
	sha := f.git.Commit(t, f.repo, "main", "candidate", map[string]string{"c.txt": "x\n"})
	// No statuses at all: the fake reports nothing however long the gate polls.
	client := &fakeForgejo{}
	res := fastGate(client).Run(context.Background(), f.git.Open(f.repo), writeGateWorkflow(t, gateWorkflowYAML), f.work, sha)
	if res.State != CandidateSilent {
		t.Fatalf("state = %v, want the no-verdict state", res.State)
	}
	if !errors.Is(res.Err, ErrCISilence) {
		t.Fatalf("err = %v, want one wrapping ErrCISilence: a wait that outlives the window is infrastructure", res.Err)
	}
}

// TestCandidateGateAPIFailureIsNotAVerdict: a Forgejo that answers with an
// error on every read exhausts the tolerance and fails the landing into the
// infra path rather than stalling it.
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

// TestCandidateGateAbsorbsTransientStatusErrors: a status read that comes back
// an error is not a verdict. Two blips in a row, then the required context
// reporting success, must still take the verdict without touching the
// candidate (gt-394h5).
func TestCandidateGateAbsorbsTransientStatusErrors(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	sha := f.git.Commit(t, f.repo, "main", "candidate", map[string]string{"c.txt": "x\n"})
	client := &fakeForgejo{
		statusErrs: []error{errors.New("forgejo: GET statuses returned 502"), errors.New("context deadline exceeded")},
		statuses:   []forgejo.CommitStatus{status(forgejo.StateSuccess, "ci / gate (push)")},
	}
	gate := fastGate(client)
	var log strings.Builder
	gate.Out = &log

	res := gate.Run(context.Background(), f.git.Open(f.repo), writeGateWorkflow(t, gateWorkflowYAML), f.work, sha)
	if res.Err != nil {
		t.Fatalf("Run: %v; two blips must not end the wait", res.Err)
	}
	if res.State != CandidatePassed {
		t.Fatalf("state = %v, want passed: the success after the blips is the verdict", res.State)
	}
	if got := f.git.Ref(f.origin, "refs/heads/land/gt-abc"); got != sha {
		t.Fatalf("origin land/gt-abc = %s, want the pushed %s: no blip discards the candidate", got, sha)
	}
	if client.polls != 3 {
		t.Fatalf("polled %d time(s), want the two blips and the success", client.polls)
	}
	for _, want := range []string{"502", "context deadline exceeded"} {
		if !strings.Contains(log.String(), want) {
			t.Fatalf("log %q; want every tolerated read error logged (%q)", log.String(), want)
		}
	}
}

// TestCandidateGateNonConsecutiveBlipsDoNotAddUp: a good read clears the blip
// count, so errors separated by reads that report nothing do not add up to a
// give-up — they are a long wait, not a broken Forgejo.
func TestCandidateGateNonConsecutiveBlipsDoNotAddUp(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	sha := f.git.Commit(t, f.repo, "main", "candidate", map[string]string{"c.txt": "x\n"})
	// A blip on every other read, and the last read reports success. Were the
	// count cumulative rather than consecutive, the fourth blip would have
	// ended the wait.
	client := &fakeForgejo{statuses: []forgejo.CommitStatus{
		status(forgejo.StatePending, "ci / gate (push)"),
		status(forgejo.StatePending, "ci / gate (push)"),
		status(forgejo.StatePending, "ci / gate (push)"),
		status(forgejo.StateSuccess, "ci / gate (push)"),
	}}
	gate := fastGate(&blipAfterReads{CandidateStatus: client, blipAt: map[int]error{
		2: errors.New("forgejo: GET statuses returned 502"),
		4: errors.New("connection reset by peer"),
		6: errors.New("context deadline exceeded"),
		8: errors.New("forgejo: GET statuses returned 502"),
	}})
	// Four blips separated by four pending reads take nine polls to play out;
	// the window is wide enough that the interleaving, not the silence window,
	// decides the outcome.
	gate.WaitTimeout = 2 * time.Second

	res := gate.Run(context.Background(), f.git.Open(f.repo), writeGateWorkflow(t, gateWorkflowYAML), f.work, sha)
	if res.Err != nil {
		t.Fatalf("Run: %v; non-consecutive blips must not add up", res.Err)
	}
	if res.State != CandidatePassed {
		t.Fatalf("state = %v, want passed", res.State)
	}
}

// blipAfterReads fails the nth read (1-based) with the mapped error and
// delegates every other read to the status source it embeds. It lets a test
// interleave errors and pending reads, which fakeForgejo's single status
// sequence cannot.
type blipAfterReads struct {
	CandidateStatus
	blipAt map[int]error
	reads  int
}

func (b *blipAfterReads) CombinedStatus(ctx context.Context, owner, repo, ref string) (*forgejo.CombinedStatus, error) {
	b.reads++
	if err, ok := b.blipAt[b.reads]; ok {
		return nil, err
	}
	return b.CandidateStatus.CombinedStatus(ctx, owner, repo, ref)
}

// TestCandidateGateGivesUpAfterTheTolerance: errors past the tolerance are not
// a wait that never ends; the landing takes the infra path with the last
// error.
func TestCandidateGateGivesUpAfterTheTolerance(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	sha := f.git.Commit(t, f.repo, "main", "candidate", map[string]string{"c.txt": "x\n"})
	blip := errors.New("forgejo: GET statuses returned 502")
	client := &fakeForgejo{
		statusErrs: []error{blip, blip, blip, blip, blip},
		statuses:   []forgejo.CommitStatus{status(forgejo.StateSuccess, "ci / gate (push)")},
	}
	res := fastGate(client).Run(context.Background(), f.git.Open(f.repo), writeGateWorkflow(t, gateWorkflowYAML), f.work, sha)
	if res.Err == nil {
		t.Fatal("errors past the tolerance must end the wait")
	}
	if !errors.Is(res.Err, blip) {
		t.Fatalf("err = %v, want the last status read error", res.Err)
	}
	if !strings.Contains(res.Err.Error(), "consecutive") {
		t.Fatalf("err = %q; want it to say the reads failed in a row", res.Err)
	}
	if res.State != CandidateSilent {
		t.Fatalf("state = %v, want no verdict: a blip is never a verdict on the work", res.State)
	}
	if errors.Is(res.Err, ErrCISilence) {
		t.Fatalf("err = %v; giving up on the reads is not the wait reporting nothing", res.Err)
	}
	if client.polls != candidateStatusErrorTolerance+1 {
		t.Fatalf("polled %d time(s), want the tolerance plus the read that gives up", client.polls)
	}
}

// TestCandidateGateFailedStatusEndsTheWaitImmediately: a status that reported a
// failure is a verdict, not a blip. It is not retried, even with blips left in
// the tolerance.
func TestCandidateGateFailedStatusEndsTheWaitImmediately(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	sha := f.git.Commit(t, f.repo, "main", "candidate", map[string]string{"c.txt": "x\n"})
	client := &fakeForgejo{
		statusErrs: []error{errors.New("forgejo: GET statuses returned 502")},
		statuses:   []forgejo.CommitStatus{status(forgejo.StateFailure, "ci / gate (push)")},
		runs:       []forgejo.ActionRun{{ID: 7, CommitSHA: sha, Status: "failure"}},
		jobs:       []forgejo.ActionRunJob{{ID: 9, RunID: 7, Name: "gate", Status: "failure"}},
		log:        gateJobLog("⭐ Run Main actions/checkout@v4", "make gate", "--- FAIL: TestThing"),
	}
	res := fastGate(client).Run(context.Background(), f.git.Open(f.repo), writeGateWorkflow(t, gateWorkflowYAML), f.work, sha)
	if res.Err != nil {
		t.Fatalf("Run: %v", res.Err)
	}
	if res.State != CandidateFailed {
		t.Fatalf("state = %v, want failed: a reported failure is the verdict", res.State)
	}
	if client.polls != 2 {
		t.Fatalf("polled %d time(s); want the blip then the red read that ends the wait", client.polls)
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
	// discarded is the branch of every Discard call Land made.
	discarded []string
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

func (c *fakeCandidate) Discard(_ context.Context, _ Work, res CandidateResult) {
	c.discarded = append(c.discarded, res.Branch)
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
	if len(cand.discarded) != 0 {
		t.Fatalf("discarded %v; a merged candidate's branch is the merger's to delete", cand.discarded)
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
// a gate rework the polecat resumes from, carrying the job log, and the
// candidate branch it pushed is deleted: the landing ended without merging it
// (gt-k796q).
func TestLandCandidateRedIsReworkWithTheLogTail(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	cand := &fakeCandidate{res: CandidateResult{
		State: CandidateFailed, Context: "ci / gate (push)", Tail: "--- FAIL: TestThing\n", Pushed: true}}
	l.Candidate = cand

	_, err := l.Land(context.Background(), f.work)
	rej := f.assertRejected(t, err, RejectGate, LabelRework)
	if !strings.Contains(rej.Reason, "ci / gate (push)") {
		t.Fatalf("reason %q; want it to name the failing context", rej.Reason)
	}
	if !strings.Contains(f.bead().Notes, "--- FAIL: TestThing") {
		t.Fatalf("the rejection note carries no job log tail:\n%s", f.bead().Notes)
	}
	if !slices.Equal(cand.discarded, []string{"land/gt-abc"}) {
		t.Fatalf("discarded %v; want the branch the red gate pushed", cand.discarded)
	}
}

// TestLandReportsTheCIGateFailure: a red candidate verdict is handed to the
// CI-failure watch as the gate reads it — the bead, the job log tail, and the
// tests parsed out of the tail — and a green verdict reports nothing
// (gt-xvw20).
func TestLandReportsTheCIGateFailure(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	tail := "=== RUN   TestThing\n--- FAIL: TestThing (0.00s)\nFAIL\nFAIL\tgithub.com/x/a\t0.1s\n"
	cand := &fakeCandidate{res: CandidateResult{
		State: CandidateFailed, Context: "ci / gate (push)", Tail: tail, Pushed: true}}
	l.Candidate = cand
	var got []CIFailure
	l.CIFailure = func(f CIFailure) { got = append(got, f) }

	_, err := l.Land(context.Background(), f.work)
	f.assertRejected(t, err, RejectGate, LabelRework)
	if len(got) != 1 {
		t.Fatalf("CIFailure called %d time(s), want once", len(got))
	}
	if got[0].Bead != "gt-abc" || got[0].Tail != tail {
		t.Errorf("CIFailure = %+v, want the bead and the job log tail", got[0])
	}
	if want := []TestFailure{{Package: "github.com/x/a", Test: "TestThing"}}; !slices.Equal(got[0].Tests, want) {
		t.Errorf("CIFailure tests = %v, want %v", got[0].Tests, want)
	}
}

// TestLandReportsNoCIGateFailureForAGreenCandidate: only a red verdict is a
// failure to watch; a green one reports nothing (gt-xvw20).
func TestLandReportsNoCIGateFailureForAGreenCandidate(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	cand := &fakeCandidate{res: CandidateResult{State: CandidatePassed, Context: "ci / gate (push)"}}
	l.Candidate = cand
	var calls int
	l.CIFailure = func(CIFailure) { calls++ }

	if _, err := l.Land(context.Background(), f.work); err != nil {
		t.Fatalf("Land: %v", err)
	}
	if calls != 0 {
		t.Errorf("CIFailure called %d time(s) on a green candidate, want none", calls)
	}
}

// TestLandCandidateSilenceIsInfrastructure: CI reporting nothing is the infra
// retry, not a rejection the polecat would be sent back to fix — and the
// candidate branch it pushed goes with the infra outcome (gt-k796q).
func TestLandCandidateSilenceIsInfrastructure(t *testing.T) {
	t.Parallel()
	f := newLandFixture(t)
	l := f.lander()
	cand := &fakeCandidate{res: CandidateResult{
		State: CandidateSilent, Context: "ci / gate (push)", Err: ErrCISilence, Pushed: true}}
	l.Candidate = cand

	_, err := l.Land(context.Background(), f.work)
	var infra *InfraError
	if !errors.As(err, &infra) || !errors.Is(err, ErrCISilence) {
		t.Fatalf("Land error = %T %v, want an *InfraError wrapping ErrCISilence", err, err)
	}
	if infra.Stage != StageCI {
		t.Fatalf("stage = %q, want %q", infra.Stage, StageCI)
	}
	if !slices.Equal(cand.discarded, []string{"land/gt-abc"}) {
		t.Fatalf("discarded %v; want the branch the silent gate pushed", cand.discarded)
	}
	f.assertUntouched(t)
}
