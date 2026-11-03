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
	"strings"
	"sync"
	"testing"
	"time"
)

// runTestID is the run id the fixture's NewRunID returns.
const runTestID = "20261103T102200Z-capable"

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
	store  *Store
	clock  *fakeDrainClock
	probes *fakeProbes
	seats  *fakeDrainProbe
	files  *fakeFiles
	hold   *Hold
	inst   *runInstaller
	log    *bytes.Buffer
	deps   Deps
	opts   Options
}

// newRunFixture builds a town where main is ahead of the installed build, no
// seat is working, nothing is due for maintenance and the installer succeeds.
func newRunFixture(t *testing.T) *runFixture {
	t.Helper()
	f := &runFixture{
		store:  &Store{Dir: filepath.Join(t.TempDir(), "update")},
		clock:  &fakeDrainClock{now: drainTestStart()},
		probes: &fakeProbes{installed: "aaaa1111", main: "bbbb2222", backup: true, published: true},
		seats:  &fakeDrainProbe{answer: func(int) ([]Seat, error) { return nil, nil }},
		files:  newFakeFiles(),
		inst:   &runInstaller{result: InstallResult{Event: "installed", Commit: "bbbb2222", Prev: "aaaa1111"}},
		log:    &bytes.Buffer{},
	}
	f.hold = NewHold(testTownRoot, f.files)
	f.deps = Deps{
		Clock:     f.clock,
		Sleep:     f.clock.sleep,
		Store:     f.store,
		Probes:    f.probes,
		Seats:     f.seats,
		Hold:      f.hold,
		Installer: f.inst,
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
	if f.inst.sha != "bbbb2222" {
		t.Errorf("installer asked for %q, want %q", f.inst.sha, "bbbb2222")
	}
	for _, name := range []StepName{StepDrain, StepInstall, StepLift} {
		if got := step(t, final, name).Result; got != ResultOK {
			t.Errorf("%s Result = %q, want %q", name, got, ResultOK)
		}
	}
	if got := step(t, final, StepMaintenance); got.Result != ResultSkipped || got.Reason != "nothing is due" {
		t.Errorf("maintenance = (%q, %q), want skipped with its reason", got.Result, got.Reason)
	}
	for _, name := range []StepName{StepRestart, StepVerify} {
		if got := step(t, final, name); got.Result != ResultSkipped || got.Detail != notImplementedDetail {
			t.Errorf("%s = (%q, %q), want skipped with %q", name, got.Result, got.Detail, notImplementedDetail)
		}
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
	f.probes.installed = "bbbb2222"
	f.probes.commits = 5

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
