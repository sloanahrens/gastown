package steward

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSpawner stands in for the agent session. block, when set, holds the job
// until the test releases it or the job's context ends.
type fakeSpawner struct {
	block   chan struct{}
	result  SpawnResult
	write   *Result
	writeTo bool

	mu    sync.Mutex
	calls []SpawnRequest
}

func (f *fakeSpawner) Spawn(ctx context.Context, req SpawnRequest) SpawnResult {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()
	if f.writeTo {
		if _, err := os.Stat(req.Dir); err != nil {
			return SpawnResult{ExitCode: -1, Err: err}
		}
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return SpawnResult{ExitCode: -1, TimedOut: true}
		}
	}
	res := f.result
	if f.write != nil {
		res.Verdict = f.write
	}
	return res
}

func (f *fakeSpawner) requests() []SpawnRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SpawnRequest(nil), f.calls...)
}

func testRunner(t *testing.T, sp Spawner, max int) *Runner {
	t.Helper()
	return &Runner{
		Ledger:  NewLedger(filepath.Join(t.TempDir(), "steward", "jobs.jsonl")),
		Spawn:   sp,
		WorkDir: filepath.Join(t.TempDir(), "jobs"),
		MaxJobs: max,
		Timeout: time.Minute,
		Now:     func() time.Time { return testEpoch },
		NewID:   func() string { return "1" },
	}
}

func reviewEvent(bead, head string) Event {
	return Event{Kind: KindReview, Rig: "gastown", Bead: bead, Branch: "polecat/emerald/" + bead, Head: head, Target: "main"}
}

// TestRunnerRecordsOneJob: the ledger gets the start row and then the
// outcome row, and the outcome is the job's own verdict.
func TestRunnerRecordsOneJob(t *testing.T) {
	t.Parallel()
	sp := &fakeSpawner{result: SpawnResult{Transcript: "/tmp/t.jsonl"}, write: &Result{Outcome: OutcomePass, Summary: "looks good"}}
	r := testRunner(t, sp, DefaultMaxJobs)
	if !r.Start(context.Background(), reviewEvent("gt-x", "aaaa"), DefaultRoutineAgent, "prompt") {
		t.Fatal("job did not start")
	}
	r.Wait()
	jobs, err := r.Ledger.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("ledger has %d rows, want a start row and an outcome row: %+v", len(jobs), jobs)
	}
	start, end := jobs[0], jobs[1]
	if !start.Ended.IsZero() || start.Outcome.Valid() {
		t.Errorf("start row is not a start row: %+v", start)
	}
	if end.ID != start.ID || end.Outcome != OutcomePass || end.Summary != "looks good" ||
		end.Transcript != "/tmp/t.jsonl" || end.Ended.IsZero() {
		t.Errorf("outcome row = %+v", end)
	}
	if len(sp.requests()) != 1 || sp.requests()[0].Prompt != "prompt" {
		t.Errorf("spawner saw %+v", sp.requests())
	}
	if got := r.Running(); len(got) != 0 {
		t.Errorf("running after Wait = %+v", got)
	}
}

// TestRunnerCapsConcurrency: the cap is the point of the runner — the third
// event waits for the next scan, it is not queued behind the others.
func TestRunnerCapsConcurrency(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	sp := &fakeSpawner{block: release, write: &Result{Outcome: OutcomePass}}
	r := testRunner(t, sp, 1)
	ctx := context.Background()
	if !r.Start(ctx, reviewEvent("gt-a", "aaaa"), DefaultRoutineAgent, "p") {
		t.Fatal("the first job did not start")
	}
	if r.Start(ctx, reviewEvent("gt-b", "bbbb"), DefaultRoutineAgent, "p") {
		t.Error("a second job started past a cap of 1")
	}
	close(release)
	r.Wait()
	if !r.Start(ctx, reviewEvent("gt-b", "bbbb"), DefaultRoutineAgent, "p") {
		t.Error("a job did not start after the cap freed")
	}
	r.Wait()
}

// TestRunnerOneJobPerBead: a bead under a job raises no second one, whatever
// head the second event names, because the fix changes the head itself.
func TestRunnerOneJobPerBead(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	sp := &fakeSpawner{block: release, write: &Result{Outcome: OutcomeFixed}}
	r := testRunner(t, sp, DefaultMaxJobs)
	ctx := context.Background()
	if !r.Start(ctx, reviewEvent("gt-a", "aaaa"), DefaultRoutineAgent, "p") {
		t.Fatal("the first job did not start")
	}
	if r.Start(ctx, reviewEvent("gt-a", "bbbb"), DefaultRoutineAgent, "p") {
		t.Error("a second job started for the same bead")
	}
	if !r.RunningBead("gt-a") {
		t.Error("the bead is not marked running")
	}
	close(release)
	r.Wait()
	if r.RunningBead("gt-a") {
		t.Error("the bead stayed marked running")
	}
}

// TestRunnerTimeout: the job's timeout reaches the spawner's context, and the
// ledger records the outcome that follows from it.
func TestRunnerTimeout(t *testing.T) {
	t.Parallel()
	sp := &fakeSpawner{block: make(chan struct{})} // never released
	r := testRunner(t, sp, DefaultMaxJobs)
	r.Timeout = 20 * time.Millisecond
	if !r.Start(context.Background(), reviewEvent("gt-a", "aaaa"), DefaultRoutineAgent, "p") {
		t.Fatal("job did not start")
	}
	r.Wait()
	jobs, err := r.Ledger.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[1].Outcome != OutcomeTimeout {
		t.Fatalf("ledger = %+v, want a timeout outcome", jobs)
	}
	if !strings.Contains(jobs[1].Summary, "20ms") {
		t.Errorf("timeout summary = %q", jobs[1].Summary)
	}
}

