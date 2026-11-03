package update

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	maintTestDir     = "/town/.runtime/update"
	maintTestRunID   = "20261103T102200Z-capable"
	maintTestTimeout = 30 * time.Minute
	maintTestPoll    = 15 * time.Second
)

var (
	maintRequestFile = filepath.Join(maintTestDir, RequestFileName)
	maintResultFile  = filepath.Join(maintTestDir, ResultFileName)
)

// fakeMaintFiles is an in-memory MaintFiles. It implements only that
// interface, so a MaintenanceRequest that reached for any other file
// operation would not compile against it.
type fakeMaintFiles struct {
	mu        sync.Mutex
	files     map[string]maintFileEntry
	readErr   map[string]error
	createErr map[string]error
	removeErr map[string]error
}

type maintFileEntry struct {
	data []byte
	mode os.FileMode
}

func newFakeMaintFiles() *fakeMaintFiles {
	return &fakeMaintFiles{
		files:     map[string]maintFileEntry{},
		readErr:   map[string]error{},
		createErr: map[string]error{},
		removeErr: map[string]error{},
	}
}

func (f *fakeMaintFiles) ReadFile(path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.readErr[path]; err != nil {
		return nil, err
	}
	entry, ok := f.files[path]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	return append([]byte(nil), entry.data...), nil
}

func (f *fakeMaintFiles) CreateExclusive(path string, data []byte, mode os.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.createErr[path]; err != nil {
		return err
	}
	if _, ok := f.files[path]; ok {
		return &fs.PathError{Op: "open", Path: path, Err: fs.ErrExist}
	}
	f.files[path] = maintFileEntry{data: append([]byte(nil), data...), mode: mode}
	return nil
}

func (f *fakeMaintFiles) Remove(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.removeErr[path]; err != nil {
		return err
	}
	if _, ok := f.files[path]; !ok {
		return &fs.PathError{Op: "remove", Path: path, Err: fs.ErrNotExist}
	}
	delete(f.files, path)
	return nil
}

// put seeds a file, as a previous run or the daemon would have left it.
func (f *fakeMaintFiles) put(path string, data []byte, mode os.FileMode) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[path] = maintFileEntry{data: append([]byte(nil), data...), mode: mode}
}

// entry returns a copy of the file at path, and whether it exists.
func (f *fakeMaintFiles) entry(path string) (maintFileEntry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry, ok := f.files[path]
	return maintFileEntry{data: append([]byte(nil), entry.data...), mode: entry.mode}, ok
}

func putMaintJSON(t *testing.T, files *fakeMaintFiles, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode %s: %v", path, err)
	}
	files.put(path, data, 0o600)
}

// fakeMaintClock is a Clock whose sleep advances Now, so Wait finishes at once
// and the waits it made are on record. onSleep runs after the clock advances,
// so a test can let the daemon finish between two polls.
type fakeMaintClock struct {
	now     time.Time
	sleeps  []time.Duration
	onSleep func(n int)
}

func newFakeMaintClock(now time.Time) *fakeMaintClock {
	return &fakeMaintClock{now: now}
}

func (c *fakeMaintClock) Now() time.Time { return c.now }

func (c *fakeMaintClock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.sleeps = append(c.sleeps, d)
	c.now = c.now.Add(d)
	if c.onSleep != nil {
		c.onSleep(len(c.sleeps))
	}
	return ctx.Err()
}

func maintTestStart() time.Time {
	return time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
}

func newTestMaintenance(files MaintFiles, clock Clock) *MaintenanceRequest {
	return &MaintenanceRequest{Dir: maintTestDir, Files: files, Clock: clock}
}

func TestMaintenanceFileNames(t *testing.T) {
	t.Parallel()
	if RequestFileName != "maintenance-request" {
		t.Errorf("RequestFileName = %q, want %q", RequestFileName, "maintenance-request")
	}
	if ResultFileName != "maintenance-result.json" {
		t.Errorf("ResultFileName = %q, want %q", ResultFileName, "maintenance-result.json")
	}
}

