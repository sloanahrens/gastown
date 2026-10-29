package hostprobe

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// testAt is the stamp the tests share. It is a fixed date, never time.Now, so
// a test's verdict does not depend on when it ran.
func testAt() time.Time { return time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC) }

// fullProbes is a value with every field filled, the value the round trip is
// about.
func fullProbes() Probes {
	return Probes{
		At:           testAt(),
		MemFreePct:   Field[float64]{Value: 94},
		Disk:         Field[Disk]{Value: Disk{UsedPct: 61.5, FreeGB: 120.25}},
		Containers:   Field[[]string]{Value: []string{"gastown-daemon", "gastown-dolt"}},
		CPUIdlePct:   Field[float64]{Value: 78.5},
		TopConsumers: Field[[]Consumer]{Value: []Consumer{{Name: "dolt", CPU: 120.5}, {Name: "gt", CPU: 40}}},
		ExecTaxMS:    Field[float64]{Value: 12.5},
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "probes.json")
	want := fullProbes()
	if err := Write(path, want); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Write stamps the schema version, so that is what the file holds.
	want.V = SchemaVersion

	got, err := Read(path, testAt().Add(30*time.Second), 3*time.Minute)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	// The stamp is compared as an instant: JSON carries no monotonic reading,
	// so a decoded time is never the same struct field by field.
	if !got.At.Equal(want.At) {
		t.Errorf("At = %v, want %v", got.At, want.At)
	}
	want.At, got.At = time.Time{}, time.Time{}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip = %+v, want %+v", got, want)
	}
}

func TestWriteReadKeepsFieldError(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "probes.json")
	want := fullProbes()
	want.Disk = Field[Disk]{Err: "df /: exit status 1"}
	if err := Write(path, want); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := Read(path, testAt(), 3*time.Minute)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Disk.Err != want.Disk.Err {
		t.Errorf("Disk.Err = %q, want %q", got.Disk.Err, want.Disk.Err)
	}
	if got.MemFreePct != want.MemFreePct {
		t.Errorf("MemFreePct = %+v, want the failed field to leave its neighbours alone: %+v",
			got.MemFreePct, want.MemFreePct)
	}
}

// TestReadRefusesUnusableFile covers the refusals that do not depend on the
// clock: no file, no JSON, and a schema version this binary does not know.
func TestReadRefusesUnusableFile(t *testing.T) {
	t.Parallel()
	now := testAt()
	for _, tc := range []struct {
		name    string
		body    string // empty means no file is written
		wantErr error  // when set, the error must wrap it
		want    string // the refusal the error must state, so a row cannot pass on another refusal
	}{
		{name: "a missing file", wantErr: fs.ErrNotExist, want: "read probe file"},
		{name: "not JSON", body: "{", want: "decode probe file"},
		{name: "a later schema version", body: fmt.Sprintf(`{"v":2,"at":%q}`, now.Format(time.RFC3339)), want: "schema version 2"},
		{name: "a zero schema version", body: fmt.Sprintf(`{"at":%q}`, now.Format(time.RFC3339)), want: "schema version 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "probes.json")
			if tc.body != "" {
				if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
					t.Fatalf("write the fixture: %v", err)
				}
			}

			got, err := Read(path, now, 3*time.Minute)
			if err == nil {
				t.Fatalf("Read = %+v, nil; want an error", got)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("Read error = %v, want it to wrap %v", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Read error = %q, want it to state %q", err, tc.want)
			}
		})
	}
}

// TestReadAgeChecks pins both ends of the freshness window: a stamp older than
// maxAge and one more than futureTolerance ahead are refused, and the two edges
// themselves are read. The stale row fails if the maxAge check goes, the future
// row if the future check goes.
func TestReadAgeChecks(t *testing.T) {
	t.Parallel()
	now := testAt()
	const maxAge = 3 * time.Minute
	for _, tc := range []struct {
		name    string
		at      time.Time
		wantErr bool
	}{
		{name: "a minute old", at: now.Add(-time.Minute)},
		{name: "exactly maxAge old", at: now.Add(-maxAge)},
		{name: "older than maxAge", at: now.Add(-maxAge - time.Second), wantErr: true},
		{name: "five seconds ahead", at: now.Add(futureTolerance)},
		{name: "more than five seconds ahead", at: now.Add(futureTolerance + time.Second), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "probes.json")
			body, err := json.Marshal(Probes{V: SchemaVersion, At: tc.at, MemFreePct: Field[float64]{Value: 94}})
			if err != nil {
				t.Fatalf("marshal the fixture: %v", err)
			}
			if err := os.WriteFile(path, body, 0o644); err != nil {
				t.Fatalf("write the fixture: %v", err)
			}

			if _, err := Read(path, now, maxAge); (err != nil) != tc.wantErr {
				t.Errorf("Read error = %v, want an error: %v", err, tc.wantErr)
			}
		})
	}
}