// TestRunnerClassify is the verdict contract: the job's own outcome when it
// writes one, an error when it writes none or writes a word the ledger does
// not know.
func TestRunnerClassify(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		res         SpawnResult
		want        Outcome
		wantSummary string
	}{
		"pass":                   {SpawnResult{Verdict: &Result{Outcome: OutcomePass, Summary: "ok"}}, OutcomePass, "ok"},
		"no verdict":             {SpawnResult{ExitCode: 0}, OutcomeError, "no verdict"},
		"a word outside the set": {SpawnResult{VerdictErr: errBadOutcome}, OutcomeError, "not one of"},
		"the job could not run":  {SpawnResult{Err: errNoWorktree}, OutcomeError, "could not run"},
		"timeout":                {SpawnResult{TimedOut: true}, OutcomeTimeout, "killed after"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, outcome, summary, _ := (&Runner{Timeout: time.Minute}).classify(tc.res, testEpoch)
			if outcome != tc.want {
				t.Fatalf("outcome = %q, want %q (%s)", outcome, tc.want, summary)
			}
			if !strings.Contains(summary, tc.wantSummary) {
				t.Errorf("summary = %q, want it to name %q", summary, tc.wantSummary)
			}
		})
	}
}

func TestReadVerdictRejectsUnknownOutcome(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ResultFile)
	if err := os.WriteFile(path, []byte(`{"outcome":"done","summary":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadVerdict(path); err == nil || !strings.Contains(err.Error(), "not one of") {
		t.Fatalf("ReadVerdict on an unknown outcome = %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"outcome":"pass","summary":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadVerdict(path)
	if err != nil || got.Outcome != OutcomePass || got.Summary != "x" {
		t.Fatalf("ReadVerdict = %+v, %v", got, err)
	}
}

// TestChooseModel is the retry rule: routine once, hard once more, then stop
// — the escalation instead of a third job is the operator's (gt-9bioi).
func TestChooseModel(t *testing.T) {
	t.Parallel()
	routine, hard := "deepseek-flash", "claude-opus"
	failed := Job{Outcome: OutcomeFail, Model: routine}
	passed := Job{Outcome: OutcomePass, Model: routine}
	hardFailed := Job{Outcome: OutcomeFail, Model: hard}
	timeout := Job{Outcome: OutcomeTimeout, Model: routine}

	model, run := ChooseModel(nil, routine, hard)
	if !run || model != routine {
		t.Errorf("no history: %q %v, want %q true", model, run, routine)
	}
	if model, run = ChooseModel([]Job{failed}, routine, hard); !run || model != hard {
		t.Errorf("one routine failure: %q %v, want %q true", model, run, hard)
	}
	if _, run = ChooseModel([]Job{passed}, routine, hard); run {
		t.Error("a passed job earned another run")
	}
	if _, run = ChooseModel([]Job{failed, hardFailed}, routine, hard); run {
		t.Error("a second failure earned a third job")
	}
	if model, run = ChooseModel([]Job{timeout}, routine, hard); !run || model != hard {
		t.Errorf("a timeout: %q %v, want %q true", model, run, hard)
	}
}

// TestStartedModelSendsConflictsToTheHardPreset: a conflict touches code that
// moved, which the routine model is not trusted with (gt-9bioi).
func TestStartedModelSendsConflictsToTheHardPreset(t *testing.T) {
	t.Parallel()
	conflict := Event{Kind: KindRejection, Bead: "gt-x", Head: "aaaa", RejectionDetail: "kind=conflict reason=main moved"}
	gate := Event{Kind: KindRejection, Bead: "gt-x", Head: "aaaa", RejectionDetail: "kind=gate reason=make test exit 2"}
	if model, run := StartedModel(conflict, nil, "flash", "opus"); !run || model != "opus" {
		t.Errorf("conflict: %q %v, want opus true", model, run)
	}
	if model, run := StartedModel(gate, nil, "flash", "opus"); !run || model != "flash" {
		t.Errorf("gate rejection: %q %v, want flash true", model, run)
	}
}

// TestRunnerPromptNamesTheVerdictFile: the contract the job is held to is in
// the prompt the runner sends, not only in the runner's reader.
func TestRunnerPromptNamesTheVerdictFile(t *testing.T) {
	t.Parallel()
	prompt := PromptFor(reviewEvent("gt-x", "c0ffee"))
	for _, want := range []string{ResultFile, "gt-x", "c0ffee", string(OutcomePass), string(OutcomeEscalated)} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	rejection := PromptFor(Event{Kind: KindRejection, Rig: "gastown", Bead: "gt-x", Branch: "b", Head: "c0ffee", Target: "main", Attempt: 2, RejectionDetail: "kind=gate reason=x"})
	if !strings.Contains(rejection, "attempt 2") || !strings.Contains(rejection, "kind=gate") {
		t.Errorf("rejection prompt lacks the refusal:\n%s", rejection)
	}
}

// errBadOutcome is the error ReadVerdict produces for a word outside the set,
// so the classification test reads a real message rather than a stand-in.
var errBadOutcome = fmt.Errorf("%s reports outcome %q, which is not one of %s", ResultFile, "done", outcomeList())

var errNoWorktree = errors.New("worktree at c0ffee: no such commit")
