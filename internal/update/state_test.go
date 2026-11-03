package update

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// fullState is a record with every field set, so a round trip that drops a
// field shows it.
func fullState() State {
	start := time.Date(2026, 11, 3, 10, 22, 0, 0, time.UTC)
	return State{
		V:           stateVersion,
		RunID:       "20261103T102200Z-furiosa",
		Source:      SourceManual,
		StartedAt:   start,
		HeartbeatAt: start.Add(15 * time.Second),
		EndedAt:     start.Add(9 * time.Minute),
		Target:      Target{SHA: "aaaa1111", InstalledSHA: "bbbb2222"},
		Steps: []StepRecord{
			{
				Name: StepPlan, WillRun: true, Reason: "the installed build differs from main",
				Result: ResultOK, StartedAt: start, EndedAt: start.Add(time.Second),
				Detail: "planned 5 steps",
			},
			{
				Name: StepDrain, WillRun: true, Reason: "an install is planned",
				Result: ResultSkipped, StartedAt: start.Add(time.Second),
				EndedAt: start.Add(2 * time.Second), Detail: "no polecats working",
			},
			{
				Name: StepMaintenance, WillRun: false,
				Reason: "the archive is current and today's backup exists",
				Result: ResultDeferred, Detail: "gc not due",
			},
		},
		Current: StepInstall,
		Hold:    HoldRecord{Owned: true, Note: "gt update 20261103T102200Z-furiosa: draining"},
		Outcome: OutcomePartial,
	}
}

// withoutTimes returns st with its timestamps zeroed, so two records can be
// compared with reflect.DeepEqual and the timestamps compared by instant.
func withoutTimes(st State) State {
	st.StartedAt, st.HeartbeatAt, st.EndedAt = time.Time{}, time.Time{}, time.Time{}
	steps := make([]StepRecord, len(st.Steps))
	for i, step := range st.Steps {
		step.StartedAt, step.EndedAt = time.Time{}, time.Time{}
		steps[i] = step
	}
	st.Steps = steps
	return st
}

// assertSameState fails unless got and want hold the same record; times are
// compared as instants because JSON keeps the instant and not the location.
func assertSameState(t *testing.T, got, want State) {
	t.Helper()
	if !got.StartedAt.Equal(want.StartedAt) {
		t.Errorf("StartedAt = %v, want %v", got.StartedAt, want.StartedAt)
	}
	if !got.HeartbeatAt.Equal(want.HeartbeatAt) {
		t.Errorf("HeartbeatAt = %v, want %v", got.HeartbeatAt, want.HeartbeatAt)
	}
	if !got.EndedAt.Equal(want.EndedAt) {
		t.Errorf("EndedAt = %v, want %v", got.EndedAt, want.EndedAt)
	}
	if len(got.Steps) != len(want.Steps) {
		t.Fatalf("Steps = %d, want %d", len(got.Steps), len(want.Steps))
	}
	for i := range got.Steps {
		if !got.Steps[i].StartedAt.Equal(want.Steps[i].StartedAt) {
			t.Errorf("Steps[%d].StartedAt = %v, want %v", i, got.Steps[i].StartedAt, want.Steps[i].StartedAt)
		}
		if !got.Steps[i].EndedAt.Equal(want.Steps[i].EndedAt) {
			t.Errorf("Steps[%d].EndedAt = %v, want %v", i, got.Steps[i].EndedAt, want.Steps[i].EndedAt)
		}
	}
	if g, w := withoutTimes(got), withoutTimes(want); !reflect.DeepEqual(g, w) {
		t.Errorf("state after round trip = %+v, want %+v", g, w)
	}
}

