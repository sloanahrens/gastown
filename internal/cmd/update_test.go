package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/update"
)

// updateTestClock is a fixed clock for the fakes.
type updateTestClock struct{ now time.Time }

func (c updateTestClock) Now() time.Time { return c.now }

// updateTestSeams is the command over a fake town, fake effects and the given
// orchestrator.
func updateTestSeams(townRoot string, deps update.Deps, run func(context.Context, update.Deps, update.Options) (update.State, error)) updateSeams {
	if run == nil {
		run = okUpdateRun
	}
	return updateSeams{
		FindTownRoot: func() (string, error) { return townRoot, nil },
		DepsFactory:  func(string) (update.Deps, error) { return deps, nil },
		Run:          run,
	}
}

// okUpdateRun answers an ok run without doing anything.
func okUpdateRun(context.Context, update.Deps, update.Options) (update.State, error) {
	return update.State{Outcome: update.OutcomeOK}, nil
}

// updateTestDeps is a Deps with only the fields the command itself reaches: a
// clock and a store in a temporary directory.
func updateTestDeps(t *testing.T) update.Deps {
	t.Helper()
	return update.Deps{
		Clock: updateTestClock{},
		Store: &update.Store{Dir: t.TempDir()},
	}
}

// runUpdateCommand executes a fresh gt update over seams and returns the
// status the production mapping gives its error, plus both output streams.
func runUpdateCommand(t *testing.T, ctx context.Context, seams updateSeams, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	c := newUpdateCmdWith(seams)
	var out, errOut bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&errOut)
	c.SetArgs(args)
	c.SetContext(ctx)
	err := c.Execute()
	if err != nil {
		code = exitCodeForError(err)
	}
	return code, out.String(), errOut.String()
}

// recordingUpdateRun records the Options the command built and answers with a
// state the test chose.
type recordingUpdateRun struct {
	calls int
	opts  update.Options
	state update.State
	err   error
}

func (r *recordingUpdateRun) call(_ context.Context, _ update.Deps, o update.Options) (update.State, error) {
	r.calls++
	r.opts = o
	if r.err != nil {
		return update.State{}, r.err
	}
	return r.state, nil
}

// TestUpdateFlagsValidateStepNames proves repeated --skip and --only values
// reach the run's Options and that an unknown name is refused with the valid
// names listed, before anything is run.
func TestUpdateFlagsValidateStepNames(t *testing.T) {
	t.Parallel()
	rec := &recordingUpdateRun{state: update.State{Outcome: update.OutcomeOK}}
	seams := updateTestSeams(t.TempDir(), updateTestDeps(t), rec.call)

	code, _, stderr := runUpdateCommand(t, context.Background(), seams,
		"--skip", "drain", "--skip", "maintenance", "--only", "verify")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
	}
	if rec.calls != 1 {
		t.Fatalf("Run called %d times, want 1", rec.calls)
	}
	for _, want := range []update.StepName{update.StepDrain, update.StepMaintenance} {
		if !rec.opts.Skip[want] {
			t.Errorf("Skip[%s] = false, want true", want)
		}
	}
	if len(rec.opts.Skip) != 2 {
		t.Errorf("Skip = %v, want exactly drain and maintenance", rec.opts.Skip)
	}
	if !rec.opts.Only[update.StepVerify] || len(rec.opts.Only) != 1 {
		t.Errorf("Only = %v, want exactly verify", rec.opts.Only)
	}

	rec.calls = 0
	code, _, stderr = runUpdateCommand(t, context.Background(), seams, "--skip", "install", "--only", "frobnicate")
	if code != 1 {
		t.Fatalf("exit = %d for an unknown step, want 1", code)
	}
	if rec.calls != 0 {
		t.Errorf("Run called %d times for a refused flag, want 0", rec.calls)
	}
	if !strings.Contains(stderr, `unknown update step "frobnicate"`) {
		t.Errorf("stderr does not name the unknown step:\n%s", stderr)
	}
	for _, step := range updateSelectableSteps {
		if !strings.Contains(stderr, string(step)) {
			t.Errorf("stderr does not list the valid step %q:\n%s", step, stderr)
		}
	}

	rec.calls = 0
	code, _, stderr = runUpdateCommand(t, context.Background(), seams, "--skip", "frobnicate")
	if code != 1 {
		t.Fatalf("exit = %d for an unknown skipped step, want 1", code)
	}
	if rec.calls != 0 {
		t.Errorf("Run called %d times for a refused --skip, want 0", rec.calls)
	}
	if !strings.Contains(stderr, `unknown update step "frobnicate"`) {
		t.Errorf("stderr does not name the unknown skipped step:\n%s", stderr)
	}
}

