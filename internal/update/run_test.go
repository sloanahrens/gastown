package update

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// runTestID is the run id the fixture's NewRunID returns.
const runTestID = "20261103T102200Z-capable"

// runTestSHA is main's build in the fixture, and so the build a run installs
// and verifies against.
const runTestSHA = "bbbb2222"

// runInstaller is an Installer a test answers for; it records whether it was
// called and with which build.
type runInstaller struct {
	calls  int
	sha    string
	result InstallResult
	err    error
	onCall func(context.Context)
}

func (i *runInstaller) Install(ctx context.Context, sha string) (InstallResult, error) {
	i.calls++
	i.sha = sha
	if i.onCall != nil {
		i.onCall(ctx)
	}
	return i.result, i.err
}

// runFixture is one test's town: a store in a temp dir, a fake clock, and the
// fakes every Deps field takes.
type runFixture struct {
	store      *Store
	clock      *fakeDrainClock
	probes     *fakeProbes
	seats      *fakeDrainProbe
	files      *fakeFiles
	hold       *Hold
	inst       *runInstaller
	maint      *MaintenanceRequest
	maintFiles *fakeMaintFiles
	verify     *fakeVerifyProbes
	log        *bytes.Buffer
	deps       Deps
	opts       Options
}

// newRunFixture builds a town where main is ahead of the installed build, no
// seat is working, nothing is due for maintenance, the installer succeeds and
// every verify probe reports the build it installed.
func newRunFixture(t *testing.T) *runFixture {
	t.Helper()
	f := &runFixture{
		store:      &Store{Dir: filepath.Join(t.TempDir(), "update")},
		clock:      &fakeDrainClock{now: drainTestStart()},
		probes:     &fakeProbes{installed: "aaaa1111", main: runTestSHA, backup: true, published: true},
		seats:      &fakeDrainProbe{answer: func(int) ([]Seat, error) { return nil, nil }},
		files:      newFakeFiles(),
		inst:       &runInstaller{result: InstallResult{Event: "installed", Commit: runTestSHA, Prev: "aaaa1111"}},
		maintFiles: newFakeMaintFiles(),
		verify:     runVerifyProbes(runTestSHA),
		log:        &bytes.Buffer{},
	}
	f.hold = NewHold(testTownRoot, f.files)
	f.maint = &MaintenanceRequest{Dir: maintTestDir, Files: f.maintFiles, Clock: f.clock}
	f.deps = Deps{
		Clock:     f.clock,
		Sleep:     f.clock.sleep,
		Store:     f.store,
		Probes:    f.probes,
		Seats:     f.seats,
		Hold:      f.hold,
		Installer: f.inst,
		Maint:     f.maint,
		Verify:    f.verify,
		NewRunID:  func() string { return runTestID },
		Log:       f.log,
	}
	f.opts = Options{
		Source:      SourceManual,
		Drain:       DrainOptions{Timeout: 0, IdleAfter: time.Minute, Poll: time.Second},
		RestartWait: time.Minute,
		RestartPoll: time.Second,
		MaintWait:   time.Minute,
		MaintPoll:   time.Second,
	}
	return f
}

// run calls Run with the fixture's deps and options.
func (f *runFixture) run(ctx context.Context) (State, error) {
	return Run(ctx, f.deps, f.opts)
}

// step returns the record for name from st.
func step(t *testing.T, st State, name StepName) StepRecord {
	t.Helper()
	for _, record := range st.Steps {
		if record.Name == name {
			return record
		}
	}
	t.Fatalf("state has no %q step: %+v", name, st.Steps)
	return StepRecord{}
}

// holdExists reports whether the hold file is in the fake file system.
func (f *runFixture) holdExists() bool {
	_, ok := f.files.entry(f.hold.Path)
	return ok
}

// runVerifyProbes reports sha from every probe, so verify passes against the
// build a run installed.
func runVerifyProbes(sha string) *fakeVerifyProbes {
	return &fakeVerifyProbes{
		gt:     sha,
		image:  sha,
		health: verifyTestGreen,
		daemon: []verifyAnswer{{version: sha}},
	}
}

// maintDaemon makes the fake daemon answer the run's maintenance request on its
// next wait, as the daemon does between two polls: it writes res as the result
// and removes the request.
func (f *runFixture) maintDaemon(t *testing.T, res MaintenanceResult) {
	t.Helper()
	f.clock.onSleep = func() {
		if _, ok := f.maintFiles.entry(f.maint.requestPath()); !ok {
			return
		}
		f.clock.onSleep = nil
		f.answerMaint(t, res)
	}
}

// answerMaint completes the request the run wrote.
func (f *runFixture) answerMaint(t *testing.T, res MaintenanceResult) {
	t.Helper()
	putMaintJSON(t, f.maintFiles, f.maint.resultPath(), res)
	if err := f.maintFiles.Remove(f.maint.requestPath()); err != nil {
		t.Fatalf("remove maintenance request: %v", err)
	}
}

