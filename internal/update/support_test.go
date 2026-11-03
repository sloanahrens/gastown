package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

// fakeClock reports the times it was given in order, one per Now call, so a
// test knows exactly which heartbeat each tick should hold.
type fakeClock struct {
	mu    sync.Mutex
	times []time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.times) == 0 {
		return time.Time{}
	}
	now := c.times[0]
	c.times = c.times[1:]
	return now
}

// captureWrites makes store send every state it stages to the returned
// channel, in publication order.
func captureWrites(t *testing.T, store *Store) <-chan State {
	t.Helper()
	written := make(chan State, 8)
	store.write = func(path string, data []byte, perm os.FileMode) error {
		var st State
		if err := json.Unmarshal(data, &st); err != nil {
			return err
		}
		select {
		case written <- st:
		case <-time.After(waitBound):
			t.Errorf("the test never read the write for run %s within %v", st.RunID, waitBound)
		}
		return atomicfile.WriteFile(path, data, perm)
	}
	return written
}

// waitBound bounds every channel wait a heartbeat test makes, so a regression
// that stops a write or an error callback fails the test instead of hanging it.
const waitBound = 2 * time.Second

// receiveWithin takes one value from ch, failing the test with what it was
// waiting for when nothing arrives within waitBound.
func receiveWithin[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(waitBound):
		t.Fatalf("nothing arrived within %v: %s", waitBound, what)
	}
	var zero T
	return zero
}

// sendWithin sends v on ch, failing the test when nothing takes it within
// waitBound and naming what never arrived.
func sendWithin[T any](t *testing.T, ch chan<- T, v T, what string) {
	t.Helper()
	select {
	case ch <- v:
	case <-time.After(waitBound):
		t.Fatalf("nothing took %s within %v", what, waitBound)
	}
}

// reportError hands err to the test goroutine from inside RunHeartbeat without
// blocking the heartbeat loop; a full channel fails the test instead of
// hanging the loop.
func reportError(t *testing.T, errs chan<- error, err error) {
	t.Helper()
	select {
	case errs <- err:
	case <-time.After(waitBound):
		t.Errorf("the test never read the reported error %v within %v", err, waitBound)
	}
}

// startHeartbeat runs RunHeartbeat in a goroutine and registers a cleanup that
// cancels it and waits for it, so a failure midway cannot leave the loop
// running past the test. The returned channel closes when RunHeartbeat returns.
func startHeartbeat(t *testing.T, store *Store, ticks <-chan time.Time, clock Clock, onError func(error)) (context.CancelFunc, <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		RunHeartbeat(ctx, store, ticks, clock, onError)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(waitBound):
			t.Errorf("RunHeartbeat did not return within %v after cancellation", waitBound)
		}
	})
	return cancel, finished
}

func TestHeartbeatRewritesOnEveryTick(t *testing.T) {
	t.Parallel()
	store := &Store{Dir: filepath.Join(t.TempDir(), "update")}
	base := fullState()
	if err := store.Write(base); err != nil {
		t.Fatalf("Write: %v", err)
	}
	written := captureWrites(t, store)

	ticks := make([]time.Time, 3)
	for i := range ticks {
		ticks[i] = base.HeartbeatAt.Add(time.Duration(i+1) * time.Minute)
	}
	clock := &fakeClock{times: ticks}

	tickCh := make(chan time.Time)
	cancel, done := startHeartbeat(t, store, tickCh, clock, nil)

	for i, want := range ticks {
		sendWithin(t, tickCh, time.Now(), "a heartbeat tick")
		got := receiveWithin(t, written, fmt.Sprintf("heartbeat write %d", i))
		if !got.HeartbeatAt.Equal(want) {
			t.Errorf("write %d HeartbeatAt = %v, want %v", i, got.HeartbeatAt, want)
		}
		// Everything but the heartbeat must be exactly what the run wrote.
		got.HeartbeatAt = base.HeartbeatAt
		assertSameState(t, got, base)
	}

	cancel()
	receiveWithin(t, done, "RunHeartbeat to return after ctx cancellation")
	select {
	case st := <-written:
		t.Errorf("heartbeat wrote after ctx ended: %+v", st)
	default:
	}

	published, err := store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !published.HeartbeatAt.Equal(ticks[len(ticks)-1]) {
		t.Errorf("published HeartbeatAt = %v, want the last tick's %v", published.HeartbeatAt, ticks[len(ticks)-1])
	}
}

