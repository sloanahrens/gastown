package ominstaller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/installlock"
)

const (
	commitNew = "0123456789abcdef0123456789abcdef01234567"
	commitOld = "fedcba9876543210fedcba9876543210fedcba98"
	// fixedTS is fixedNow in the receipt's timestamp format.
	fixedTS = "2026-10-30T12:00:00Z"
)

// fixedNow is the clock every test runs on, so a receipt's timestamp and
// duration are known before the run starts.
var fixedNow = time.Date(2026, time.October, 30, 12, 0, 0, 0, time.UTC)

// fakeAcquire is an Acquire seam that grants the lock at once, or fails or
// panics as the test sets it up, and records the call.
type fakeAcquire struct {
	err       error
	panicWith any

	path     string
	wait     time.Duration
	released bool
}

func (f *fakeAcquire) acquire(path string, wait time.Duration) (func(), error) {
	f.path, f.wait = path, wait
	if f.panicWith != nil {
		panic(f.panicWith)
	}
	if f.err != nil {
		return nil, f.err
	}
	return func() { f.released = true }, nil
}

// testConfig is a run's config over a temporary daemon directory, with the lock
// answered by acq. No real process runs and no real time passes.
func testConfig(t *testing.T, acq *fakeAcquire) Config {
	t.Helper()
	return Config{
		DaemonDir: t.TempDir(),
		Source:    "manual",
		LockWait:  300 * time.Second,
		Acquire:   acq.acquire,
		Now:       func() time.Time { return fixedNow },
	}
}

func TestResultLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		result Result
		want   string
	}{
		{
			name:   "installed",
			result: Result{Event: "installed", Commit: commitNew, Prev: commitOld},
			want:   "install-om: RESULT installed " + commitNew + " " + commitOld + " -",
		},
		{
			name:   "failed with a reason",
			result: Result{Event: "failed", Commit: commitNew, Prev: commitOld, Reason: "unexpected"},
			want:   "install-om: RESULT failed " + commitNew + " " + commitOld + " unexpected",
		},
		{
			name:   "refused for a dirty clone",
			result: Result{Event: "refused", Commit: commitNew, Prev: commitOld, Reason: "dirty"},
			want:   "install-om: RESULT refused " + commitNew + " " + commitOld + " dirty",
		},
		{
			name:   "refused for a busy lock with empty commit and prev",
			result: Result{Event: "refused", Reason: "lock-busy"},
			want:   "install-om: RESULT refused - - lock-busy",
		},
		{
			name:   "event only prints every empty field as a dash",
			result: Result{Event: "failed"},
			want:   "install-om: RESULT failed - - -",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.result.Line(); got != tc.want {
				t.Errorf("Line() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRunRecoversAPanicAsFailedUnexpected(t *testing.T) {
	t.Parallel()
	acq := &fakeAcquire{panicWith: "seam exploded"}

	res, exit := Run(context.Background(), testConfig(t, acq))

	if exit != ExitFailed {
		t.Errorf("exit = %d, want %d", exit, ExitFailed)
	}
	if want := (Result{Event: "failed", Reason: "unexpected"}); res != want {
		t.Errorf("result = %+v, want %+v", res, want)
	}
}

func TestRunLockErrorsEndTheRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		err        error
		wantResult Result
		wantExit   Exit
	}{
		{
			name:       "busy lock is refused and exits busy",
			err:        installlock.ErrBusy,
			wantResult: Result{Event: "refused", Reason: "lock-busy"},
			wantExit:   ExitBusy,
		},
		{
			name:       "other lock failure is failed and unexpected",
			err:        errors.New("open lock: permission denied"),
			wantResult: Result{Event: "failed", Reason: "unexpected"},
			wantExit:   ExitFailed,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, exit := Run(context.Background(), testConfig(t, &fakeAcquire{err: tc.err}))

			if exit != tc.wantExit {
				t.Errorf("exit = %d, want %d", exit, tc.wantExit)
			}
			if res != tc.wantResult {
				t.Errorf("result = %+v, want %+v", res, tc.wantResult)
			}
		})
	}
}

func TestRunHoldsTheInstallLockUnderTheDaemonDir(t *testing.T) {
	t.Parallel()
	acq := &fakeAcquire{}
	cfg := testConfig(t, acq)

	Run(context.Background(), cfg)

	if want := filepath.Join(cfg.DaemonDir, "install-om.lock"); acq.path != want {
		t.Errorf("lock path = %q, want %q", acq.path, want)
	}
	if acq.wait != cfg.LockWait {
		t.Errorf("lock wait = %v, want %v", acq.wait, cfg.LockWait)
	}
	if !acq.released {
		t.Error("lock was not released when Run returned")
	}
}