func TestRequestWritesTheFileAndClearsAStaleResult(t *testing.T) {
	t.Parallel()
	files := newFakeMaintFiles()
	start := maintTestStart()
	putMaintJSON(t, files, maintResultFile, MaintenanceResult{
		RunID:   "earlier-run",
		Outcome: "completed",
		At:      start.Add(-time.Hour),
	})

	if err := newTestMaintenance(files, newFakeMaintClock(start)).Request(maintTestRunID); err != nil {
		t.Fatalf("Request: %v", err)
	}

	entry, ok := files.entry(maintRequestFile)
	if !ok {
		t.Fatalf("request file %s was not created", maintRequestFile)
	}
	if entry.mode != 0o600 {
		t.Errorf("request file mode = %o, want 600", entry.mode)
	}
	var req RequestFile
	if err := json.Unmarshal(entry.data, &req); err != nil {
		t.Fatalf("parse request file: %v", err)
	}
	if req.RunID != maintTestRunID {
		t.Errorf("request run id = %q, want %q", req.RunID, maintTestRunID)
	}
	if !req.At.Equal(start) {
		t.Errorf("request At = %v, want the clock's %v", req.At, start)
	}
	if _, ok := files.entry(maintResultFile); ok {
		t.Errorf("stale result file %s survived Request", maintResultFile)
	}
}

func TestRequestSameRunIsIdempotentAndAForeignRunIsRefused(t *testing.T) {
	t.Parallel()

	t.Run("same run id", func(t *testing.T) {
		files := newFakeMaintFiles()
		start := maintTestStart()
		clock := newFakeMaintClock(start)
		m := newTestMaintenance(files, clock)
		if err := m.Request(maintTestRunID); err != nil {
			t.Fatalf("first Request: %v", err)
		}
		first, ok := files.entry(maintRequestFile)
		if !ok {
			t.Fatalf("first Request did not create %s", maintRequestFile)
		}
		clock.now = start.Add(time.Minute)
		if err := m.Request(maintTestRunID); err != nil {
			t.Fatalf("second Request for the same run: %v", err)
		}
		second, ok := files.entry(maintRequestFile)
		if !ok {
			t.Fatalf("second Request removed %s", maintRequestFile)
		}
		if !bytes.Equal(second.data, first.data) {
			t.Errorf("second Request rewrote the request file: %q, want %q", second.data, first.data)
		}
	})

	t.Run("foreign run id", func(t *testing.T) {
		files := newFakeMaintFiles()
		putMaintJSON(t, files, maintRequestFile, RequestFile{RunID: "other-run", At: maintTestStart()})
		err := newTestMaintenance(files, newFakeMaintClock(maintTestStart())).Request(maintTestRunID)
		if err == nil {
			t.Fatal("Request over another run's request returned nil")
		}
		if !strings.Contains(err.Error(), "other-run") {
			t.Errorf("error %q does not name the run that holds the request", err)
		}
		if entry, ok := files.entry(maintRequestFile); !ok || !bytes.Contains(entry.data, []byte("other-run")) {
			t.Errorf("Request overwrote the other run's request file")
		}
	})
}

func TestRequestKeepsAnotherRunsResult(t *testing.T) {
	t.Parallel()
	files := newFakeMaintFiles()
	start := maintTestStart()
	putMaintJSON(t, files, maintRequestFile, RequestFile{RunID: "other-run", At: start})
	putMaintJSON(t, files, maintResultFile, MaintenanceResult{
		RunID:   "other-run",
		Outcome: "completed",
		At:      start,
		Detail:  "the other run's cycle",
	})
	before, ok := files.entry(maintResultFile)
	if !ok {
		t.Fatalf("seed did not write %s", maintResultFile)
	}

	err := newTestMaintenance(files, newFakeMaintClock(start)).Request(maintTestRunID)
	if err == nil {
		t.Fatal("Request over another run's request returned nil")
	}
	if !strings.Contains(err.Error(), "other-run") {
		t.Errorf("error %q does not name the run that holds the request", err)
	}
	after, ok := files.entry(maintResultFile)
	if !ok {
		t.Fatalf("Request removed the other run's result file %s", maintResultFile)
	}
	if !bytes.Equal(after.data, before.data) || after.mode != before.mode {
		t.Errorf("Request changed the other run's result file: got %q mode %o, want %q mode %o", after.data, after.mode, before.data, before.mode)
	}
}