func TestHeartbeatStopsWithContextAndReportsErrors(t *testing.T) {
	t.Parallel()

	t.Run("publish failure is reported and the loop continues", func(t *testing.T) {
		t.Parallel()
		store := &Store{Dir: filepath.Join(t.TempDir(), "update")}
		if err := store.Write(fullState()); err != nil {
			t.Fatalf("Write: %v", err)
		}
		var writes atomic.Int32
		store.write = func(path string, data []byte, perm os.FileMode) error {
			writes.Add(1)
			return atomicfile.WriteFile(path, data, perm)
		}
		store.Rename = func(oldpath, newpath string) error {
			return errors.New("rename refused")
		}

		errs := make(chan error, 2)
		ticks := make(chan time.Time)
		cancel, done := startHeartbeat(t, store, ticks,
			&fakeClock{times: []time.Time{time.Now(), time.Now()}},
			func(err error) { reportError(t, errs, err) })

		for i := 0; i < 2; i++ {
			sendWithin(t, ticks, time.Now(), "a heartbeat tick")
			err := receiveWithin(t, errs, fmt.Sprintf("the publish error for tick %d", i))
			if err == nil {
				t.Fatal("onError got nil for a failed publish")
			} else if !strings.Contains(err.Error(), "publish update state") {
				t.Errorf("reported error %q does not name the publish", err)
			}
		}

		cancel()
		receiveWithin(t, done, "RunHeartbeat to return after ctx cancellation")
		if got := writes.Load(); got != 2 {
			t.Errorf("staged %d writes, want 2: the loop stopped on the first error", got)
		}
	})

	t.Run("no state yet is reported and the loop continues", func(t *testing.T) {
		t.Parallel()
		store := &Store{Dir: filepath.Join(t.TempDir(), "update")}
		errs := make(chan error, 2)
		ticks := make(chan time.Time)
		cancel, done := startHeartbeat(t, store, ticks,
			&fakeClock{times: []time.Time{time.Now(), time.Now()}},
			func(err error) { reportError(t, errs, err) })

		for i := 0; i < 2; i++ {
			sendWithin(t, ticks, time.Now(), "a heartbeat tick")
			err := receiveWithin(t, errs, fmt.Sprintf("the ErrNoCurrent report for tick %d", i))
			if !errors.Is(err, ErrNoCurrent) {
				t.Fatalf("onError got %v, want ErrNoCurrent", err)
			}
		}

		cancel()
		receiveWithin(t, done, "RunHeartbeat to return after ctx cancellation")
	})

	t.Run("closed ticks end the loop", func(t *testing.T) {
		t.Parallel()
		store := &Store{Dir: filepath.Join(t.TempDir(), "update")}
		ticks := make(chan time.Time)
		close(ticks)
		_, done := startHeartbeat(t, store, ticks, &fakeClock{}, func(error) {})
		receiveWithin(t, done, "RunHeartbeat to return when ticks is closed")
	})
}

func TestLockExcludesASecondHolder(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "update")
	release, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("first AcquireLock: %v", err)
	}

	secondRelease, err := AcquireLock(dir)
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("second AcquireLock = %v, want ErrLocked", err)
	}
	if secondRelease != nil {
		t.Error("second AcquireLock returned a release for a lock it did not take")
	}

	release()
	release() // twice must not panic

	thirdRelease, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("AcquireLock after release: %v", err)
	}
	thirdRelease()
}

func TestAcquireLockCreatesTheDirectoryOwnerOnly(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "update")
	release, err := AcquireLock(dir)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	defer release()

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat update directory: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("update directory mode = %o, want 0700", got)
	}
}

func TestAcquireLockRefusesAFileWhereTheDirectoryBelongs(t *testing.T) {
	t.Parallel()
	blocked := filepath.Join(t.TempDir(), "update")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("block the update directory: %v", err)
	}

	release, err := AcquireLock(blocked)
	if err == nil {
		release()
		t.Fatal("AcquireLock succeeded with a file where the directory belongs")
	}
	if errors.Is(err, ErrLocked) {
		t.Errorf("AcquireLock = %v, want the directory error, not ErrLocked", err)
	}
}