// TestUpdateExitCodesFollowTheOutcome proves each outcome maps to its status,
// that a dry run exits 0, and that a failed run of the orchestrator exits 1
// with its error shown.
func TestUpdateExitCodesFollowTheOutcome(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		outcome update.Outcome
		args    []string
		want    int
	}{
		{"ok", update.OutcomeOK, nil, 0},
		{"failed", update.OutcomeFailed, nil, 1},
		{"partial", update.OutcomePartial, nil, 3},
		{"aborted", update.OutcomeAborted, nil, 4},
		{"interrupted", update.OutcomeInterrupted, nil, 130},
		{"dry run", update.OutcomeOK, []string{"--dry-run"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingUpdateRun{state: update.State{V: 1, RunID: "run-x", Outcome: tc.outcome}}
			seams := updateTestSeams(t.TempDir(), updateTestDeps(t), rec.call)
			code, stdout, stderr := runUpdateCommand(t, context.Background(), seams, tc.args...)
			if code != tc.want {
				t.Fatalf("exit = %d for outcome %q, want %d; stderr:\n%s", code, tc.outcome, tc.want, stderr)
			}
			if !strings.Contains(stdout, "outcome: "+string(tc.outcome)) {
				t.Errorf("report does not print the outcome %q:\n%s", tc.outcome, stdout)
			}
			dryRun := len(tc.args) > 0 && tc.args[0] == "--dry-run"
			if rec.opts.DryRun != dryRun {
				t.Errorf("DryRun = %v, want %v", rec.opts.DryRun, dryRun)
			}
		})
	}

	t.Run("orchestrator error", func(t *testing.T) {
		rec := &recordingUpdateRun{err: errors.New("MainSHA: no such remote")}
		seams := updateTestSeams(t.TempDir(), updateTestDeps(t), rec.call)
		code, stdout, stderr := runUpdateCommand(t, context.Background(), seams)
		if code != 1 {
			t.Fatalf("exit = %d for a failed run, want 1", code)
		}
		if !strings.Contains(stderr, "MainSHA: no such remote") {
			t.Errorf("stderr does not name the failure:\n%s", stderr)
		}
		if strings.Contains(stdout, "outcome:") {
			t.Errorf("a failed run printed a report:\n%s", stdout)
		}
	})
}

// TestUpdateSetupErrorsExitFailed proves a town that cannot be found and a
// dependency set that cannot be built both end as a failed run rather than a
// success or a panic.
func TestUpdateSetupErrorsExitFailed(t *testing.T) {
	t.Parallel()
	t.Run("town root", func(t *testing.T) {
		seams := updateSeams{
			FindTownRoot: func() (string, error) { return "", errors.New("not in a town") },
			DepsFactory: func(string) (update.Deps, error) {
				t.Fatal("DepsFactory called for a town that could not be found")
				return update.Deps{}, nil
			},
			Run: okUpdateRun,
		}
		code, _, stderr := runUpdateCommand(t, context.Background(), seams)
		if code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if !strings.Contains(stderr, "not in a town") {
			t.Errorf("stderr does not name the failure:\n%s", stderr)
		}
	})

	t.Run("deps", func(t *testing.T) {
		seams := updateSeams{
			FindTownRoot: func() (string, error) { return t.TempDir(), nil },
			DepsFactory: func(string) (update.Deps, error) {
				return update.Deps{}, errors.New("no gt source checkout")
			},
			Run: okUpdateRun,
		}
		code, _, stderr := runUpdateCommand(t, context.Background(), seams)
		if code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		if !strings.Contains(stderr, "no gt source checkout") {
			t.Errorf("stderr does not name the failure:\n%s", stderr)
		}
	})
}

// TestUpdateLockedExitsFiveNamingTheRun proves a held lock ends the command at
// once with status 5 and the name, step and start time of the run holding it.
func TestUpdateLockedExitsFiveNamingTheRun(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	dir := update.Dir(townRoot)
	started := time.Date(2026, 11, 3, 12, 30, 0, 0, time.UTC)
	store := &update.Store{Dir: dir}
	if err := store.Write(update.State{
		RunID:     "run-holder",
		Current:   update.StepInstall,
		StartedAt: started,
		Outcome:   update.OutcomeRunning,
	}); err != nil {
		t.Fatalf("writing the held run's state: %v", err)
	}
	release, err := update.AcquireLock(dir)
	if err != nil {
		t.Fatalf("holding the lock: %v", err)
	}
	defer release()

	rec := &recordingUpdateRun{state: update.State{Outcome: update.OutcomeOK}}
	deps := update.Deps{Clock: updateTestClock{}, Store: &update.Store{Dir: dir}}
	seams := updateTestSeams(townRoot, deps, rec.call)

	code, _, stderr := runUpdateCommand(t, context.Background(), seams)
	if code != 5 {
		t.Fatalf("exit = %d with the lock held, want 5", code)
	}
	if rec.calls != 0 {
		t.Errorf("Run called %d times with the lock held, want 0", rec.calls)
	}
	for _, want := range []string{"run-holder", string(update.StepInstall), started.Format(time.RFC3339)} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not print %q:\n%s", want, stderr)
		}
	}
}