// watchPublishes makes the store record every record it publishes, so a test
// can show that each step reached the disk when it started and when it ended.
func (f *runFixture) watchPublishes() func() []State {
	var mu sync.Mutex
	var got []State
	f.store.Rename = func(oldpath, newpath string) error {
		data, err := os.ReadFile(oldpath)
		if err != nil {
			return fmt.Errorf("read staging state: %w", err)
		}
		var st State
		if err := json.Unmarshal(data, &st); err != nil {
			return fmt.Errorf("parse staging state: %w", err)
		}
		mu.Lock()
		got = append(got, st)
		mu.Unlock()
		return os.Rename(oldpath, newpath)
	}
	return func() []State {
		mu.Lock()
		defer mu.Unlock()
		out := make([]State, len(got))
		copy(out, got)
		return out
	}
}

// publishedWith reports whether one of the published records shows step name
// with result want.
func publishedWith(published []State, name StepName, want StepResult) bool {
	for _, st := range published {
		for _, record := range st.Steps {
			if record.Name == name && record.Result == want {
				return true
			}
		}
	}
	return false
}

// failNthPublish makes the store fail its nth publish and publish every other
// record, so a test can take each write's failure path in turn.
func (f *runFixture) failNthPublish(n int) {
	calls := 0
	f.store.Rename = func(oldpath, newpath string) error {
		calls++
		if calls == n {
			return errors.New("disk full")
		}
		return os.Rename(oldpath, newpath)
	}
}

// publishCount runs f to completion and reports how many records it published.
func publishCount(t *testing.T, f *runFixture) int {
	t.Helper()
	published := f.watchPublishes()
	if _, err := f.run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return len(published())
}

// assertEveryPublishIsChecked fails the test unless failing each of total
// publishes in turn makes Run return the write error: a state write that fails
// must never end the run as if it had been recorded.
func assertEveryPublishIsChecked(t *testing.T, total int, fresh func(*testing.T) *runFixture) {
	t.Helper()
	for n := 1; n <= total; n++ {
		f := fresh(t)
		f.failNthPublish(n)
		if _, err := f.run(context.Background()); err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Errorf("publish %d of %d: Run error = %v, want the failed write named", n, total, err)
		}
	}
}

func TestRunStoreWriteFailureIsReturned(t *testing.T) {
	t.Parallel()
	total := publishCount(t, newRunFixture(t))
	assertEveryPublishIsChecked(t, total, newRunFixture)
}

func TestRunStoreWriteFailureDuringDrainAbortIsReturned(t *testing.T) {
	t.Parallel()
	blocked := func(t *testing.T) *runFixture {
		f := newRunFixture(t)
		f.seats.answer = func(int) ([]Seat, error) {
			return []Seat{{Name: "furiosa", Working: true, LastActive: drainTestStart()}}, nil
		}
		return f
	}
	total := publishCount(t, blocked(t))
	assertEveryPublishIsChecked(t, total, blocked)
}

func TestRunStoreWriteFailureOnLiftIsReturned(t *testing.T) {
	t.Parallel()
	liftOnly := func(t *testing.T) *runFixture {
		f := newRunFixture(t)
		f.opts.Only = map[StepName]bool{StepLift: true}
		return f
	}
	total := publishCount(t, liftOnly(t))
	assertEveryPublishIsChecked(t, total, liftOnly)
}

func TestRunDryRunPrintsThePlanAndWritesNothing(t *testing.T) {
	t.Parallel()

	t.Run("a fresh directory keeps no state.json", func(t *testing.T) {
		t.Parallel()
		f := newRunFixture(t)
		f.opts.DryRun = true

		st, err := f.run(context.Background())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if st.Outcome != OutcomeOK {
			t.Errorf("Outcome = %q, want %q", st.Outcome, OutcomeOK)
		}
		if !st.StartedAt.IsZero() || !st.HeartbeatAt.IsZero() || !st.EndedAt.IsZero() {
			t.Errorf("times = (%v, %v, %v), want none", st.StartedAt, st.HeartbeatAt, st.EndedAt)
		}
		if _, err := os.Stat(filepath.Join(f.store.Dir, stateFileName)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("stat state.json after a dry run: %v, want ErrNotExist", err)
		}
		if f.inst.calls != 0 {
			t.Errorf("installer called %d times on a dry run, want 0", f.inst.calls)
		}
		if f.holdExists() {
			t.Error("dry run took the dispatch hold")
		}
		for _, record := range st.Steps {
			action := "skip"
			if record.WillRun {
				action = "run"
			}
			want := fmt.Sprintf("%s: %s: %s", record.Name, action, record.Reason)
			if !strings.Contains(f.log.String(), want) {
				t.Errorf("log is missing %q:\n%s", want, f.log.String())
			}
		}
	})

	t.Run("a previous record is unchanged", func(t *testing.T) {
		t.Parallel()
		f := newRunFixture(t)
		if err := f.store.Write(fullState()); err != nil {
			t.Fatalf("seed state: %v", err)
		}
		path := filepath.Join(f.store.Dir, stateFileName)
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read seeded state: %v", err)
		}
		f.opts.DryRun = true

		if _, err := f.run(context.Background()); err != nil {
			t.Fatalf("Run: %v", err)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read state after the dry run: %v", err)
		}
		if !bytes.Equal(before, after) {
			t.Error("a dry run rewrote the previous run's state.json")
		}
	})
}