func TestAcquireLockReportsALockItCannotOpen(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "update")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create update directory: %v", err)
	}
	// A symlink to itself cannot be opened, so the flock cannot be taken.
	if err := os.Symlink(lockFileName, filepath.Join(dir, lockFileName)); err != nil {
		t.Fatalf("make the lock path unopenable: %v", err)
	}

	release, err := AcquireLock(dir)
	if err == nil {
		release()
		t.Fatal("AcquireLock succeeded on an unopenable lock path")
	}
	if errors.Is(err, ErrLocked) {
		t.Errorf("AcquireLock = %v, want the open error, not ErrLocked", err)
	}
}

func TestAppendHistoryRecordsDurations(t *testing.T) {
	t.Parallel()
	store := &Store{Dir: filepath.Join(t.TempDir(), "update")}
	historyPath := filepath.Join(store.Dir, historyFileName)

	running := fullState()
	running.Outcome = OutcomeRunning
	if err := store.AppendHistory(running); err != nil {
		t.Fatalf("AppendHistory(running): %v", err)
	}
	if _, err := os.Stat(historyPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a running run wrote history: stat = %v", err)
	}

	// The step times, and therefore the expected durations below, are written
	// here rather than taken from another file's fixture.
	start := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	steps := []StepRecord{
		{Name: StepPlan, Result: ResultOK, StartedAt: start, EndedAt: start.Add(1500 * time.Millisecond)},
		{Name: StepDrain, Result: ResultOK, StartedAt: start.Add(time.Second), EndedAt: start.Add(4 * time.Second)},
		// maintenance has no times, so it is absent from durations_ms.
		{Name: StepMaintenance, Result: ResultDeferred},
	}
	finished := fullState()
	finished.Steps = steps
	if err := store.AppendHistory(finished); err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}
	second := fullState()
	second.RunID = "20261103T110000Z-furiosa"
	second.Outcome = OutcomeOK
	second.Steps = steps
	if err := store.AppendHistory(second); err != nil {
		t.Fatalf("second AppendHistory: %v", err)
	}

	lines := readHistoryLines(t, historyPath)
	if len(lines) != 2 {
		t.Fatalf("history has %d lines, want 2", len(lines))
	}
	wantDurations := map[string]int64{
		string(StepPlan):  1500,
		string(StepDrain): 3000,
	}
	for i, want := range []State{finished, second} {
		if !reflect.DeepEqual(lines[i].DurationsMS, wantDurations) {
			t.Errorf("line %d durations_ms = %v, want %v", i, lines[i].DurationsMS, wantDurations)
		}
		assertSameState(t, lines[i].State, want)
	}
}

// TestHistoryDurationsBuildExpectedFromOwnSteps checks the durations_ms
// arithmetic against step times this test writes itself.
func TestHistoryDurationsBuildExpectedFromOwnSteps(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		steps []StepRecord
		want  map[string]int64
	}{
		{
			name: "a step with both times has the gap in milliseconds",
			steps: []StepRecord{
				{Name: StepPlan, StartedAt: start, EndedAt: start.Add(2500 * time.Millisecond)},
				{Name: StepDrain, StartedAt: start.Add(time.Second), EndedAt: start.Add(9 * time.Second)},
			},
			want: map[string]int64{string(StepPlan): 2500, string(StepDrain): 8000},
		},
		{
			name:  "a step with only a start has no duration",
			steps: []StepRecord{{Name: StepInstall, StartedAt: start}},
			want:  map[string]int64{},
		},
		{
			name:  "a step with only an end has no duration",
			steps: []StepRecord{{Name: StepVerify, EndedAt: start.Add(time.Minute)}},
			want:  map[string]int64{},
		},
		{
			name:  "a sub-millisecond gap is zero milliseconds",
			steps: []StepRecord{{Name: StepRestart, StartedAt: start, EndedAt: start.Add(500 * time.Microsecond)}},
			want:  map[string]int64{string(StepRestart): 0},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			store := &Store{Dir: filepath.Join(t.TempDir(), "update")}
			st := fullState()
			st.Outcome = OutcomeOK
			st.Steps = tt.steps
			if err := store.AppendHistory(st); err != nil {
				t.Fatalf("AppendHistory: %v", err)
			}

			lines := readHistoryLines(t, filepath.Join(store.Dir, historyFileName))
			if len(lines) != 1 {
				t.Fatalf("history has %d lines, want 1", len(lines))
			}
			if !reflect.DeepEqual(lines[0].DurationsMS, tt.want) {
				t.Errorf("durations_ms = %v, want %v", lines[0].DurationsMS, tt.want)
			}
		})
	}
}

