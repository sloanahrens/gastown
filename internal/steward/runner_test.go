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
	routine, hard := "deepseek-flash", "deepseek-pro"
	failed := Job{ID: "1", Outcome: OutcomeFail, Model: routine}
	passed := Job{ID: "1", Outcome: OutcomePass, Model: routine}
	hardFailed := Job{ID: "2", Outcome: OutcomeFail, Model: hard}
	timeout := Job{ID: "1", Outcome: OutcomeTimeout, Model: routine}

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
	// The ledger holds a start row and an end row per job: one job, not two.
	started := Job{ID: "1", Model: routine}
	if model, run = ChooseModel([]Job{started, failed}, routine, hard); !run || model != hard {
		t.Errorf("start+end rows of one failed job: %q %v, want %q true", model, run, hard)
	}
	if _, run = ChooseModel([]Job{started}, routine, hard); run {
		t.Error("a running job earned another run")
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

// errBadOutcome is the error ReadVerdict produces for a word outside the set,
// so the classification test reads a real message rather than a stand-in.
var errBadOutcome = fmt.Errorf("%s reports outcome %q, which is not one of %s", ResultFile, "done", outcomeList())

var errNoWorktree = errors.New("worktree at c0ffee: no such commit")

// shutdownSpawner is a job the daemon's shutdown kills: it runs until its
// context ends and reports the kill the way AgentSpawner does, as a run error
// rather than a timeout.
type shutdownSpawner struct{ started chan struct{} }

func (s shutdownSpawner) Spawn(ctx context.Context, _ SpawnRequest) SpawnResult {
	close(s.started)
	<-ctx.Done()
	return SpawnResult{ExitCode: -1, Err: errors.New("signal: killed"), VerdictErr: errors.New("no verdict")}
}

// TestRunnerShutdownRecordsInterrupted: a job the daemon's shutdown kills is
// not a failed attempt. Recorded as an error it would spend the routine run,
// and at merge-cadence restarts most events would end with no real attempt
// (gt-9bioi.5).
func TestRunnerShutdownRecordsInterrupted(t *testing.T) {
	t.Parallel()
	sp := shutdownSpawner{started: make(chan struct{})}
	r := testRunner(t, sp, DefaultMaxJobs)
	ctx, cancel := context.WithCancel(context.Background())
	if !r.Start(ctx, reviewEvent("gt-x", "aaaa"), DefaultRoutineAgent, "p") {
		t.Fatal("job did not start")
	}
	<-sp.started
	cancel()
	r.Wait()
	jobs, err := r.Ledger.Read()
	if err != nil {
		t.Fatal(err)
	}
	end := jobs[len(jobs)-1]
	if end.Outcome != OutcomeInterrupted || end.Ended.IsZero() {
		t.Fatalf("outcome row = %+v, want interrupted", end)
	}
	if end.Outcome.Failed() {
		t.Error("an interrupted job reads as a failure")
	}
}

// TestRunnerTimeoutIsNotInterrupted: the job's own timeout still records a
// timeout while the daemon is up.
func TestRunnerTimeoutIsNotInterrupted(t *testing.T) {
	t.Parallel()
	sp := &fakeSpawner{block: make(chan struct{})}
	r := testRunner(t, sp, DefaultMaxJobs)
	r.Timeout = 20 * time.Millisecond
	if !r.Start(context.Background(), reviewEvent("gt-x", "aaaa"), DefaultRoutineAgent, "p") {
		t.Fatal("job did not start")
	}
	r.Wait()
	jobs, _ := r.Ledger.Read()
	if got := jobs[len(jobs)-1].Outcome; got != OutcomeTimeout {
		t.Fatalf("outcome = %q, want timeout", got)
	}
}

// TestChooseModelIgnoresInterruptedJobs: restarts are not attempts. A routine
// job interrupted twice still gets its routine run, and the hard retry after
// a real failure (gt-9bioi.5).
func TestChooseModelIgnoresInterruptedJobs(t *testing.T) {
	t.Parallel()
	routine, hard := "deepseek-flash", "deepseek-pro"
	cut := func(id string) []Job {
		return []Job{{ID: id, Model: routine}, {ID: id, Model: routine, Outcome: OutcomeInterrupted}}
	}
	history := append(cut("1"), cut("2")...)
	if model, run := ChooseModel(history, routine, hard); !run || model != routine {
		t.Fatalf("two interrupted jobs: %q %v, want %q true", model, run, routine)
	}
	history = append(history, Job{ID: "3", Model: routine, Outcome: OutcomeFail})
	if model, run := ChooseModel(history, routine, hard); !run || model != hard {
		t.Fatalf("interrupted twice then failed: %q %v, want the hard retry %q", model, run, hard)
	}
	history = append(history, Job{ID: "4", Model: hard}, Job{ID: "4", Model: hard, Outcome: OutcomeInterrupted})
	if model, run := ChooseModel(history, routine, hard); !run || model != hard {
		t.Fatalf("hard job interrupted: %q %v, want the hard retry again", model, run)
	}
	history = append(history, Job{ID: "5", Model: hard, Outcome: OutcomeFail})
	if _, run := ChooseModel(history, routine, hard); run {
		t.Fatal("a failed hard retry earned another job")
	}
	// A conflict starts hard, and a restart does not change that.
	ev := Event{Kind: KindRejection, RejectionDetail: "kind=conflict"}
	if model, run := StartedModel(ev, cut("1"), routine, hard); !run || model != hard {
		t.Fatalf("interrupted conflict job: %q %v, want %q true", model, run, hard)
	}
}

// TestVerdictCannotReportInterrupted: only the runner knows a job was cut
// short, so a verdict file claiming it is as invalid as any other unknown word.
func TestVerdictCannotReportInterrupted(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), ResultFile)
	if err := os.WriteFile(path, []byte(`{"outcome":"interrupted"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadVerdict(path); err == nil {
		t.Fatal("ReadVerdict accepted interrupted")
	}
}

// groupSpawner reports a process group the way AgentSpawner does, signals
// started once the report returned, then holds the job until release is
// closed.
type groupSpawner struct {
	pgid    int
	started chan struct{}
	release chan struct{}
}

func (g groupSpawner) Spawn(_ context.Context, req SpawnRequest) SpawnResult {
	err := req.Started(g.pgid)
	if g.started != nil {
		close(g.started)
	}
	if err != nil {
		return SpawnResult{ExitCode: -1, Err: err}
	}
	<-g.release
	return SpawnResult{Verdict: &Result{Outcome: OutcomePass}}
}

// TestRunnerRecordsTheProcessGroup: the row that outlives the daemon names
// the group, and it supersedes the start row rather than doubling the job.
func TestRunnerRecordsTheProcessGroup(t *testing.T) {
	t.Parallel()
	sp := groupSpawner{pgid: 4242, started: make(chan struct{}), release: make(chan struct{})}
	r := testRunner(t, sp, DefaultMaxJobs)
	if !r.Start(context.Background(), reviewEvent("gt-x", "aaaa"), DefaultRoutineAgent, "p") {
		t.Fatal("job did not start")
	}
	// Bounded by the test binary's timeout: Started has returned, so the
	// row naming the group is written.
	<-sp.started
	active, _ := r.Ledger.Active()
	if len(active) != 1 || active[0].Pgid != 4242 {
		t.Fatalf("active = %+v, want one job carrying its group", active)
	}
	if got := r.Running(); len(got) != 1 || got[0].Pgid != 4242 {
		t.Errorf("running = %+v, want the group on the in-flight job", got)
	}
	close(sp.release)
	r.Wait()
	if active, _ = r.Ledger.Active(); len(active) != 0 {
		t.Errorf("active after the job ended = %+v", active)
	}
}

// TestRunnerStartsNothingBesideALiveOrphan: a previous daemon's job whose
// group survives the kill may still push to the bead's branch, so the bead
// gets no second job until the group is gone (gt-9bioi.5).
func TestRunnerStartsNothingBesideALiveOrphan(t *testing.T) {
	t.Parallel()
	sp := &fakeSpawner{write: &Result{Outcome: OutcomePass}}
	r := testRunner(t, sp, DefaultMaxJobs)
	orphan := Job{ID: "steward-old", Event: KindReview, Bead: "gt-x", Rig: "gastown", Head: "aaaa", Started: testEpoch, Pgid: 4242}
	if err := r.Ledger.Append(orphan); err != nil {
		t.Fatal(err)
	}
	alive := true
	var killed []int
	r.Kill = func(pgid int) error {
		killed = append(killed, pgid)
		if alive {
			return errors.New("operation not permitted")
		}
		return nil
	}
	n, err := r.ReapOrphans()
	if err == nil || n != 0 {
		t.Fatalf("ReapOrphans = %d, %v; want 0 and an error for the live group", n, err)
	}
	if !r.RunningBead("gt-x") {
		t.Error("the bead is not busy beside a live orphan")
	}
	if r.Start(context.Background(), reviewEvent("gt-x", "bbbb"), DefaultRoutineAgent, "p") {
		t.Fatal("a second job started beside the orphan")
	}
	if !r.Start(context.Background(), reviewEvent("gt-y", "cccc"), DefaultRoutineAgent, "p") {
		t.Error("an unrelated bead was held up")
	}
	r.Wait()

	alive = false
	if n, err = r.ReapOrphans(); err != nil || n != 1 {
		t.Fatalf("ReapOrphans after the group died = %d, %v; want 1, nil", n, err)
	}
	if len(killed) != 2 || killed[0] != 4242 || killed[1] != 4242 {
		t.Errorf("killed %v, want group 4242 twice", killed)
	}
	if r.RunningBead("gt-x") {
		t.Error("the bead is still busy after the orphan closed")
	}
	if !r.Start(context.Background(), reviewEvent("gt-x", "bbbb"), DefaultRoutineAgent, "p") {
		t.Error("the bead got no job once the orphan was gone")
	}
	r.Wait()
}

// TestReapOrphansSparesRunningJobs: the per-scan reap must not read the
// runner's own in-flight job as an orphan.
func TestReapOrphansSparesRunningJobs(t *testing.T) {
	t.Parallel()
	sp := groupSpawner{pgid: 4242, release: make(chan struct{})}
	r := testRunner(t, sp, DefaultMaxJobs)
	r.Kill = func(pgid int) error { t.Errorf("killed group %d of a running job", pgid); return nil }
	if !r.Start(context.Background(), reviewEvent("gt-x", "aaaa"), DefaultRoutineAgent, "p") {
		t.Fatal("job did not start")
	}
	if n, err := r.ReapOrphans(); err != nil || n != 0 {
		t.Fatalf("ReapOrphans = %d, %v; want 0, nil", n, err)
	}
	close(sp.release)
	r.Wait()
}