// TestUpdateLockedWithoutStateStillExitsFive proves a holder whose state
// cannot be read is named as unreadable, not mistaken for no run.
func TestUpdateLockedWithoutStateStillExitsFive(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	dir := update.Dir(townRoot)
	release, err := update.AcquireLock(dir)
	if err != nil {
		t.Fatalf("holding the lock: %v", err)
	}
	defer release()

	deps := update.Deps{Clock: updateTestClock{}, Store: &update.Store{Dir: dir}}
	code, _, stderr := runUpdateCommand(t, context.Background(), updateTestSeams(townRoot, deps, nil))
	if code != 5 {
		t.Fatalf("exit = %d with the lock held, want 5", code)
	}
	if !strings.Contains(stderr, "state could not be read") {
		t.Errorf("stderr does not say the state was unreadable:\n%s", stderr)
	}
}

// TestUpdateCancelledContextIsInterrupted proves cancelling the command's
// context while the run waits ends it with outcome interrupted, status 130,
// and the lock released.
func TestUpdateCancelledContextIsInterrupted(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	finished := make(chan struct{})
	run := func(runCtx context.Context, _ update.Deps, _ update.Options) (update.State, error) {
		close(started)
		<-runCtx.Done()
		return update.State{V: 1, RunID: "run-cancel", Outcome: update.OutcomeInterrupted}, nil
	}
	go func() {
		select {
		case <-started:
			cancel()
		case <-finished:
		}
	}()
	seams := updateTestSeams(townRoot, updateTestDeps(t), run)

	code, stdout, stderr := runUpdateCommand(t, ctx, seams)
	close(finished)
	if code != 130 {
		t.Fatalf("exit = %d after cancellation, want 130; stderr:\n%s", code, stderr)
	}
	if !strings.Contains(stdout, "outcome: interrupted") {
		t.Errorf("report does not print the interrupted outcome:\n%s", stdout)
	}
	release, err := update.AcquireLock(update.Dir(townRoot))
	if err != nil {
		t.Fatalf("lock still held after the interrupted run: %v", err)
	}
	release()
}

// TestUpdateFromDaemonSetsSource proves the source flag reaches the run:
// manual by default, daemon with --from-daemon.
func TestUpdateFromDaemonSetsSource(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"manual", nil, update.SourceManual},
		{"daemon", []string{"--from-daemon"}, update.SourceDaemon},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordingUpdateRun{state: update.State{Outcome: update.OutcomeOK}}
			seams := updateTestSeams(t.TempDir(), updateTestDeps(t), rec.call)
			code, _, stderr := runUpdateCommand(t, context.Background(), seams, tc.args...)
			if code != 0 {
				t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
			}
			if rec.opts.Source != tc.want {
				t.Errorf("Source = %q, want %q", rec.opts.Source, tc.want)
			}
		})
	}
}

// TestUpdateJSONPrintsTheState proves --json replaces the text report with the
// final state.
func TestUpdateJSONPrintsTheState(t *testing.T) {
	t.Parallel()
	rec := &recordingUpdateRun{state: update.State{V: 1, RunID: "run-json", Outcome: update.OutcomeOK}}
	seams := updateTestSeams(t.TempDir(), updateTestDeps(t), rec.call)

	code, stdout, stderr := runUpdateCommand(t, context.Background(), seams, "--json")
	if code != 0 {
		t.Fatalf("exit = %d, want 0; stderr:\n%s", code, stderr)
	}
	var st update.State
	if err := json.Unmarshal([]byte(stdout), &st); err != nil {
		t.Fatalf("stdout is not the state JSON: %v\n%s", err, stdout)
	}
	if st.RunID != "run-json" || st.Outcome != update.OutcomeOK {
		t.Errorf("decoded state = %+v, want run-json/ok", st)
	}
	if strings.Contains(stdout, "outcome: ") {
		t.Errorf("--json printed the text report as well:\n%s", stdout)
	}
}