// TestWriteRefusesNaN: JSON cannot carry a NaN, and a write that failed must
// leave no file for the daemon to decode.
func TestWriteRefusesNaN(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "probes.json")
	p := fullProbes()
	p.CPUIdlePct = Field[float64]{Value: math.NaN()}

	if err := Write(path, p); err == nil {
		t.Fatal("Write of a NaN reading succeeded")
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat after the failed write = %v, want the file to be absent", err)
	}
}

// TestWriteRenameFailureLeavesNoPartialFile is the acceptance criterion: the
// rename fails and the directory the writer touched holds nothing but the
// target it found. The fake filesystem leaves the temp file where a real
// failed rename leaves it, so this fails if write stops removing it.
func TestWriteRenameFailureLeavesNoPartialFile(t *testing.T) {
	t.Parallel()
	const target = "/town/.runtime/host/probes.json"
	previous := `{"v":1,"at":"2026-10-29T11:59:00Z"}`
	fake := newFakeFS(filepath.Dir(target))
	fake.entries[target] = []byte(previous)
	fake.renameErr = errors.New("rename: device or resource busy")

	err := write(target, fullProbes(), fake.seam())
	if err == nil {
		t.Fatal("write whose rename failed returned nil")
	}
	if !errors.Is(err, fake.renameErr) {
		t.Errorf("write error = %v, want it to wrap the rename failure", err)
	}
	if len(fake.entries) != 1 || string(fake.entries[target]) != previous {
		t.Errorf("filesystem after the failed rename = %v, want only the untouched target %q",
			fake.names(), target)
	}
}

// TestWritePublishesTheTarget: a write that finishes leaves the bytes in the
// target, no temp file beside it, and the mode the daemon reads it with.
func TestWritePublishesTheTarget(t *testing.T) {
	t.Parallel()
	const target = "/town/.runtime/host/probes.json"
	fake := newFakeFS(filepath.Dir(target))

	if err := write(target, fullProbes(), fake.seam()); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := fake.names(); len(got) != 1 || got[0] != target {
		t.Errorf("filesystem after the write = %v, want only the target", got)
	}
	if fake.mode != probeFileMode {
		t.Errorf("published mode = %v, want %v", fake.mode, probeFileMode)
	}
	if body := string(fake.entries[target]); !strings.Contains(body, fmt.Sprintf(`"v":%d`, SchemaVersion)) {
		t.Errorf("target = %.60q, want the schema version the writer stamps", body)
	}
}

// TestWriteStepFailureLeavesNoPartialFile drives every step but the rename
// failing: the temp file is removed and the target keeps what it held, except
// where the remove itself is the failing step, and then the error names it.
func TestWriteStepFailureLeavesNoPartialFile(t *testing.T) {
	t.Parallel()
	const target = "/town/.runtime/host/probes.json"
	previous := `{"v":1,"at":"2026-10-29T11:59:00Z"}`
	for _, tc := range []struct {
		name string
		fail func(f *fakeFS, cause error)
		// temps is how many temp files the directory must hold afterwards,
		// and states is a phrase the error must carry beyond the target.
		temps  int
		states string
	}{
		{name: "the temp file cannot be created",
			fail: func(f *fakeFS, cause error) { f.createErr = cause }},
		{name: "the write fails",
			fail: func(f *fakeFS, cause error) { f.writeErr = cause }},
		{name: "the chmod fails",
			fail: func(f *fakeFS, cause error) { f.chmodErr = cause }},
		{name: "the sync fails",
			fail: func(f *fakeFS, cause error) { f.syncErr = cause }},
		{name: "the close fails",
			fail: func(f *fakeFS, cause error) { f.closeErr = cause }},
		{name: "the temp file cannot be removed",
			fail: func(f *fakeFS, cause error) {
				f.renameErr = cause
				f.removeErr = errors.New("remove: device busy")
			},
			temps: 1, states: "removing the temp file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cause := errors.New(tc.name)
			fake := newFakeFS(filepath.Dir(target))
			fake.entries[target] = []byte(previous)
			tc.fail(fake, cause)

			err := write(target, fullProbes(), fake.seam())
			if err == nil {
				t.Fatal("write that could not finish returned nil")
			}
			if !errors.Is(err, cause) {
				t.Errorf("write error = %v, want it to wrap %v", err, cause)
			}
			if !strings.Contains(err.Error(), "write probes to") {
				t.Errorf("write error = %q, want it to name the target %s", err, target)
			}
			if tc.states != "" && !strings.Contains(err.Error(), tc.states) {
				t.Errorf("write error = %q, want it to state %q", err, tc.states)
			}
			if got := string(fake.entries[target]); got != previous {
				t.Errorf("target = %q after the failed write, want %q untouched", got, previous)
			}
			if temps := len(fake.entries) - 1; temps != tc.temps {
				t.Errorf("filesystem = %v after the failed write, want %d temp file(s) and the target",
					fake.names(), tc.temps)
			}
		})
	}
}