func TestRunInstallPathRecordsEveryStep(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t)
	published := f.watchPublishes()

	// A second writer refreshes HeartbeatAt throughout the run, as the
	// command's timer does, so the race detector checks that no step update
	// is lost to it.
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			_, err := f.store.Mutate(func(st *State) {
				st.HeartbeatAt = st.HeartbeatAt.Add(time.Second)
			})
			if err != nil && !errors.Is(err, ErrNoCurrent) {
				t.Errorf("heartbeat mutate: %v", err)
			}
			runtime.Gosched()
		}
	}()

	final, err := f.run(context.Background())
	close(done)
	wg.Wait()
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if final.Outcome != OutcomeOK {
		t.Errorf("Outcome = %q, want %q", final.Outcome, OutcomeOK)
	}
	if final.RunID != runTestID {
		t.Errorf("RunID = %q, want %q", final.RunID, runTestID)
	}
	if final.Target.SHA != "bbbb2222" {
		t.Errorf("Target.SHA = %q, want the main build", final.Target.SHA)
	}
	if f.inst.sha != runTestSHA {
		t.Errorf("installer asked for %q, want %q", f.inst.sha, runTestSHA)
	}
	for _, name := range []StepName{StepDrain, StepInstall, StepRestart, StepVerify, StepLift} {
		if got := step(t, final, name).Result; got != ResultOK {
			t.Errorf("%s Result = %q, want %q", name, got, ResultOK)
		}
	}
	if got := step(t, final, StepMaintenance); got.Result != ResultSkipped || got.Reason != "nothing is due" {
		t.Errorf("maintenance = (%q, %q), want skipped with its reason", got.Result, got.Reason)
	}
	if f.holdExists() {
		t.Error("the run owns the hold, so it must have released it")
	}
	if final.Hold.Owned {
		t.Error("Hold.Owned = true after the hold was released")
	}

	// Each step that ran reached the disk at its start and again at its end.
	records := published()
	for _, name := range []StepName{StepDrain, StepInstall, StepLift} {
		if !publishedWith(records, name, ResultRunning) {
			t.Errorf("%s was never published as running", name)
		}
		if !publishedWith(records, name, ResultOK) {
			t.Errorf("%s was never published as ok", name)
		}
	}
}

func TestRunBlockedOrUnreadableDrainAborts(t *testing.T) {
	t.Parallel()
	errSeats := errors.New("seat heartbeats unreadable")
	tests := []struct {
		name   string
		answer func(int) ([]Seat, error)
		want   string
	}{
		{
			name: "a seat that stays working",
			answer: func(int) ([]Seat, error) {
				return []Seat{{Name: "furiosa", Working: true, LastActive: drainTestStart()}}, nil
			},
			want: "furiosa",
		},
		{
			name:   "a seat probe that always errors",
			answer: func(int) ([]Seat, error) { return nil, errSeats },
			want:   errSeats.Error(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newRunFixture(t)
			f.seats = &fakeDrainProbe{answer: tt.answer}
			f.deps.Seats = f.seats

			final, err := f.run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if final.Outcome != OutcomeAborted {
				t.Errorf("Outcome = %q, want %q", final.Outcome, OutcomeAborted)
			}
			drain := step(t, final, StepDrain)
			if drain.Result != ResultFailed {
				t.Errorf("drain Result = %q, want %q", drain.Result, ResultFailed)
			}
			if !strings.Contains(drain.Detail, tt.want) {
				t.Errorf("drain Detail = %q, want it to name %q", drain.Detail, tt.want)
			}
			if got := step(t, final, StepInstall).Result; got != ResultPending {
				t.Errorf("install Result = %q, want %q", got, ResultPending)
			}
			if f.inst.calls != 0 {
				t.Errorf("installer called %d times, want 0 after a drain that did not finish", f.inst.calls)
			}
			if f.holdExists() {
				t.Error("the run owns the hold, so it must have released it")
			}
		})
	}
}