func TestAppendHistoryCreatesTheFileOwnerOnly(t *testing.T) {
	t.Parallel()
	store := &Store{Dir: filepath.Join(t.TempDir(), "update")}
	if err := store.AppendHistory(fullState()); err != nil {
		t.Fatalf("AppendHistory: %v", err)
	}

	info, err := os.Stat(filepath.Join(store.Dir, historyFileName))
	if err != nil {
		t.Fatalf("stat history.jsonl: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("history.jsonl mode = %o, want 0600", got)
	}
}

func TestAppendHistoryRefusesADirectoryItCannotCreate(t *testing.T) {
	t.Parallel()
	blocked := filepath.Join(t.TempDir(), "update")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("block the update directory: %v", err)
	}

	if err := (&Store{Dir: blocked}).AppendHistory(fullState()); err == nil {
		t.Fatal("AppendHistory succeeded with a file where the directory belongs")
	}
}

func TestAppendHistoryReportsAnUnopenableHistoryFile(t *testing.T) {
	t.Parallel()
	store := &Store{Dir: filepath.Join(t.TempDir(), "update")}
	if err := os.MkdirAll(filepath.Join(store.Dir, historyFileName), 0o700); err != nil {
		t.Fatalf("make history.jsonl a directory: %v", err)
	}

	err := store.AppendHistory(fullState())
	if err == nil {
		t.Fatal("AppendHistory succeeded on a directory")
	}
	if !strings.Contains(err.Error(), "open update history") {
		t.Errorf("error %q does not name the open", err)
	}
}

func TestAppendHistoryRefusesATimeItCannotEncode(t *testing.T) {
	t.Parallel()
	st := fullState()
	st.StartedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)

	err := (&Store{Dir: t.TempDir()}).AppendHistory(st)
	if err == nil {
		t.Fatal("AppendHistory encoded a year outside the RFC 3339 range")
	}
	if !strings.Contains(err.Error(), "encode update history") {
		t.Errorf("error %q does not name the encoding", err)
	}
}

// stubHistoryFile is an open history file whose write or close can fail.
type stubHistoryFile struct {
	writeErr error
	closeErr error
}

func (f *stubHistoryFile) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return len(p), nil
}

func (f *stubHistoryFile) Close() error { return f.closeErr }

func TestAppendHistoryReportsAFailedWrite(t *testing.T) {
	t.Parallel()
	store := &Store{Dir: t.TempDir()}
	store.openHistory = func(string) (historyWriter, error) {
		return &stubHistoryFile{writeErr: errors.New("write refused")}, nil
	}

	err := store.AppendHistory(fullState())
	if err == nil {
		t.Fatal("AppendHistory succeeded with a failing write")
	}
	if !strings.Contains(err.Error(), "append update history") {
		t.Errorf("error %q does not name the append", err)
	}
}

func TestAppendHistoryReportsAFailedWriteAndClose(t *testing.T) {
	t.Parallel()
	store := &Store{Dir: t.TempDir()}
	store.openHistory = func(string) (historyWriter, error) {
		return &stubHistoryFile{
			writeErr: errors.New("write refused"),
			closeErr: errors.New("close refused"),
		}, nil
	}

	err := store.AppendHistory(fullState())
	if err == nil {
		t.Fatal("AppendHistory succeeded with a failing write and close")
	}
	if !strings.Contains(err.Error(), "close file") {
		t.Errorf("error %q does not name the close", err)
	}
}

func TestAppendHistoryReportsAFailedClose(t *testing.T) {
	t.Parallel()
	store := &Store{Dir: t.TempDir()}
	store.openHistory = func(string) (historyWriter, error) {
		return &stubHistoryFile{closeErr: errors.New("close refused")}, nil
	}

	err := store.AppendHistory(fullState())
	if err == nil {
		t.Fatal("AppendHistory succeeded with a failing close")
	}
	if !strings.Contains(err.Error(), "close update history") {
		t.Errorf("error %q does not name the close", err)
	}
}

// readHistoryLines parses history.jsonl, one JSON line per entry.
func readHistoryLines(t *testing.T, path string) []HistoryLine {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read history.jsonl: %v", err)
	}
	text := strings.TrimSuffix(string(data), "\n")
	if text == "" {
		return nil
	}
	raw := strings.Split(text, "\n")
	lines := make([]HistoryLine, len(raw))
	for i, line := range raw {
		if err := json.Unmarshal([]byte(line), &lines[i]); err != nil {
			t.Fatalf("parse history line %d: %v", i, err)
		}
	}
	return lines
}