func TestRunAppendsOneReceiptForItsOutcome(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		acq  *fakeAcquire
		want string
	}{
		{
			name: "busy lock",
			acq:  &fakeAcquire{err: installlock.ErrBusy},
			want: `{"commit": null, "duration_s": 0, "event": "refused", "prev_commit": null, "reason": "lock-busy", "source": "manual", "ts": "` + fixedTS + `"}` + "\n",
		},
		{
			name: "unexpected panic",
			acq:  &fakeAcquire{panicWith: "boom"},
			want: `{"commit": null, "duration_s": 0, "event": "failed", "prev_commit": null, "reason": "unexpected", "source": "manual", "ts": "` + fixedTS + `"}` + "\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testConfig(t, tc.acq)

			Run(context.Background(), cfg)

			got, err := os.ReadFile(filepath.Join(cfg.DaemonDir, "install-om-receipts.jsonl"))
			if err != nil {
				t.Fatalf("read receipts: %v", err)
			}
			if string(got) != tc.want {
				t.Errorf("receipts = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReceiptLineMatchesTheScript(t *testing.T) {
	t.Parallel()
	// The goldens are the bytes scripts/install-om.sh's om_receipt appends:
	// Python's json.dumps with sort_keys, absent fields as null.
	tests := []struct {
		name string
		rec  receipt
		want string
	}{
		{
			name: "installed",
			rec:  receipt{event: "installed", commit: commitNew, prev: commitOld, source: "manual"},
			want: `{"commit": "` + commitNew + `", "duration_s": null, "event": "installed", "prev_commit": "` + commitOld + `", "reason": null, "source": "manual", "ts": "` + fixedTS + `"}` + "\n",
		},
		{
			name: "failed with a reason",
			rec:  receipt{event: "failed", commit: commitNew, source: "rebuild-om", reason: "unexpected"},
			want: `{"commit": "` + commitNew + `", "duration_s": null, "event": "failed", "prev_commit": null, "reason": "unexpected", "source": "rebuild-om", "ts": "` + fixedTS + `"}` + "\n",
		},
		{
			name: "nothing to do",
			rec:  receipt{event: "noop", commit: commitNew, prev: commitNew, source: "manual", reason: "already-installed"},
			want: `{"commit": "` + commitNew + `", "duration_s": null, "event": "noop", "prev_commit": "` + commitNew + `", "reason": "already-installed", "source": "manual", "ts": "` + fixedTS + `"}` + "\n",
		},
		{
			name: "reason is escaped as Python escapes it",
			rec:  receipt{event: "failed", source: "manual", reason: `say "hi" \ é`},
			want: `{"commit": null, "duration_s": null, "event": "failed", "prev_commit": null, "reason": "say \"hi\" \\ \u00e9", "source": "manual", "ts": "` + fixedTS + `"}` + "\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := receiptLine(tc.rec, fixedNow); got != tc.want {
				t.Errorf("receiptLine() =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

func TestReceiptDurationIsWholeSecondsSinceTheStart(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		start time.Time
		now   time.Time
		want  string
	}{
		{
			name:  "whole seconds",
			start: fixedNow.Add(-42 * time.Second),
			now:   fixedNow,
			want:  "42",
		},
		{
			name:  "a fraction of a second is dropped",
			start: fixedNow.Add(-42 * time.Second),
			now:   fixedNow.Add(300 * time.Millisecond),
			want:  "42",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			line := receiptLine(receipt{event: "installed", start: tc.start}, tc.now)

			if want := `"duration_s": ` + tc.want + ","; !strings.Contains(line, want) {
				t.Errorf("receipt %q has no %q", line, want)
			}
		})
	}
}

func TestAReceiptWriteFailureIsLoggedNotFatal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		blocked    func(t *testing.T) string
		acq        *fakeAcquire
		wantResult Result
		wantExit   Exit
	}{
		{
			name:       "unexpected, daemon directory is a file",
			blocked:    fileBlocksDaemonDir,
			acq:        &fakeAcquire{panicWith: "boom"},
			wantResult: Result{Event: "failed", Reason: "unexpected"},
			wantExit:   ExitFailed,
		},
		{
			name:       "unexpected, receipts path is a directory",
			blocked:    dirBlocksReceipts,
			acq:        &fakeAcquire{panicWith: "boom"},
			wantResult: Result{Event: "failed", Reason: "unexpected"},
			wantExit:   ExitFailed,
		},
		{
			name:       "busy lock, daemon directory is a file",
			blocked:    fileBlocksDaemonDir,
			acq:        &fakeAcquire{err: installlock.ErrBusy},
			wantResult: Result{Event: "refused", Reason: "lock-busy"},
			wantExit:   ExitBusy,
		},
		{
			name:       "busy lock, receipts path is a directory",
			blocked:    dirBlocksReceipts,
			acq:        &fakeAcquire{err: installlock.ErrBusy},
			wantResult: Result{Event: "refused", Reason: "lock-busy"},
			wantExit:   ExitBusy,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var logged []string
			cfg := testConfig(t, tc.acq)
			cfg.DaemonDir = tc.blocked(t)
			cfg.Logf = func(format string, args ...any) {
				logged = append(logged, fmt.Sprintf(format, args...))
			}

			res, exit := Run(context.Background(), cfg)

			if res != tc.wantResult {
				t.Errorf("result = %+v, want %+v", res, tc.wantResult)
			}
			if exit != tc.wantExit {
				t.Errorf("exit = %d, want %d", exit, tc.wantExit)
			}
			if !strings.Contains(strings.Join(logged, "\n"), "could not append a receipt") {
				t.Errorf("failed receipt write was not logged: %q", logged)
			}
		})
	}
}

// fileBlocksDaemonDir makes the daemon directory a regular file, so the receipts
// directory cannot be created.
func fileBlocksDaemonDir(t *testing.T) string {
	t.Helper()
	blocker := filepath.Join(t.TempDir(), "daemon")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	return blocker
}

// dirBlocksReceipts makes the receipts path a directory, so the receipts file
// cannot be opened for append.
func dirBlocksReceipts(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "install-om-receipts.jsonl"), 0o755); err != nil {
		t.Fatalf("make blocker: %v", err)
	}
	return dir
}