func TestRunInstallFailureFailsAndBusyAborts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		result      InstallResult
		err         error
		wantOutcome Outcome
		wantStep    StepResult
		wantDetail  string
	}{
		{
			name:        "a failed install",
			result:      InstallResult{Event: "failed", Exit: 1, Reason: "the build did not compile"},
			wantOutcome: OutcomeFailed,
			wantStep:    ResultFailed,
			wantDetail:  "the build did not compile",
		},
		{
			name:        "a busy installer",
			result:      InstallResult{Event: "refused", Exit: 3, Reason: "the installer lock is held"},
			wantOutcome: OutcomeAborted,
			wantStep:    ResultDeferred,
			wantDetail:  "the installer lock is held",
		},
		{
			name:        "an installer whose run errored after a success line",
			result:      InstallResult{Event: "installed", Exit: 0},
			err:         errors.New("reading the installer's output: pipe closed"),
			wantOutcome: OutcomeFailed,
			wantStep:    ResultFailed,
			wantDetail:  "reading the installer's output",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newRunFixture(t)
			f.inst.result = tt.result
			f.inst.err = tt.err

			final, err := f.run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if final.Outcome != tt.wantOutcome {
				t.Errorf("Outcome = %q, want %q", final.Outcome, tt.wantOutcome)
			}
			install := step(t, final, StepInstall)
			if install.Result != tt.wantStep {
				t.Errorf("install Result = %q, want %q", install.Result, tt.wantStep)
			}
			if !strings.Contains(install.Detail, tt.wantDetail) {
				t.Errorf("install Detail = %q, want it to carry %q", install.Detail, tt.wantDetail)
			}
			if f.holdExists() {
				t.Error("the run owns the hold, so it must have released it")
			}
		})
	}
}

func TestRunForeignHoldIsRespected(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t)
	const foreign = "operator hold: maintenance window"
	f.files.put(f.hold.Path, foreign, 0o600)

	final, err := f.run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := step(t, final, StepDrain).Result; got != ResultOK {
		t.Errorf("drain Result = %q, want %q: a foreign hold still drains", got, ResultOK)
	}
	if f.inst.calls != 1 {
		t.Errorf("installer called %d times, want 1", f.inst.calls)
	}
	entry, ok := f.files.entry(f.hold.Path)
	if !ok {
		t.Fatal("the foreign hold file was removed")
	}
	if string(entry.data) != foreign || entry.mode != 0o600 {
		t.Errorf("foreign hold = (%q, %o), want (%q, 600) untouched", entry.data, entry.mode, foreign)
	}
	lift := step(t, final, StepLift)
	if lift.Result != ResultSkipped || lift.Detail != noHoldDetail {
		t.Errorf("lift = (%q, %q), want skipped with %q", lift.Result, lift.Detail, noHoldDetail)
	}
	if final.Hold.Owned {
		t.Error("Hold.Owned = true for a hold the run did not create")
	}
	if final.Hold.Note != foreign {
		t.Errorf("Hold.Note = %q, want the foreign hold's content %q", final.Hold.Note, foreign)
	}
}

func TestRunCancelledIsInterruptedAndReleasesTheHold(t *testing.T) {
	t.Parallel()

	t.Run("during the install", func(t *testing.T) {
		t.Parallel()
		f := newRunFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		f.inst.onCall = func(context.Context) { cancel() }
		f.inst.err = context.Canceled

		final, err := f.run(ctx)
		if err != nil {
			t.Fatalf("Run returned %v, want no error for a canceled run", err)
		}
		if final.Outcome != OutcomeInterrupted {
			t.Errorf("Outcome = %q, want %q", final.Outcome, OutcomeInterrupted)
		}
		if f.inst.calls != 1 {
			t.Errorf("installer called %d times, want 1", f.inst.calls)
		}
		if f.holdExists() {
			t.Error("the canceled run left the hold it owns in place")
		}
		if got := step(t, final, StepLift).Result; got != ResultPending {
			t.Errorf("lift Result = %q, want %q: the run never reached it", got, ResultPending)
		}
	})

	t.Run("between steps", func(t *testing.T) {
		t.Parallel()
		f := newRunFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		// The cancel lands as the drain step's ok is published, so the walk
		// reaches its next step with the context already done.
		f.store.Rename = func(oldpath, newpath string) error {
			data, err := os.ReadFile(oldpath)
			if err != nil {
				return fmt.Errorf("read staging state: %w", err)
			}
			var st State
			if err := json.Unmarshal(data, &st); err != nil {
				return fmt.Errorf("parse staging state: %w", err)
			}
			for _, record := range st.Steps {
				if record.Name == StepDrain && record.Result == ResultOK {
					cancel()
				}
			}
			return os.Rename(oldpath, newpath)
		}

		final, err := f.run(ctx)
		if err != nil {
			t.Fatalf("Run returned %v, want no error for a canceled run", err)
		}
		if final.Outcome != OutcomeInterrupted {
			t.Errorf("Outcome = %q, want %q", final.Outcome, OutcomeInterrupted)
		}
		if got := step(t, final, StepDrain).Result; got != ResultOK {
			t.Errorf("drain Result = %q, want %q: the drain finished before the cancel", got, ResultOK)
		}
		if f.inst.calls != 0 {
			t.Errorf("installer called %d times, want 0 after the cancel", f.inst.calls)
		}
		if f.holdExists() {
			t.Error("the canceled run left the hold it owns in place")
		}
	})
}