func TestStoreWriteReadRoundTrip(t *testing.T) {
	t.Parallel()
	store := &Store{Dir: filepath.Join(t.TempDir(), "update")}
	want := fullState()

	if err := store.Write(want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	assertSameState(t, got, want)
}

func TestStoreWritesTheVersionAsV1(t *testing.T) {
	t.Parallel()
	store := &Store{Dir: t.TempDir()}
	if err := store.Write(fullState()); err != nil {
		t.Fatalf("Write: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(store.Dir, stateFileName))
	if err != nil {
		t.Fatalf("read state.json: %v", err)
	}
	if !bytes.Contains(raw, []byte(`"v": 1`)) {
		t.Errorf("state.json does not carry v 1:\n%s", raw)
	}
}

func TestStoreWriteIsAtomic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := &Store{Dir: dir}
	if err := store.Write(fullState()); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	statePath := filepath.Join(dir, stateFileName)
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read published state: %v", err)
	}

	store.Rename = func(oldpath, newpath string) error {
		return errors.New("rename refused")
	}
	next := fullState()
	next.RunID = "second-run"
	if err := store.Write(next); err == nil {
		t.Fatal("Write succeeded with a failing rename")
	}

	after, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state after failed Write: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Errorf("failed Write changed state.json:\n got %s\nwant %s", after, before)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read state dir: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() != stateFileName {
			t.Errorf("failed Write left %q behind", entry.Name())
		}
	}
}

func TestStoreReadMissingAndUnknownVersion(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := &Store{Dir: dir}

	if _, err := store.Read(); !errors.Is(err, ErrNoState) {
		t.Fatalf("Read with no state.json = %v, want ErrNoState", err)
	}

	if err := os.WriteFile(filepath.Join(dir, stateFileName), []byte(`{"v": 2}`), 0o600); err != nil {
		t.Fatalf("write future state: %v", err)
	}
	_, err := store.Read()
	if err == nil {
		t.Fatal("Read accepted version 2")
	}
	if !strings.Contains(err.Error(), "version 2") {
		t.Errorf("Read error %q does not name the version it found", err)
	}
}

func TestStoreReadRefusesMalformedJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, stateFileName), []byte("{"), 0o600); err != nil {
		t.Fatalf("write truncated state: %v", err)
	}

	if _, err := (&Store{Dir: dir}).Read(); err == nil {
		t.Fatal("Read parsed a truncated record")
	}
}

func TestStoreWriteCreatesTheDirectoryOwnerOnly(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "update")
	store := &Store{Dir: dir}
	if err := store.Write(fullState()); err != nil {
		t.Fatalf("Write: %v", err)
	}

	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat state dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("state directory mode = %o, want 0700", got)
	}
	fileInfo, err := os.Stat(filepath.Join(dir, stateFileName))
	if err != nil {
		t.Fatalf("stat state.json: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("state.json mode = %o, want 0600", got)
	}
}

func TestStoreWriteRefusesWhenTheDirectoryCannotBeCreated(t *testing.T) {
	t.Parallel()
	blocked := filepath.Join(t.TempDir(), "update")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("block the state directory: %v", err)
	}

	if err := (&Store{Dir: blocked}).Write(fullState()); err == nil {
		t.Fatal("Write succeeded with a file where the state directory belongs")
	}
}

func TestStoreMutateSerializesWriters(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store := &Store{Dir: dir}
	if _, err := store.Mutate(func(*State) {}); !errors.Is(err, ErrNoCurrent) {
		t.Fatalf("Mutate before Write = %v, want ErrNoCurrent", err)
	}
	if err := store.Write(fullState()); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// A reader parses every state.json it can see while the writers run: each
	// intermediate file must hold a whole record.
	statePath := filepath.Join(dir, stateFileName)
	done := make(chan struct{})
	readErr := make(chan error, 1)
	parsed := make(chan struct{})
	go func() {
		signaled := false
		for {
			select {
			case <-done:
				readErr <- nil
				return
			default:
			}
			data, err := os.ReadFile(statePath)
			if err != nil {
				readErr <- fmt.Errorf("read state.json: %w", err)
				return
			}
			var st State
			if err := json.Unmarshal(data, &st); err != nil {
				readErr <- fmt.Errorf("state.json did not parse: %w", err)
				return
			}
			if !signaled {
				close(parsed)
				signaled = true
			}
		}
	}()
	select {
	case <-parsed:
	case err := <-readErr:
		t.Fatalf("reading state.json: %v", err)
	}

	const writes = 200
	var wg sync.WaitGroup
	mutErrs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < writes; i++ {
			step := StepName(fmt.Sprintf("step-%d", i))
			if _, err := store.Mutate(func(st *State) { st.Current = step }); err != nil {
				mutErrs <- err
				return
			}
		}
		mutErrs <- nil
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < writes; i++ {
			outcome := Outcome(fmt.Sprintf("outcome-%d", i))
			if _, err := store.Mutate(func(st *State) { st.Outcome = outcome }); err != nil {
				mutErrs <- err
				return
			}
		}
		mutErrs <- nil
	}()
	wg.Wait()
	close(done)
	if err := <-readErr; err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := <-mutErrs; err != nil {
			t.Fatalf("Mutate: %v", err)
		}
	}

	final, err := store.Read()
	if err != nil {
		t.Fatalf("Read final state: %v", err)
	}
	if want := StepName(fmt.Sprintf("step-%d", writes-1)); final.Current != want {
		t.Errorf("Current = %q, want %q", final.Current, want)
	}
	if want := Outcome(fmt.Sprintf("outcome-%d", writes-1)); final.Outcome != want {
		t.Errorf("Outcome = %q, want %q", final.Outcome, want)
	}
}