// TestUpdateHeartbeatIgnoresNoCurrentAndLogsOtherWrites proves the heartbeat's
// error feed: a missing current state is silent, and any other write failure
// is printed.
func TestUpdateHeartbeatIgnoresNoCurrentAndLogsOtherWrites(t *testing.T) {
	t.Parallel()
	t.Run("no current state is silent", func(t *testing.T) {
		store := &update.Store{Dir: t.TempDir()}
		var buf bytes.Buffer
		got := make(chan error, 1)
		ticks := make(chan time.Time, 1)
		stop := startUpdateHeartbeat(context.Background(), store, ticks, updateTestClock{}, func(err error) {
			updateHeartbeatError(&buf)(err)
			got <- err
		})
		ticks <- time.Now()
		err := <-got
		stop()
		if !errors.Is(err, update.ErrNoCurrent) {
			t.Fatalf("heartbeat error = %v, want ErrNoCurrent", err)
		}
		if buf.Len() != 0 {
			t.Errorf("heartbeat printed %q for a missing current state", buf.String())
		}
	})

	t.Run("another write failure is printed", func(t *testing.T) {
		dir := t.TempDir()
		store := &update.Store{Dir: filepath.Join(dir, "state")}
		if err := store.Write(update.State{V: 1}); err != nil {
			t.Fatalf("writing the first state: %v", err)
		}
		// Replace the state directory with a file, so the next publish cannot
		// recreate it.
		if err := os.RemoveAll(store.Dir); err != nil {
			t.Fatalf("removing the state directory: %v", err)
		}
		if err := os.WriteFile(store.Dir, nil, 0o600); err != nil {
			t.Fatalf("blocking the state directory: %v", err)
		}

		var buf bytes.Buffer
		got := make(chan error, 1)
		ticks := make(chan time.Time, 1)
		stop := startUpdateHeartbeat(context.Background(), store, ticks, updateTestClock{}, func(err error) {
			updateHeartbeatError(&buf)(err)
			got <- err
		})
		ticks <- time.Now()
		err := <-got
		stop()
		if err == nil || errors.Is(err, update.ErrNoCurrent) {
			t.Fatalf("heartbeat error = %v, want a write failure", err)
		}
		if !strings.Contains(buf.String(), "heartbeat:") {
			t.Errorf("heartbeat failure not printed:\n%s", buf.String())
		}
	})
}

// TestUpdateLockErrorExitsFailed proves a lock that cannot be taken at all
// (rather than one another run holds) ends as a failed run before the
// orchestrator starts.
func TestUpdateLockErrorExitsFailed(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	// A regular file where the runtime directory belongs makes creating the
	// update directory fail.
	if err := os.WriteFile(filepath.Join(townRoot, ".runtime"), nil, 0o600); err != nil {
		t.Fatalf("blocking the runtime directory: %v", err)
	}
	rec := &recordingUpdateRun{state: update.State{Outcome: update.OutcomeOK}}
	seams := updateTestSeams(townRoot, updateTestDeps(t), rec.call)

	code, _, stderr := runUpdateCommand(t, context.Background(), seams)
	if code != 1 {
		t.Fatalf("exit = %d when the lock cannot be taken, want 1; stderr:\n%s", code, stderr)
	}
	if rec.calls != 0 {
		t.Errorf("Run called %d times, want 0", rec.calls)
	}
	if !strings.Contains(stderr, "taking the update lock") {
		t.Errorf("stderr does not name the lock failure:\n%s", stderr)
	}
}

// failingUpdateWriter is the writer that reports the JSON report's write
// failure.
type failingUpdateWriter struct{}