func TestRunNoInstallTakesNoHold(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t)
	// Nothing to install, but the archive is behind, so only maintenance is
	// planned: with no drain there is no hold, and the lift step says so.
	f.probes.installed = runTestSHA
	f.probes.commits = 5
	f.maintDaemon(t, MaintenanceResult{RunID: runTestID, Outcome: "completed", Detail: "the archive is current"})

	final, err := f.run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.inst.calls != 0 {
		t.Errorf("installer called %d times, want 0", f.inst.calls)
	}
	if f.holdExists() {
		t.Error("a run with nothing to install took the dispatch hold")
	}
	if got := step(t, final, StepDrain).Result; got != ResultSkipped {
		t.Errorf("drain Result = %q, want %q", got, ResultSkipped)
	}
	if got := step(t, final, StepInstall).Result; got != ResultSkipped {
		t.Errorf("install Result = %q, want %q", got, ResultSkipped)
	}
	lift := step(t, final, StepLift)
	if lift.Result != ResultSkipped || lift.Detail != noHoldDetail {
		t.Errorf("lift = (%q, %q), want skipped with %q", lift.Result, lift.Detail, noHoldDetail)
	}
	if final.Outcome != OutcomeOK {
		t.Errorf("Outcome = %q, want %q", final.Outcome, OutcomeOK)
	}
}

func TestRunPlanErrorReturnsItAndWritesNothing(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t)
	f.probes.errMain = errors.New("origin/main cannot be read")

	_, err := f.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "origin/main cannot be read") {
		t.Fatalf("Run error = %v, want the probe's error", err)
	}
	if _, statErr := os.Stat(filepath.Join(f.store.Dir, stateFileName)); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("stat state.json after a failed plan: %v, want ErrNotExist", statErr)
	}
	if f.inst.calls != 0 {
		t.Errorf("installer called %d times, want 0", f.inst.calls)
	}
	if f.holdExists() {
		t.Error("a failed plan took the dispatch hold")
	}
}

func TestRunHoldAcquireFailureAbortsWithoutInstalling(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t)
	f.files.statErr[f.hold.Path] = errors.New("permission denied")

	final, err := f.run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Outcome != OutcomeAborted {
		t.Errorf("Outcome = %q, want %q", final.Outcome, OutcomeAborted)
	}
	drain := step(t, final, StepDrain)
	if drain.Result != ResultFailed || !strings.Contains(drain.Detail, "permission denied") {
		t.Errorf("drain = (%q, %q), want failed naming the unreadable hold", drain.Result, drain.Detail)
	}
	if f.inst.calls != 0 {
		t.Errorf("installer called %d times, want 0: an unreadable hold must stop the run", f.inst.calls)
	}
}

func TestRunHoldReleaseFailureIsRecorded(t *testing.T) {
	t.Parallel()

	t.Run("after the walk ends early", func(t *testing.T) {
		t.Parallel()
		f := newRunFixture(t)
		f.seats.answer = func(int) ([]Seat, error) {
			return []Seat{{Name: "furiosa", Working: true, LastActive: drainTestStart()}}, nil
		}
		f.files.removeErr[f.hold.Path] = errors.New("permission denied")

		final, err := f.run(context.Background())
		if err != nil {
			t.Fatalf("Run returned %v, want the failure in the record", err)
		}
		if got := step(t, final, StepDrain).Result; got != ResultFailed {
			t.Errorf("drain Result = %q, want %q", got, ResultFailed)
		}
		if final.Outcome != OutcomeFailed {
			t.Errorf("Outcome = %q, want %q: a hold left set is not a clean ending", final.Outcome, OutcomeFailed)
		}
		lift := step(t, final, StepLift)
		if lift.Result != ResultFailed || !strings.Contains(lift.Detail, "permission denied") {
			t.Errorf("lift = (%q, %q), want failed naming the release error", lift.Result, lift.Detail)
		}
		if !f.holdExists() {
			t.Error("the test expected the hold to remain when its removal fails")
		}
	})

	t.Run("at the lift step", func(t *testing.T) {
		t.Parallel()
		f := newRunFixture(t)
		f.files.removeErr[f.hold.Path] = errors.New("permission denied")

		final, err := f.run(context.Background())
		if err != nil {
			t.Fatalf("Run returned %v, want the failure in the record", err)
		}
		if final.Outcome != OutcomeFailed {
			t.Errorf("Outcome = %q, want %q", final.Outcome, OutcomeFailed)
		}
		lift := step(t, final, StepLift)
		if lift.Result != ResultFailed || !strings.Contains(lift.Detail, "permission denied") {
			t.Errorf("lift = (%q, %q), want failed naming the release error", lift.Result, lift.Detail)
		}
		if !f.holdExists() {
			t.Error("the test expected the hold to remain when its removal fails")
		}
	})
}