func TestStoreMutateReportsAFailedPublish(t *testing.T) {
	t.Parallel()
	store := &Store{Dir: t.TempDir()}
	if err := store.Write(fullState()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	store.Rename = func(oldpath, newpath string) error {
		return errors.New("rename refused")
	}

	if _, err := store.Mutate(func(st *State) { st.Outcome = OutcomeOK }); err == nil {
		t.Fatal("Mutate succeeded with a failing publish")
	}
	final, err := store.Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if final.Outcome != OutcomePartial {
		t.Errorf("Outcome = %q, want the published %q", final.Outcome, OutcomePartial)
	}
}

func TestStoreReadReportsAnUnreadableFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, stateFileName), 0o700); err != nil {
		t.Fatalf("make state.json a directory: %v", err)
	}

	_, err := (&Store{Dir: dir}).Read()
	if err == nil {
		t.Fatal("Read succeeded on a directory")
	}
	if errors.Is(err, ErrNoState) {
		t.Errorf("Read of an unreadable state.json = %v, want the read error, not ErrNoState", err)
	}
	if !strings.Contains(err.Error(), "read update state") {
		t.Errorf("error %q does not name the read", err)
	}
}

func TestStoreWriteRefusesATimeItCannotEncode(t *testing.T) {
	t.Parallel()
	store := &Store{Dir: t.TempDir()}
	st := fullState()
	st.StartedAt = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)

	err := store.Write(st)
	if err == nil {
		t.Fatal("Write encoded a year outside the RFC 3339 range")
	}
	if !strings.Contains(err.Error(), "encode update state") {
		t.Errorf("error %q does not name the encoding", err)
	}
}

func TestStoreWriteReportsAFailedStagingWrite(t *testing.T) {
	t.Parallel()
	store := &Store{Dir: t.TempDir()}
	store.write = func(path string, data []byte, perm os.FileMode) error {
		return errors.New("write refused")
	}

	err := store.Write(fullState())
	if err == nil {
		t.Fatal("Write succeeded with a failing staging write")
	}
	if !strings.Contains(err.Error(), "write update state") {
		t.Errorf("error %q does not name the write", err)
	}
	if _, statErr := os.Stat(filepath.Join(store.Dir, stateFileName)); !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("a failed staging write published a state file: %v", statErr)
	}
}

func TestStoreWriteReportsAStagingFileItCannotRemove(t *testing.T) {
	t.Parallel()
	store := &Store{Dir: t.TempDir()}
	if err := store.Write(fullState()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Leave a non-empty directory where the staging file was: the rename
	// fails, and the cleanup cannot remove it either.
	store.Rename = func(oldpath, newpath string) error {
		if err := os.Remove(oldpath); err != nil {
			return err
		}
		if err := os.Mkdir(oldpath, 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(oldpath, "blocker"), []byte("x"), 0o600); err != nil {
			return err
		}
		return errors.New("rename refused")
	}

	err := store.Write(fullState())
	if err == nil {
		t.Fatal("Write succeeded with a staging file it could not publish")
	}
	if !strings.Contains(err.Error(), "remove staging file") {
		t.Errorf("error %q does not report the staging file it kept", err)
	}
}