func TestRequestCorruptRequestFileNamesTheFileToRemove(t *testing.T) {
	t.Parallel()
	files := newFakeMaintFiles()
	files.put(maintRequestFile, []byte("{"), 0o600)

	err := newTestMaintenance(files, newFakeMaintClock(maintTestStart())).Request(maintTestRunID)
	if err == nil {
		t.Fatal("Request over a malformed request file returned nil")
	}
	if !strings.Contains(err.Error(), maintRequestFile) {
		t.Errorf("error %q does not name %s", err, maintRequestFile)
	}
	if !strings.Contains(err.Error(), "remove") {
		t.Errorf("error %q does not say to remove the file", err)
	}
	if entry, ok := files.entry(maintRequestFile); !ok || !bytes.Equal(entry.data, []byte("{")) {
		t.Errorf("Request changed the malformed request file instead of reporting it")
	}
}

func TestRequestRefusesAnEmptyRunID(t *testing.T) {
	t.Parallel()
	files := newFakeMaintFiles()
	err := newTestMaintenance(files, newFakeMaintClock(maintTestStart())).Request("")
	if err == nil {
		t.Fatal("Request(\"\") returned nil")
	}
	if _, ok := files.entry(maintRequestFile); ok {
		t.Errorf("Request(\"\") wrote a request file")
	}
}

func TestRequestRefusesATimeThatCannotBeEncoded(t *testing.T) {
	t.Parallel()
	files := newFakeMaintFiles()
	// time.Time cannot encode a year outside [0,9999], which is the one way
	// encoding the request can fail.
	clock := newFakeMaintClock(time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC))

	err := newTestMaintenance(files, clock).Request(maintTestRunID)
	if err == nil {
		t.Fatal("Request with an unencodable time returned nil")
	}
	if !strings.Contains(err.Error(), "encode maintenance request") {
		t.Errorf("error %q does not name the encode", err)
	}
	if _, ok := files.entry(maintRequestFile); ok {
		t.Errorf("Request wrote a request file for a time it could not encode")
	}
}

func TestRequestFailureBranches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		seed func(t *testing.T, files *fakeMaintFiles)
		want string
	}{
		{
			name: "stale result cannot be removed",
			seed: func(_ *testing.T, files *fakeMaintFiles) {
				files.removeErr[maintResultFile] = errors.New("permission denied")
			},
			want: "clear stale maintenance result",
		},
		{
			name: "request file cannot be created",
			seed: func(_ *testing.T, files *fakeMaintFiles) {
				files.createErr[maintRequestFile] = errors.New("disk full")
			},
			want: "create maintenance request",
		},
		{
			name: "stale result and the request cannot be removed",
			seed: func(_ *testing.T, files *fakeMaintFiles) {
				files.removeErr[maintResultFile] = errors.New("permission denied")
				files.removeErr[maintRequestFile] = errors.New("permission denied")
			},
			want: "release request",
		},
		{
			name: "existing request cannot be read",
			seed: func(t *testing.T, files *fakeMaintFiles) {
				putMaintJSON(t, files, maintRequestFile, RequestFile{RunID: maintTestRunID, At: maintTestStart()})
				files.readErr[maintRequestFile] = errors.New("input/output error")
			},
			want: "read existing maintenance request",
		},
		{
			name: "existing request is malformed",
			seed: func(_ *testing.T, files *fakeMaintFiles) {
				files.put(maintRequestFile, []byte("{"), 0o600)
			},
			want: "parse existing maintenance request",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := newFakeMaintFiles()
			tt.seed(t, files)
			err := newTestMaintenance(files, newFakeMaintClock(maintTestStart())).Request(maintTestRunID)
			if err == nil {
				t.Fatalf("Request returned nil, want an error naming %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not name %q", err, tt.want)
			}
		})
	}
}

func TestWaitReturnsTheDaemonsResult(t *testing.T) {
	t.Parallel()
	files := newFakeMaintFiles()
	start := maintTestStart()
	clock := newFakeMaintClock(start)
	putMaintJSON(t, files, maintRequestFile, RequestFile{RunID: maintTestRunID, At: start})
	want := MaintenanceResult{
		RunID:   maintTestRunID,
		Outcome: "completed",
		At:      start.Add(30 * time.Second),
		Detail:  "archive, backup and gc ran",
	}
	clock.onSleep = func(n int) {
		if n != 2 {
			return
		}
		if err := files.Remove(maintRequestFile); err != nil {
			t.Fatalf("remove request after the daemon finished: %v", err)
		}
		putMaintJSON(t, files, maintResultFile, want)
	}

	got, err := newTestMaintenance(files, clock).Wait(context.Background(), clock.sleep, maintTestRunID, maintTestTimeout, maintTestPoll)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if got != want {
		t.Errorf("Wait = %+v, want the daemon's result %+v", got, want)
	}
	if len(clock.sleeps) != 2 {
		t.Errorf("Wait slept %d times, want 2: it should stop as soon as the request file is gone", len(clock.sleeps))
	}
}