func TestRunMaintenanceRunsBetweenDrainAndInstall(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t)
	f.probes.commits = 5 // the archive is behind, so maintenance is planned

	var events []string
	f.seats.answer = func(int) ([]Seat, error) {
		if f.holdExists() {
			events = append(events, "hold")
		}
		events = append(events, "drain")
		return nil, nil
	}
	f.inst.onCall = func(context.Context) { events = append(events, "install") }
	// The daemon takes the request while the run waits for it, so the wait's
	// first sleep is where it answers.
	f.clock.onSleep = func() {
		if _, ok := f.maintFiles.entry(f.maint.requestPath()); !ok {
			return
		}
		f.clock.onSleep = nil
		events = append(events, "maintenance")
		f.answerMaint(t, MaintenanceResult{RunID: runTestID, Outcome: "completed", Detail: "the archive is current"})
	}

	final, err := f.run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if want := []string{"hold", "drain", "maintenance", "install"}; !slices.Equal(events, want) {
		t.Errorf("events = %v, want %v", events, want)
	}
	if final.Outcome != OutcomeOK {
		t.Errorf("Outcome = %q, want %q", final.Outcome, OutcomeOK)
	}
	maint := step(t, final, StepMaintenance)
	if maint.Result != ResultOK {
		t.Errorf("maintenance Result = %q, want %q", maint.Result, ResultOK)
	}
	if maint.Detail != "the archive is current" {
		t.Errorf("maintenance Detail = %q, want the cycle's detail", maint.Detail)
	}
	if !strings.Contains(f.log.String(), "the archive is current") {
		t.Errorf("log is missing the maintenance detail:\n%s", f.log.String())
	}
	for _, name := range []StepName{StepDrain, StepMaintenance, StepInstall, StepRestart, StepVerify, StepLift} {
		if got := step(t, final, name).Result; got != ResultOK {
			t.Errorf("%s Result = %q, want %q", name, got, ResultOK)
		}
	}
	if f.holdExists() {
		t.Error("the run owns the hold, so it must have released it")
	}
}

func TestRunMaintenanceFailureOrDeferralIsPartialAndInstallStillRuns(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		outcome    string
		detail     string
		requestErr error
		wantStep   StepResult
		wantDetail string
	}{
		{
			name:       "a failed cycle",
			outcome:    maintenanceFailed,
			detail:     "the archive is locked",
			wantStep:   ResultFailed,
			wantDetail: "the archive is locked",
		},
		{
			name:       "a deferred cycle",
			outcome:    maintenanceDeferred,
			detail:     "the quiet guard held the gc",
			wantStep:   ResultDeferred,
			wantDetail: "the quiet guard held the gc",
		},
		{
			name:       "an outcome the protocol does not define",
			outcome:    "half-done",
			detail:     "the daemon wrote its own word",
			wantStep:   ResultFailed,
			wantDetail: "the daemon wrote its own word",
		},
		{
			name:       "a request the daemon cannot take",
			requestErr: errors.New("permission denied"),
			wantStep:   ResultFailed,
			wantDetail: "permission denied",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newRunFixture(t)
			f.probes.commits = 5
			if tt.requestErr != nil {
				f.maintFiles.createErr[f.maint.requestPath()] = tt.requestErr
			} else {
				f.maintDaemon(t, MaintenanceResult{RunID: runTestID, Outcome: tt.outcome, Detail: tt.detail})
			}

			final, err := f.run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if final.Outcome != OutcomePartial {
				t.Errorf("Outcome = %q, want %q", final.Outcome, OutcomePartial)
			}
			if f.inst.calls != 1 {
				t.Errorf("installer called %d times, want 1: maintenance must not stop the install", f.inst.calls)
			}
			if got := step(t, final, StepInstall).Result; got != ResultOK {
				t.Errorf("install Result = %q, want %q", got, ResultOK)
			}
			maint := step(t, final, StepMaintenance)
			if maint.Result != tt.wantStep {
				t.Errorf("maintenance Result = %q, want %q", maint.Result, tt.wantStep)
			}
			if !strings.Contains(maint.Detail, tt.wantDetail) {
				t.Errorf("maintenance Detail = %q, want it to carry %q", maint.Detail, tt.wantDetail)
			}
			if !strings.Contains(f.log.String(), tt.wantDetail) {
				t.Errorf("log is missing %q:\n%s", tt.wantDetail, f.log.String())
			}
			if f.holdExists() {
				t.Error("the run owns the hold, so it must have released it")
			}
		})
	}
}