// TestWriteOverDirectoryLeavesNoPartialFile drives the same failure through the
// real filesystem, which is where the seam's defaults come from: a rename over
// a non-empty directory fails on every platform this runs on, and the temp file
// must not survive it. It guards the wiring the fake bypasses.
func TestWriteOverDirectoryLeavesNoPartialFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	target := filepath.Join(dir, "probes.json")
	if err := os.MkdirAll(filepath.Join(target, "occupant"), 0o755); err != nil {
		t.Fatalf("stage the target directory: %v", err)
	}

	if err := Write(target, fullProbes()); err == nil {
		t.Fatal("Write over a non-empty directory succeeded")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the directory: %v", err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	if len(entries) != 1 || entries[0].Name() != "probes.json" || !entries[0].IsDir() {
		t.Errorf("entries after the failed write = %v, want only the untouched target", names)
	}
}

// TestWriteRefusesZeroAt: a zero At is a stamp every reader computes as older
// than any maxAge, so the write refuses it rather than publishing a file no
// reader can use.
func TestWriteRefusesZeroAt(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "probes.json")
	p := fullProbes()
	p.At = time.Time{}

	if err := Write(path, p); err == nil {
		t.Fatal("Write with a zero At succeeded")
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat after the refused write = %v, want the file to be absent", err)
	}
}

// fakeFS is an in-memory filesystem holding one directory: the temp file a
// write creates and the target its rename publishes. entries is keyed by full
// path. Each *Err field makes that one step fail; a rename that fails leaves
// the temp file where it was and the target holding what it held, as a real
// one does.
type fakeFS struct {
	dir     string
	entries map[string][]byte
	created int
	mode    os.FileMode // the mode the last Chmod applied, and whether it ran

	createErr error
	writeErr  error
	chmodErr  error
	syncErr   error
	closeErr  error
	renameErr error
	removeErr error
}

func newFakeFS(dir string) *fakeFS {
	return &fakeFS{dir: dir, entries: map[string][]byte{}}
}

// names lists the entries, for an assertion's failure message.
func (f *fakeFS) names() []string {
	names := make([]string, 0, len(f.entries))
	for name := range f.entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// seam is the write's filesystem. createTemp refuses a directory other than
// the target's own, which is the property the rename depends on: a rename is
// atomic only within a filesystem.
func (f *fakeFS) seam() probeFS {
	return probeFS{
		createTemp: func(dir, pattern string) (tempFile, error) {
			if dir != f.dir {
				return nil, fmt.Errorf("createTemp in %q, want the target's own directory %q", dir, f.dir)
			}
			if f.createErr != nil {
				return nil, f.createErr
			}
			f.created++
			name := filepath.Join(dir, strings.Replace(pattern, "*", strconv.Itoa(f.created), 1))
			f.entries[name] = []byte{}
			return &fakeTempFile{fs: f, name: name}, nil
		},
		rename: func(oldpath, newpath string) error {
			if f.renameErr != nil {
				return f.renameErr
			}
			data, ok := f.entries[oldpath]
			if !ok {
				return fmt.Errorf("rename %q: no such file", oldpath)
			}
			f.entries[newpath] = data
			delete(f.entries, oldpath)
			return nil
		},
		remove: func(name string) error {
			if f.removeErr != nil {
				return f.removeErr
			}
			if _, ok := f.entries[name]; !ok {
				return fmt.Errorf("remove %q: no such file", name)
			}
			delete(f.entries, name)
			return nil
		},
	}
}

// fakeTempFile is the temp file the seam hands back: its bytes land in the
// fake's entries, so a failed write's leftovers are what the test reads.
type fakeTempFile struct {
	fs   *fakeFS
	name string
}

func (t *fakeTempFile) Name() string { return t.name }

func (t *fakeTempFile) Write(p []byte) (int, error) {
	if t.fs.writeErr != nil {
		return 0, t.fs.writeErr
	}
	if _, ok := t.fs.entries[t.name]; !ok {
		return 0, fmt.Errorf("write to %q: no such file", t.name)
	}
	t.fs.entries[t.name] = append(t.fs.entries[t.name], p...)
	return len(p), nil
}

func (t *fakeTempFile) Chmod(mode os.FileMode) error {
	if t.fs.chmodErr != nil {
		return t.fs.chmodErr
	}
	t.fs.mode = mode
	return nil
}

func (t *fakeTempFile) Sync() error {
	if t.fs.syncErr != nil {
		return t.fs.syncErr
	}
	return nil
}
func (t *fakeTempFile) Close() error { return t.fs.closeErr }