func TestWaitMissingOrForeignResultIsFailed(t *testing.T) {
	t.Parallel()
	foreign := MaintenanceResult{RunID: "other-run", Outcome: "completed", At: maintTestStart()}
	tests := []struct {
		name     string
		daemon   func(t *testing.T, files *fakeMaintFiles)
		wantLies []string
	}{
		{
			name: "no result file",
			daemon: func(t *testing.T, files *fakeMaintFiles) {
				if err := files.Remove(maintRequestFile); err != nil {
					t.Fatalf("remove request: %v", err)
				}
			},
			wantLies: []string{"without writing " + ResultFileName},
		},
		{
			name: "result for another run",
			daemon: func(t *testing.T, files *fakeMaintFiles) {
				if err := files.Remove(maintRequestFile); err != nil {
					t.Fatalf("remove request: %v", err)
				}
				putMaintJSON(t, files, maintResultFile, foreign)
			},
			wantLies: []string{"other-run", maintTestRunID},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := newFakeMaintFiles()
			clock := newFakeMaintClock(maintTestStart())
			putMaintJSON(t, files, maintRequestFile, RequestFile{RunID: maintTestRunID, At: clock.now})
			clock.onSleep = func(n int) {
				if n == 1 {
					tt.daemon(t, files)
				}
			}

			got, err := newTestMaintenance(files, clock).Wait(context.Background(), clock.sleep, maintTestRunID, maintTestTimeout, maintTestPoll)
			if err != nil {
				t.Fatalf("Wait: %v", err)
			}
			if got.Outcome != "failed" {
				t.Errorf("Outcome = %q, want %q", got.Outcome, "failed")
			}
			for _, lie := range tt.wantLies {
				if !strings.Contains(got.Detail, lie) {
					t.Errorf("Detail = %q, want it to name %q", got.Detail, lie)
				}
			}
		})
	}
}

func TestWaitRefusesAnUnreadableOrMalformedResult(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		seed       func(t *testing.T, files *fakeMaintFiles)
		wantDetail string
	}{
		{
			name: "unreadable result",
			seed: func(t *testing.T, files *fakeMaintFiles) {
				putMaintJSON(t, files, maintResultFile, MaintenanceResult{RunID: maintTestRunID, Outcome: "completed"})
				files.readErr[maintResultFile] = errors.New("input/output error")
			},
			wantDetail: "read maintenance result",
		},
		{
			name: "malformed result",
			seed: func(_ *testing.T, files *fakeMaintFiles) {
				files.put(maintResultFile, []byte("{"), 0o600)
			},
			wantDetail: "parse maintenance result",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files := newFakeMaintFiles()
			tt.seed(t, files)
			clock := newFakeMaintClock(maintTestStart())

			got, err := newTestMaintenance(files, clock).Wait(context.Background(), clock.sleep, maintTestRunID, maintTestTimeout, maintTestPoll)
			if err != nil {
				t.Fatalf("Wait: %v", err)
			}
			if got.Outcome != "failed" {
				t.Errorf("Outcome = %q, want %q", got.Outcome, "failed")
			}
			if !strings.Contains(got.Detail, tt.wantDetail) {
				t.Errorf("Detail = %q, want it to name %q", got.Detail, tt.wantDetail)
			}
		})
	}
}

func TestWaitTimeoutIsDeferredAndLeavesTheRequest(t *testing.T) {
	t.Parallel()
	files := newFakeMaintFiles()
	start := maintTestStart()
	clock := newFakeMaintClock(start)
	putMaintJSON(t, files, maintRequestFile, RequestFile{RunID: maintTestRunID, At: start})

	got, err := newTestMaintenance(files, clock).Wait(context.Background(), clock.sleep, maintTestRunID, maintTestTimeout, maintTestPoll)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	want := MaintenanceResult{Outcome: "deferred", Detail: "the daemon did not finish in time"}
	if got != want {
		t.Errorf("Wait = %+v, want %+v", got, want)
	}
	if _, ok := files.entry(maintRequestFile); !ok {
		t.Errorf("request file %s was removed on timeout", maintRequestFile)
	}
	if sleepsWant := int(maintTestTimeout / maintTestPoll); len(clock.sleeps) != sleepsWant {
		t.Errorf("Wait slept %d times, want %d", len(clock.sleeps), sleepsWant)
	}
	if waited := clock.now.Sub(start); waited != maintTestTimeout {
		t.Errorf("Wait waited %v, want %v", waited, maintTestTimeout)
	}
}