func TestRunRestartTimeoutIsPartialInstalledRestartPending(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t)
	// The daemon never comes up on the new build, so every poll reports the
	// build that was installed before this run.
	f.verify.daemon = []verifyAnswer{{version: verifyTestOld}}

	final, err := f.run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Outcome != OutcomePartial {
		t.Errorf("Outcome = %q, want %q", final.Outcome, OutcomePartial)
	}
	if got := step(t, final, StepInstall).Result; got != ResultOK {
		t.Errorf("install Result = %q, want %q", got, ResultOK)
	}
	restart := step(t, final, StepRestart)
	if restart.Result != ResultDeferred || restart.Detail != restartPendingDetail {
		t.Errorf("restart = (%q, %q), want deferred with %q", restart.Result, restart.Detail, restartPendingDetail)
	}
	verify := step(t, final, StepVerify)
	if verify.Result != ResultSkipped || verify.Detail != verifyRestartPendingDetail {
		t.Errorf("verify = (%q, %q), want skipped with %q", verify.Result, verify.Detail, verifyRestartPendingDetail)
	}
	if f.verify.daemonCalls < 2 {
		t.Errorf("the daemon was polled %d times, want the wait to keep polling until its timeout", f.verify.daemonCalls)
	}
	if f.verify.gtCalls+f.verify.imageCalls+f.verify.healthCalls != 0 {
		t.Errorf("verify ran (%d gt, %d image, %d health calls), want none after a restart that timed out",
			f.verify.gtCalls, f.verify.imageCalls, f.verify.healthCalls)
	}
	if got := step(t, final, StepLift).Result; got != ResultOK {
		t.Errorf("lift Result = %q, want %q", got, ResultOK)
	}
	if f.holdExists() {
		t.Error("the run owns the hold, so it must have released it")
	}
}

func TestRunVerifyFailureIsPartialNamingTheChecks(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t)
	f.verify.gt = verifyTestOld

	final, err := f.run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Outcome != OutcomePartial {
		t.Errorf("Outcome = %q, want %q", final.Outcome, OutcomePartial)
	}
	if got := step(t, final, StepInstall).Result; got != ResultOK {
		t.Errorf("install Result = %q, want %q", got, ResultOK)
	}
	if got := step(t, final, StepRestart).Result; got != ResultOK {
		t.Errorf("restart Result = %q, want %q", got, ResultOK)
	}
	verify := step(t, final, StepVerify)
	if verify.Result != ResultFailed {
		t.Errorf("verify Result = %q, want %q", verify.Result, ResultFailed)
	}
	if !strings.Contains(verify.Detail, checkGTVersion) {
		t.Errorf("verify Detail = %q, want the failed check's name %q", verify.Detail, checkGTVersion)
	}
	if !strings.Contains(verify.Detail, verifyTestOld) {
		t.Errorf("verify Detail = %q, want what the check reported %q", verify.Detail, verifyTestOld)
	}
	if strings.Contains(verify.Detail, checkHealth) {
		t.Errorf("verify Detail = %q, want only the checks that failed", verify.Detail)
	}
	if f.holdExists() {
		t.Error("the run owns the hold, so it must have released it")
	}
}

func TestRunMaintenanceOnlyTakesNoHold(t *testing.T) {
	t.Parallel()
	f := newRunFixture(t)
	f.probes.installed = runTestSHA // nothing to install
	f.probes.commits = 5            // but the archive is behind
	f.maintDaemon(t, MaintenanceResult{RunID: runTestID, Outcome: "completed", Detail: "the archive is current"})

	final, err := f.run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if final.Outcome != OutcomeOK {
		t.Errorf("Outcome = %q, want %q", final.Outcome, OutcomeOK)
	}
	if got := step(t, final, StepMaintenance).Result; got != ResultOK {
		t.Errorf("maintenance Result = %q, want %q", got, ResultOK)
	}
	if f.inst.calls != 0 {
		t.Errorf("installer called %d times, want 0", f.inst.calls)
	}
	if f.holdExists() {
		t.Error("a run with nothing to install took the dispatch hold")
	}
	for _, name := range []StepName{StepDrain, StepInstall} {
		if got := step(t, final, name).Result; got != ResultSkipped {
			t.Errorf("%s Result = %q, want %q", name, got, ResultSkipped)
		}
	}
	lift := step(t, final, StepLift)
	if lift.Result != ResultSkipped || lift.Detail != noHoldDetail {
		t.Errorf("lift = (%q, %q), want skipped with %q", lift.Result, lift.Detail, noHoldDetail)
	}
	if f.verify.gtCalls+f.verify.daemonCalls+f.verify.imageCalls+f.verify.healthCalls != 0 {
		t.Error("verify ran on a run that installed nothing")
	}
}