func (failingUpdateWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// TestUpdateWriteJSONReportsItsFailures proves the JSON report returns the
// encoding and the write failure rather than printing a partial record.
func TestUpdateWriteJSONReportsItsFailures(t *testing.T) {
	t.Parallel()
	t.Run("encoding", func(t *testing.T) {
		err := writeUpdateJSON(&bytes.Buffer{}, func() {})
		if err == nil || !strings.Contains(err.Error(), "encoding the update state") {
			t.Fatalf("writeUpdateJSON(func) = %v, want the encoding error", err)
		}
	})
	t.Run("write", func(t *testing.T) {
		err := writeUpdateJSON(failingUpdateWriter{}, update.State{V: 1})
		if err == nil || !strings.Contains(err.Error(), "disk full") {
			t.Fatalf("writeUpdateJSON(failing writer) = %v, want the write error", err)
		}
	})
}

// TestUpdateRealDepsBuildsTheTown proves the production factory refuses a town
// with no source checkout or an unparseable daemon config, and wires the real
// effects for a town that has its checkout.
func TestUpdateRealDepsBuildsTheTown(t *testing.T) {
	t.Parallel()
	t.Run("no source checkout", func(t *testing.T) {
		if _, err := realUpdateDeps(t.TempDir()); err == nil {
			t.Fatal("realUpdateDeps(town with no source) = nil error, want a refusal")
		}
	})

	t.Run("unparseable daemon config", func(t *testing.T) {
		townRoot := townWithGtSource(t)
		path := config.DaemonPatrolConfigPath(townRoot)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("creating the config directory: %v", err)
		}
		if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
			t.Fatalf("writing the malformed config: %v", err)
		}
		if _, err := realUpdateDeps(townRoot); err == nil {
			t.Fatal("realUpdateDeps(unparseable config) = nil error, want a refusal")
		}
	})

	t.Run("wired", func(t *testing.T) {
		townRoot := townWithGtSource(t)
		deps, err := realUpdateDeps(townRoot)
		if err != nil {
			t.Fatalf("realUpdateDeps() error = %v", err)
		}
		if deps.Store == nil || deps.Store.Dir != update.Dir(townRoot) {
			t.Errorf("Store = %+v, want Dir %q", deps.Store, update.Dir(townRoot))
		}
		if deps.Probes == nil || deps.Seats == nil || deps.Hold == nil || deps.Installer == nil ||
			deps.Maint == nil || deps.Verify == nil || deps.Clock == nil || deps.Sleep == nil || deps.NewRunID == nil {
			t.Fatalf("realUpdateDeps() left a nil effect: %+v", deps)
		}
		if deps.Log == nil {
			t.Error("Log is nil; the run's plan and notes would be discarded")
		}
		if id := deps.NewRunID(); id == "" {
			t.Error("NewRunID() = \"\", want a run id")
		}
		if deps.Clock.Now().IsZero() {
			t.Error("Clock.Now() is zero, want the real time")
		}
	})
}

// TestUpdateExclusiveFileRefusesAnExistingPath proves the os-backed exclusive
// create leaves an existing file alone, and the os-backed Stat reports a
// missing path as such.
func TestUpdateExclusiveFileRefusesAnExistingPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "hold")
	if err := createExclusiveFile(path, []byte("first"), 0o600); err != nil {
		t.Fatalf("createExclusiveFile() error = %v", err)
	}
	if err := createExclusiveFile(path, []byte("second"), 0o600); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("createExclusiveFile(existing) = %v, want fs.ErrExist", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the file: %v", err)
	}
	if string(data) != "first" {
		t.Errorf("file = %q, want the first write kept", data)
	}

	if err := (updateFiles{}).Stat(path); err != nil {
		t.Errorf("Stat(existing) = %v, want nil", err)
	}
	if err := (updateFiles{}).Stat(filepath.Join(dir, "absent")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat(missing) = %v, want fs.ErrNotExist", err)
	}
}

// fakeExclusiveFile is exclusiveFile with chosen write and close results.
type fakeExclusiveFile struct {
	writeErr, closeErr error
	closed             int
}

func (f *fakeExclusiveFile) Write([]byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return 0, nil
}

func (f *fakeExclusiveFile) Close() error {
	f.closed++
	return f.closeErr
}

// TestUpdateWriteExclusiveReportsWriteAndCloseFailures proves each failure of
// the exclusive write is returned, and that the handle is closed even when
// the write failed.
func TestUpdateWriteExclusiveReportsWriteAndCloseFailures(t *testing.T) {
	t.Parallel()
	writeErr := errors.New("write refused")
	closeErr := errors.New("close refused")
	cases := []struct {
		name string
		f    *fakeExclusiveFile
		want string
	}{
		{"write", &fakeExclusiveFile{writeErr: writeErr}, writeErr.Error()},
		{"write and close", &fakeExclusiveFile{writeErr: writeErr, closeErr: closeErr}, "close file: close refused"},
		{"close", &fakeExclusiveFile{closeErr: closeErr}, closeErr.Error()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			open := func(string, int, os.FileMode) (exclusiveFile, error) { return tc.f, nil }
			err := writeExclusive(open, "path", []byte("data"), 0o600)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("writeExclusive() = %v, want %q", err, tc.want)
			}
			if tc.f.closed != 1 {
				t.Errorf("handle closed %d times, want 1", tc.f.closed)
			}
		})
	}
}