func TestWaitCancelledBeforeTheFirstPoll(t *testing.T) {
	t.Parallel()
	files := newFakeMaintFiles()
	clock := newFakeMaintClock(maintTestStart())
	putMaintJSON(t, files, maintRequestFile, RequestFile{RunID: maintTestRunID, At: clock.now})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := newTestMaintenance(files, clock).Wait(ctx, clock.sleep, maintTestRunID, maintTestTimeout, maintTestPoll)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait error = %v, want context.Canceled", err)
	}
	if len(clock.sleeps) != 0 {
		t.Errorf("Wait slept %d times after cancellation, want 0", len(clock.sleeps))
	}
}

func TestWaitCancelledWithAResultAvailableReturnsTheContextError(t *testing.T) {
	t.Parallel()
	files := newFakeMaintFiles()
	clock := newFakeMaintClock(maintTestStart())
	putMaintJSON(t, files, maintResultFile, MaintenanceResult{RunID: maintTestRunID, Outcome: "completed"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := newTestMaintenance(files, clock).Wait(ctx, clock.sleep, maintTestRunID, maintTestTimeout, maintTestPoll)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait error = %v, want context.Canceled even though a result file exists", err)
	}
}

func TestWaitCancelledDuringASleepReturnsTheContextError(t *testing.T) {
	t.Parallel()
	files := newFakeMaintFiles()
	clock := newFakeMaintClock(maintTestStart())
	putMaintJSON(t, files, maintRequestFile, RequestFile{RunID: maintTestRunID, At: clock.now})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock.onSleep = func(int) { cancel() }

	_, err := newTestMaintenance(files, clock).Wait(ctx, clock.sleep, maintTestRunID, maintTestTimeout, maintTestPoll)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait error = %v, want context.Canceled", err)
	}
}

func TestWaitReturnsASleepError(t *testing.T) {
	t.Parallel()
	files := newFakeMaintFiles()
	clock := newFakeMaintClock(maintTestStart())
	putMaintJSON(t, files, maintRequestFile, RequestFile{RunID: maintTestRunID, At: clock.now})
	sentinel := errors.New("sleep failed")
	// The sleep advances the clock so a Wait that ignored the error would
	// reach its timeout and fail, not poll forever.
	sleep := func(_ context.Context, d time.Duration) error {
		clock.now = clock.now.Add(d)
		return sentinel
	}

	_, err := newTestMaintenance(files, clock).Wait(context.Background(), sleep, maintTestRunID, maintTestTimeout, maintTestPoll)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Wait error = %v, want %v", err, sentinel)
	}
}

func TestWaitRefusesAnUnreadableRequest(t *testing.T) {
	t.Parallel()
	files := newFakeMaintFiles()
	clock := newFakeMaintClock(maintTestStart())
	putMaintJSON(t, files, maintRequestFile, RequestFile{RunID: maintTestRunID, At: clock.now})
	files.readErr[maintRequestFile] = errors.New("input/output error")

	_, err := newTestMaintenance(files, clock).Wait(context.Background(), clock.sleep, maintTestRunID, maintTestTimeout, maintTestPoll)
	if err == nil {
		t.Fatal("Wait returned nil for an unreadable request file")
	}
	if !strings.Contains(err.Error(), "read maintenance request") {
		t.Errorf("error %q does not name the request read", err)
	}
}

func TestWaitRefusesAnEmptyRunID(t *testing.T) {
	t.Parallel()
	files := newFakeMaintFiles()
	clock := newFakeMaintClock(maintTestStart())
	_, err := newTestMaintenance(files, clock).Wait(context.Background(), clock.sleep, "", maintTestTimeout, maintTestPoll)
	if err == nil {
		t.Fatal("Wait(\"\") returned nil")
	}
}