func TestRunCancelledDuringTheWaitsIsInterrupted(t *testing.T) {
	t.Parallel()

	t.Run("during maintenance", func(t *testing.T) {
		t.Parallel()
		f := newRunFixture(t)
		f.probes.commits = 5
		ctx, cancel := context.WithCancel(context.Background())
		f.clock.onSleep = func() { cancel() }

		final, err := f.run(ctx)
		if err != nil {
			t.Fatalf("Run returned %v, want no error for a canceled run", err)
		}
		if final.Outcome != OutcomeInterrupted {
			t.Errorf("Outcome = %q, want %q", final.Outcome, OutcomeInterrupted)
		}
		maint := step(t, final, StepMaintenance)
		if maint.Result != ResultFailed || maint.Detail != "canceled" {
			t.Errorf("maintenance = (%q, %q), want failed with %q", maint.Result, maint.Detail, "canceled")
		}
		if f.inst.calls != 0 {
			t.Errorf("installer called %d times, want 0 after the cancel", f.inst.calls)
		}
		if f.holdExists() {
			t.Error("the canceled run left the hold it owns in place")
		}
	})

	t.Run("during the restart", func(t *testing.T) {
		t.Parallel()
		f := newRunFixture(t)
		f.verify.daemon = []verifyAnswer{{version: verifyTestOld}}
		ctx, cancel := context.WithCancel(context.Background())
		f.clock.onSleep = func() { cancel() }

		final, err := f.run(ctx)
		if err != nil {
			t.Fatalf("Run returned %v, want no error for a canceled run", err)
		}
		if final.Outcome != OutcomeInterrupted {
			t.Errorf("Outcome = %q, want %q", final.Outcome, OutcomeInterrupted)
		}
		restart := step(t, final, StepRestart)
		if restart.Result != ResultFailed || restart.Detail != "canceled" {
			t.Errorf("restart = (%q, %q), want failed with %q", restart.Result, restart.Detail, "canceled")
		}
		if got := step(t, final, StepVerify).Result; got != ResultPending {
			t.Errorf("verify Result = %q, want %q: the run never reached it", got, ResultPending)
		}
		if f.inst.calls != 1 {
			t.Errorf("installer called %d times, want 1", f.inst.calls)
		}
		if f.holdExists() {
			t.Error("the canceled run left the hold it owns in place")
		}
	})
}

func TestRunAppendsHistoryOnEveryEnding(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		want   Outcome
		dryRun bool
		setup  func(t *testing.T, f *runFixture) context.Context
	}{
		{name: "ok", want: OutcomeOK},
		{
			name: "partial",
			want: OutcomePartial,
			setup: func(t *testing.T, f *runFixture) context.Context {
				f.verify.gt = verifyTestOld
				return context.Background()
			},
		},
		{
			name: "failed",
			want: OutcomeFailed,
			setup: func(t *testing.T, f *runFixture) context.Context {
				f.inst.result = InstallResult{Event: "failed", Exit: 1, Reason: "the build did not compile"}
				return context.Background()
			},
		},
		{
			name: "aborted",
			want: OutcomeAborted,
			setup: func(t *testing.T, f *runFixture) context.Context {
				f.seats.answer = func(int) ([]Seat, error) {
					return []Seat{{Name: "furiosa", Working: true, LastActive: drainTestStart()}}, nil
				}
				return context.Background()
			},
		},
		{
			name: "interrupted",
			want: OutcomeInterrupted,
			setup: func(t *testing.T, f *runFixture) context.Context {
				ctx, cancel := context.WithCancel(context.Background())
				f.inst.onCall = func(context.Context) { cancel() }
				f.inst.err = context.Canceled
				return ctx
			},
		},
		{name: "a dry run", want: OutcomeOK, dryRun: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newRunFixture(t)
			f.opts.DryRun = tt.dryRun
			ctx := context.Background()
			if tt.setup != nil {
				ctx = tt.setup(t, f)
			}

			final, err := f.run(ctx)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if final.Outcome != tt.want {
				t.Errorf("Outcome = %q, want %q", final.Outcome, tt.want)
			}
			path := filepath.Join(f.store.Dir, historyFileName)
			if tt.dryRun {
				if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
					t.Errorf("stat history.jsonl after a dry run: %v, want ErrNotExist", err)
				}
				return
			}
			lines := readHistoryLines(t, path)
			if len(lines) != 1 {
				t.Fatalf("history has %d lines, want 1", len(lines))
			}
			if lines[0].Outcome != tt.want {
				t.Errorf("history outcome = %q, want %q", lines[0].Outcome, tt.want)
			}
			if lines[0].RunID != runTestID {
				t.Errorf("history run id = %q, want %q", lines[0].RunID, runTestID)
			}
		})
	}

	t.Run("an append error is logged and the run still ends", func(t *testing.T) {
		t.Parallel()
		f := newRunFixture(t)
		f.store.openHistory = func(string) (historyWriter, error) {
			return nil, errors.New("the disk is full")
		}

		final, err := f.run(context.Background())
		if err != nil {
			t.Fatalf("Run returned %v, want the history failure in the log", err)
		}
		if final.Outcome != OutcomeOK {
			t.Errorf("Outcome = %q, want %q", final.Outcome, OutcomeOK)
		}
		if !strings.Contains(f.log.String(), "the disk is full") {
			t.Errorf("log is missing the history failure:\n%s", f.log.String())
		}
	})
}
