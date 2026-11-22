package land

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func sampleGateRun(bead string) GateRun {
	return GateRun{Bead: bead,
		Packages:  []GateRunPackage{{Pkg: "internal/land", S: 139}},
		SlowTests: []GateRunTest{{Pkg: "internal/land", Test: "TestSlow", S: 40}}}
}
func mustLine(t *testing.T, r GateRun) string {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal gate run: %v", err)
	}
	return string(b)
}
func TestGateRunStoreRoundTrip(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	want := []GateRun{sampleGateRun("gt-first"), sampleGateRun("gt-second")}
	for _, r := range want {
		if err := AppendGateRun(town, r); err != nil {
			t.Fatalf("AppendGateRun(%s): %v", r.Bead, err)
		}
	}
	got, unparsed, err := ReadGateRuns(town)
	if err != nil {
		t.Fatalf("ReadGateRuns: %v", err)
	}
	if unparsed != 0 || !reflect.DeepEqual(got, want) {
		t.Errorf("ReadGateRuns = %+v, unparsed %d; want %+v, 0", got, unparsed, want)
	}
}
func TestGateRunStoreErrors(t *testing.T) {
	t.Parallel()
	first, last := sampleGateRun("gt-first"), sampleGateRun("gt-last")
	for _, corrupt := range []string{`{"bead":"gt-x"`, `null`, `[1,2]`} {
		t.Run(corrupt, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			body := strings.Join([]string{mustLine(t, first), corrupt, mustLine(t, last)}, "\n") + "\n"
			writeFile(t, town, ".runtime/metrics/gate-runs.jsonl", body)
			got, unparsed, err := ReadGateRuns(town)
			if err != nil {
				t.Fatalf("ReadGateRuns: %v", err)
			}
			if unparsed != 1 || !reflect.DeepEqual(got, []GateRun{first, last}) {
				t.Errorf("got %+v, unparsed %d; want the two runs around the corrupt line", got, unparsed)
			}
		})
	}
	if err := AppendGateRun("", first); !errors.Is(err, ErrNoGateRunRoot) {
		t.Errorf("AppendGateRun with no town root = %v, want ErrNoGateRunRoot", err)
	}
	if _, _, err := ReadGateRuns(""); !errors.Is(err, ErrNoGateRunRoot) {
		t.Errorf("ReadGateRuns with no town root = %v, want ErrNoGateRunRoot", err)
	}
	t.Run("a town with no log", func(t *testing.T) {
		t.Parallel()
		if runs, unparsed, err := ReadGateRuns(t.TempDir()); err != nil || runs != nil || unparsed != 0 {
			t.Errorf("ReadGateRuns = %v, %d, %v; want no runs and no error", runs, unparsed, err)
		}
	})
	t.Run("a log that cannot be read", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		if err := os.MkdirAll(gateRunsPath(town), 0o755); err != nil {
			t.Fatal(err)
		}
		runs, unparsed, err := ReadGateRuns(town)
		if err == nil || !strings.Contains(err.Error(), gateRunsPath(town)) || runs != nil || unparsed != 0 {
			t.Errorf("ReadGateRuns = %v, %d, %v; want no runs and a read error naming the log", runs, unparsed, err)
		}
	})
	t.Run("a file where the log directory belongs", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		writeFile(t, town, ".runtime", "not a directory")
		runs, _, err := ReadGateRuns(town)
		if err == nil || !strings.Contains(err.Error(), gateRunsPath(town)) {
			t.Errorf("ReadGateRuns = %v, %v; want the open error naming the log", runs, err)
		}
	})
}
func TestGateRunStoreOverlongLine(t *testing.T) {
	t.Parallel()
	first, last := sampleGateRun("gt-first"), sampleGateRun("gt-last")
	town := t.TempDir()
	body := mustLine(t, first) + "\n" + strings.Repeat("x", 2<<20) + "\n" + mustLine(t, last) + "\n"
	writeFile(t, town, ".runtime/metrics/gate-runs.jsonl", body)
	got, unparsed, err := ReadGateRuns(town)
	if err != nil {
		t.Fatalf("ReadGateRuns: %v", err)
	}
	if unparsed != 1 || !reflect.DeepEqual(got, []GateRun{first, last}) {
		t.Errorf("ReadGateRuns = %+v, unparsed %d; want the two runs around the over-long line, 1 unparsed", got, unparsed)
	}
	// The limit is on the line without its newline: a row of exactly the limit
	// reads, one byte more is skipped.
	fill := gateRunLineMax - len(mustLine(t, sampleGateRun("")))
	atLimit := sampleGateRun(strings.Repeat("a", fill))
	if n := len(mustLine(t, atLimit)); n != gateRunLineMax {
		t.Fatalf("test row is %d bytes, want %d", n, gateRunLineMax)
	}
	t.Run("a row of exactly the limit reads", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		body := mustLine(t, atLimit) + "\n" + mustLine(t, last) + "\n"
		writeFile(t, town, ".runtime/metrics/gate-runs.jsonl", body)
		got, unparsed, err := ReadGateRuns(town)
		if err != nil || unparsed != 0 || !reflect.DeepEqual(got, []GateRun{atLimit, last}) {
			t.Errorf("ReadGateRuns = %+v, unparsed %d, %v; want both rows, 0 unparsed", got, unparsed, err)
		}
	})
	t.Run("a row one byte past the limit is skipped", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		over := sampleGateRun(strings.Repeat("a", fill+1))
		body := mustLine(t, over) + "\n" + mustLine(t, last) + "\n"
		writeFile(t, town, ".runtime/metrics/gate-runs.jsonl", body)
		got, unparsed, err := ReadGateRuns(town)
		if err != nil || unparsed != 1 || !reflect.DeepEqual(got, []GateRun{last}) {
			t.Errorf("ReadGateRuns = %+v, unparsed %d, %v; want the last row, 1 unparsed", got, unparsed, err)
		}
	})
	t.Run("a last row without a newline reads", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		writeFile(t, town, ".runtime/metrics/gate-runs.jsonl", mustLine(t, first)+"\n"+mustLine(t, last))
		got, unparsed, err := ReadGateRuns(town)
		if err != nil || unparsed != 0 || !reflect.DeepEqual(got, []GateRun{first, last}) {
			t.Errorf("ReadGateRuns = %+v, unparsed %d, %v; want both rows, 0 unparsed", got, unparsed, err)
		}
	})
	t.Run("an over-long last line without a newline is skipped", func(t *testing.T) {
		t.Parallel()
		town := t.TempDir()
		writeFile(t, town, ".runtime/metrics/gate-runs.jsonl", mustLine(t, first)+"\n"+strings.Repeat("x", 2<<20))
		got, unparsed, err := ReadGateRuns(town)
		if err != nil || unparsed != 1 || !reflect.DeepEqual(got, []GateRun{first}) {
			t.Errorf("ReadGateRuns = %+v, unparsed %d, %v; want the first row, 1 unparsed", got, unparsed, err)
		}
	})
}
func TestGateRunStoreFailures(t *testing.T) {
	t.Parallel()
	dirErr, openErr := errors.New("mkdir refused"), errors.New("open refused")
	writeErr, closeErr := errors.New("disk full"), errors.New("close failed")
	tests := []struct {
		name string
		fsys *fakeGateRunFS
		run  GateRun
		want []error
	}{
		{"directory", &fakeGateRunFS{mkdirErr: dirErr}, sampleGateRun("gt-a"), []error{dirErr}},
		{"open", &fakeGateRunFS{openErr: openErr}, sampleGateRun("gt-a"), []error{openErr}},
		{"write and close", &fakeGateRunFS{writeErr: writeErr, closeErr: closeErr}, sampleGateRun("gt-a"), []error{writeErr, closeErr}},
		{"short write", &fakeGateRunFS{shortBy: 3}, sampleGateRun("gt-a"), []error{io.ErrShortWrite}},
		{"close", &fakeGateRunFS{closeErr: closeErr}, sampleGateRun("gt-a"), []error{closeErr}},
		{"unencodable run", &fakeGateRunFS{}, GateRun{Bead: "gt-nan", UnitWallS: math.NaN()}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := appendGateRun(tt.fsys, "/town", tt.run)
			if err == nil || !strings.Contains(err.Error(), gateRunsPath("/town")) {
				t.Fatalf("appendGateRun = %v, want an error naming %q", err, gateRunsPath("/town"))
			}
			for _, want := range tt.want {
				if !errors.Is(err, want) {
					t.Errorf("error = %v, want it to wrap %v", err, want)
				}
			}
		})
	}
}

type fakeGateRunFS struct {
	mkdirErr, openErr, writeErr, closeErr error
	shortBy                               int
}

func (f *fakeGateRunFS) MkdirAll(string, os.FileMode) error { return f.mkdirErr }
func (f *fakeGateRunFS) OpenAppend(string) (io.WriteCloser, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	return f, nil
}
func (f *fakeGateRunFS) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return max(0, len(p)-f.shortBy), nil
}
func (f *fakeGateRunFS) Close() error                    { return f.closeErr }
func (f *fakeGateRunFS) Size(string) (int64, error)      { return 0, nil }
func (f *fakeGateRunFS) ReadFile(string) ([]byte, error) { return nil, nil }
func (f *fakeGateRunFS) Remove(string) error             { return nil }

// memGateRunFS is an in-memory gateRunFS whose failures a row names by path part
// (gt-t3upsa); a write or close that fails does so for the month file only.
type memGateRunFS struct {
	files                          map[string][]byte
	sizeErr, readErr, removeErr    error
	openFailOn, writeFailOn, clsOn string
	opened                         []string // every path OpenAppend was asked for
	readFailOn                     string   // a read error applies to paths holding this ("" = all)
	err                            error
}

func newMemGateRunFS() *memGateRunFS { return &memGateRunFS{files: map[string][]byte{}} }

func (m *memGateRunFS) MkdirAll(string, os.FileMode) error { return nil }
func (m *memGateRunFS) Size(p string) (int64, error)       { return int64(len(m.files[p])), m.sizeErr }
func (m *memGateRunFS) ReadFile(p string) ([]byte, error) {
	if m.readErr != nil && strings.Contains(p, m.readFailOn) {
		return nil, m.readErr
	}
	data, ok := m.files[p]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return data, nil
}
func (m *memGateRunFS) Remove(p string) error {
	if m.removeErr == nil {
		delete(m.files, p)
	}
	return m.removeErr
}
func (m *memGateRunFS) OpenAppend(p string) (io.WriteCloser, error) {
	m.opened = append(m.opened, p)
	if m.openFailOn != "" && strings.Contains(p, m.openFailOn) {
		return nil, m.err
	}
	return &memGateFile{m: m, path: p}, nil
}

type memGateFile struct {
	m    *memGateRunFS
	path string
}

func (f *memGateFile) Write(b []byte) (int, error) {
	if f.m.writeFailOn != "" && strings.Contains(f.path, f.m.writeFailOn) {
		return 0, f.m.err
	}
	f.m.files[f.path] = append(f.m.files[f.path], b...)
	return len(b), nil
}
func (f *memGateFile) Close() error {
	if f.m.clsOn != "" && strings.Contains(f.path, f.m.clsOn) {
		return f.m.err
	}
	return nil
}

// beadsIn lists the beads of the rows in a file's bytes, gzipped or not.
func beadsIn(t *testing.T, data []byte, gz bool) []string {
	t.Helper()
	if gz {
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("gzip reader: %v", err)
		}
		if data, err = io.ReadAll(zr); err != nil {
			t.Fatalf("reading the members: %v", err)
		}
	}
	var beads []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var r GateRun
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		beads = append(beads, r.Bead)
	}
	return beads
}

const rotateLimit = 200

// TestGateRunStoreRotates: the append that finds the log at the limit moves the
// earlier rows into the month's gzip file and starts a new log holding the new
// row, and a second rotation in the month adds a gzip member that reads back
// with the first (gt-t3upsa).
func TestGateRunStoreRotates(t *testing.T) {
	t.Parallel()
	fsys := newMemGateRunFS()
	cur := gateRunsPath("/town")
	month := filepath.Join(filepath.Dir(cur), "gate-runs-2026-10.jsonl.gz")
	for i, bead := range []string{"gt-a", "gt-b", "gt-c"} {
		r := sampleGateRun(bead)
		r.TS = time.Date(2026, 10, 1+i, 12, 0, 0, 0, time.UTC)
		if err := appendGateRunLimit(fsys, "/town", r, rotateLimit); err != nil {
			t.Fatalf("append %s: %v", bead, err)
		}
		switch bead {
		case "gt-a":
			if _, ok := fsys.files[month]; ok {
				t.Fatal("a log under the limit was rotated")
			}
		case "gt-b":
			if got := beadsIn(t, fsys.files[month], true); !slices.Equal(got, []string{"gt-a"}) {
				t.Errorf("month file after the first rotation = %v, want [gt-a]", got)
			}
		}
	}
	if got := beadsIn(t, fsys.files[month], true); !slices.Equal(got, []string{"gt-a", "gt-b"}) {
		t.Errorf("month file after the second rotation = %v, want both batches [gt-a gt-b]", got)
	}
	if got := beadsIn(t, fsys.files[cur], false); !slices.Equal(got, []string{"gt-c"}) {
		t.Errorf("current log = %v, want only the row that crossed the limit [gt-c]", got)
	}
}

// TestGateRunStoreRotationFailure: a rotation that fails at any step is returned
// after the row is appended, and the log still holds the rows it held (gt-t3upsa).
func TestGateRunStoreRotationFailure(t *testing.T) {
	t.Parallel()
	boom := errors.New("disk says no")
	for _, tc := range []struct {
		name  string
		setup func(*memGateRunFS)
	}{
		{"sizing the log", func(m *memGateRunFS) { m.sizeErr = boom }},
		{"reading the log", func(m *memGateRunFS) { m.readErr = boom }},
		{"opening the month file", func(m *memGateRunFS) { m.openFailOn, m.err = "2026-10", boom }},
		{"flushing the gzip into the month file", func(m *memGateRunFS) { m.writeFailOn, m.err = "2026-10", boom }},
		{"closing the month file", func(m *memGateRunFS) { m.clsOn, m.err = "2026-10", boom }},
		{"removing the rotated log", func(m *memGateRunFS) { m.removeErr = boom }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fsys := newMemGateRunFS()
			cur := gateRunsPath("/town")
			old := sampleGateRun("gt-old")
			old.TS = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
			fsys.files[cur] = []byte(mustLine(t, old) + "\n")
			tc.setup(fsys)
			err := appendGateRunLimit(fsys, "/town", sampleGateRun("gt-new"), 1)
			if !errors.Is(err, boom) {
				t.Fatalf("%s: error = %v, want it to wrap %v", tc.name, err, boom)
			}
			if got := beadsIn(t, fsys.files[cur], false); !slices.Equal(got, []string{"gt-old", "gt-new"}) {
				t.Errorf("%s: current log = %v, want the old row kept and the new row appended", tc.name, got)
			}
		})
	}
}

// TestGateRunStoreRotatesEachMonth: a log holding two months' rows leaves one
// gzip file per month, each holding only its own rows (gt-t3upsa).
func TestGateRunStoreRotatesEachMonth(t *testing.T) {
	t.Parallel()
	fsys := newMemGateRunFS()
	cur := gateRunsPath("/town")
	oct, nov := sampleGateRun("gt-oct"), sampleGateRun("gt-nov")
	oct.TS, nov.TS = time.Date(2026, 10, 31, 23, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 1, 0, 0, 0, time.UTC)
	fsys.files[cur] = []byte(mustLine(t, oct) + "\n" + mustLine(t, nov) + "\n")
	if err := appendGateRunLimit(fsys, "/town", sampleGateRun("gt-new"), 1); err != nil {
		t.Fatalf("append: %v", err)
	}
	for file, want := range map[string]string{"gate-runs-2026-10.jsonl.gz": "gt-oct", "gate-runs-2026-11.jsonl.gz": "gt-nov"} {
		if got := beadsIn(t, fsys.files[filepath.Join(filepath.Dir(cur), file)], true); !slices.Equal(got, []string{want}) {
			t.Errorf("%s = %v, want only [%s]", file, got, want)
		}
	}
}

// TestOSGateRunFSSize: a log that does not exist has size 0 and no error, and a
// path the OS cannot stat is an error, not a size (gt-t3upsa).
func TestOSGateRunFSSize(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path string
		size       int64
		wantErr    bool
	}{
		{"a file", file, 5, false},
		{"a missing log", filepath.Join(dir, "none.jsonl"), 0, false},
		{"a path under a file", filepath.Join(file, "x"), 0, true},
	} {
		got, err := osGateRunFS{}.Size(tc.path)
		if got != tc.size || (err != nil) != tc.wantErr {
			t.Errorf("%s: Size = %d, %v; want %d, error %t", tc.name, got, err, tc.size, tc.wantErr)
		}
	}
}

// holdingGateRunFS blocks the rotation's read of the log until released, and
// counts the Size calls so a second append that got past the lock shows.
type holdingGateRunFS struct {
	*memGateRunFS
	inRead  chan struct{}
	release chan struct{}
	once    sync.Once
	relOnce sync.Once
	sizes   atomic.Int32
}

// open releases the blocked read; safe to call more than once, so a Cleanup can
// release it when the test fails before its own release and no append keeps the
// package mutex.
func (h *holdingGateRunFS) open() { h.relOnce.Do(func() { close(h.release) }) }

func (h *holdingGateRunFS) Size(p string) (int64, error) {
	h.sizes.Add(1)
	return h.memGateRunFS.Size(p)
}

func (h *holdingGateRunFS) ReadFile(p string) ([]byte, error) {
	h.once.Do(func() { close(h.inRead) })
	<-h.release
	return h.memGateRunFS.ReadFile(p)
}

// awaitGateRun waits for ch and fails the test rather than hang when it does not
// close within the deadline.
func awaitGateRun(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for %s", what)
	}
}

// assertSecondAppendHeld waits (bounded) for the second append to have started,
// then gives it a bounded number of scheduler yields to reach the filesystem while
// the first holds a rotation open, and asserts that only the first did: with the
// lock held by that rotation the second is blocked on it. Waiting for the start
// means the second goroutine has run up to its call, so a window spent before it
// was scheduled cannot pass this check; the rows read after the release are what
// fail a removed lock, and this check only names the cause earlier.
func assertSecondAppendHeld(t *testing.T, h *holdingGateRunFS, started <-chan struct{}) {
	t.Helper()
	awaitGateRun(t, started, "the second append to start")
	for i := 0; i < 20000 && h.sizes.Load() < 2; i++ {
		runtime.Gosched()
	}
	if n := h.sizes.Load(); n != 1 {
		t.Errorf("%d appends reached the filesystem while the first held a rotation open, want 1", n)
	}
}

// TestGateRunStoreAnAppendDuringARotationIsNotLost: a second append started while
// the first holds a rotation open (its read of the log blocked) does not get past
// the lock until the rotation is done, and afterwards every row is in the log or a
// month file exactly once (gt-6wfk0k). The test is not parallel, so no other test
// holds the log's mutex while it runs.
//
//testpolicy:allow parallel — holds the package-wide gate-run mutex that every other append in this package also takes
func TestGateRunStoreAnAppendDuringARotationIsNotLost(t *testing.T) {
	h := &holdingGateRunFS{memGateRunFS: newMemGateRunFS(), inRead: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(h.open)
	cur := gateRunsPath("/town")
	old := sampleGateRun("gt-old")
	old.TS = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	h.files[cur] = []byte(mustLine(t, old) + "\n")
	bStarted := make(chan struct{})
	appendRow := func(bead string, done chan<- struct{}) {
		r := sampleGateRun(bead)
		r.TS = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
		if bead == "gt-b" {
			close(bStarted)
		}
		if err := appendGateRunLimit(h, "/town", r, 1); err != nil {
			t.Errorf("append %s: %v", bead, err)
		}
		close(done)
	}
	aDone, bDone := make(chan struct{}), make(chan struct{})
	go appendRow("gt-a", aDone)
	awaitGateRun(t, h.inRead, "the first append's rotation to read the log")
	go appendRow("gt-b", bDone)
	assertSecondAppendHeld(t, h, bStarted)
	h.open()
	awaitGateRun(t, aDone, "the first append")
	awaitGateRun(t, bDone, "the second append")
	got := beadsIn(t, h.files[cur], false)
	for path, month := range h.files {
		if path != cur {
			got = append(got, beadsIn(t, month, true)...)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"gt-a", "gt-b", "gt-old"}) {
		t.Errorf("rows after both appends = %v, want gt-a, gt-b and gt-old once each", got)
	}
}

// TestGateRunStoreConcurrentAppendsLoseNothing: appends from many goroutines over a
// log that rotates leave every row exactly once, in the log or the month file
// (gt-6wfk0k; run with -race).
//
//testpolicy:allow parallel — takes the package-wide gate-run mutex, which the lock-held test above must find free of other tests
func TestGateRunStoreConcurrentAppendsLoseNothing(t *testing.T) {
	fsys := newMemGateRunFS()
	var wg sync.WaitGroup
	var want []string
	for i := range 12 {
		bead := fmt.Sprintf("gt-%02d", i)
		want = append(want, bead)
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := sampleGateRun(bead)
			r.TS = time.Date(2026, 10, 1, 12, 0, i, 0, time.UTC)
			if err := appendGateRunLimit(fsys, "/town", r, rotateLimit); err != nil {
				t.Errorf("append %s: %v", bead, err)
			}
		}()
	}
	wg.Wait()
	cur := gateRunsPath("/town")
	got := beadsIn(t, fsys.files[cur], false)
	if month := fsys.files[filepath.Join(filepath.Dir(cur), "gate-runs-2026-10.jsonl.gz")]; month != nil {
		got = append(got, beadsIn(t, month, true)...)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("rows after the appends = %v, want each of %v exactly once", got, want)
	}
}

// TestGateRunStoreTwoAppendsAtTheLimitRotateOnce: two appends arriving at a log
// that has reached the limit rotate it once: the second waits on the lock, finds
// the log small again and appends without rotating, so the month file holds each
// old row once and the two new rows are in the log (gt-53ezs0).
//
//testpolicy:allow parallel — holds the package-wide gate-run mutex that every other append in this package also takes
func TestGateRunStoreTwoAppendsAtTheLimitRotateOnce(t *testing.T) {
	h := &holdingGateRunFS{memGateRunFS: newMemGateRunFS(), inRead: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(h.open)
	cur := gateRunsPath("/town")
	row := func(bead string, day int) GateRun {
		r := sampleGateRun(bead)
		r.TS = time.Date(2026, 10, day, 12, 0, 0, 0, time.UTC)
		return r
	}
	oneRow := len(mustLine(t, row("gt-o1", 1))) + 1
	h.files[cur] = []byte(mustLine(t, row("gt-o1", 1)) + "\n" + mustLine(t, row("gt-o2", 1)) + "\n")
	limit := int64(oneRow + oneRow/2) // the two old rows reach it; one new row does not
	done := map[string]chan struct{}{"gt-n1": make(chan struct{}), "gt-n2": make(chan struct{})}
	n2Started := make(chan struct{})
	appendRow := func(bead string) {
		if bead == "gt-n2" {
			close(n2Started)
		}
		if err := appendGateRunLimit(h, "/town", row(bead, 2), limit); err != nil {
			t.Errorf("append %s: %v", bead, err)
		}
		close(done[bead])
	}
	go appendRow("gt-n1")
	awaitGateRun(t, h.inRead, "the first append's rotation to read the log")
	go appendRow("gt-n2")
	assertSecondAppendHeld(t, h, n2Started)
	h.open()
	awaitGateRun(t, done["gt-n1"], "the first append")
	awaitGateRun(t, done["gt-n2"], "the second append")
	if got := beadsIn(t, h.files[cur], false); !slices.Equal(got, []string{"gt-n1", "gt-n2"}) {
		t.Errorf("log after both appends = %v, want the two new rows [gt-n1 gt-n2]", got)
	}
	month := filepath.Join(filepath.Dir(cur), "gate-runs-2026-10.jsonl.gz")
	if got := beadsIn(t, h.files[month], true); !slices.Equal(got, []string{"gt-o1", "gt-o2"}) {
		t.Errorf("month file = %v, want each old row once [gt-o1 gt-o2]", got)
	}
}

// gzipRows gzips rows as one member, the way a month file's members are written.
func gzipRows(t *testing.T, rows ...GateRun) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	for _, r := range rows {
		if _, err := zw.Write([]byte(mustLine(t, r) + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestGateRunStoreRotationIsIdempotent: a rotation that runs again over rows it
// already archived leaves each row once in the month file, tells two runs of one
// bead apart, and a month file it cannot read or decode is an error that keeps the
// log (gt-5acjcn).
func TestGateRunStoreRotationIsIdempotent(t *testing.T) {
	t.Parallel()
	boom := errors.New("disk says no")
	row := func(bead, run string, sec int) GateRun {
		r := sampleGateRun(bead)
		r.RunID, r.TS = run, time.Date(2026, 10, 1, 12, 0, sec, 0, time.UTC)
		return r
	}
	o1, o2 := row("gt-o1", "r1", 1), row("gt-o2", "r2", 2)
	cur := gateRunsPath("/town")
	month := filepath.Join(filepath.Dir(cur), "gate-runs-2026-10.jsonl.gz")
	for _, tc := range []struct {
		name      string
		setup     func(*memGateRunFS)
		appends   []string
		wantMonth []string // nil: the month file is left as it was
		wantErr   error
	}{
		{"removal fails twice, three appends: each row once", func(m *memGateRunFS) { m.removeErr = boom },
			[]string{"gt-n1", "gt-n2", "gt-n3"}, []string{"gt-n1", "gt-n2", "gt-o1", "gt-o2"}, boom},
		{"a month file already holding part of the group gets the rest", func(m *memGateRunFS) { m.files[month] = gzipRows(t, o1) },
			[]string{"gt-n1"}, []string{"gt-o1", "gt-o2"}, nil},
		{"two runs of one bead stay two rows", func(m *memGateRunFS) {
			m.files[month] = gzipRows(t, o1) // the first run is archived; the second, of the same bead, is not
			m.files[cur] = []byte(mustLine(t, o1) + "\n" + mustLine(t, row("gt-o1", "r9", 3)) + "\n")
		}, []string{"gt-n1"}, []string{"gt-o1", "gt-o1"}, nil},
		{"a month file that cannot be read is an error and the log is kept", func(m *memGateRunFS) {
			m.files[month] = gzipRows(t, o1)
			m.readErr, m.readFailOn = boom, "2026-10"
		}, []string{"gt-n1"}, nil, boom},
		{"a month file that is not gzip is an error and the log is kept", func(m *memGateRunFS) { m.files[month] = []byte("plain text") },
			[]string{"gt-n1"}, nil, gzip.ErrHeader},
		{"a month file cut short is an error and the log is kept", func(m *memGateRunFS) {
			whole := gzipRows(t, o1)
			m.files[month] = whole[:len(whole)-9]
		}, []string{"gt-n1"}, nil, io.ErrUnexpectedEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fsys := newMemGateRunFS()
			fsys.files[cur] = []byte(mustLine(t, o1) + "\n" + mustLine(t, o2) + "\n")
			tc.setup(fsys)
			before := slices.Clone(fsys.files[month])
			var err error
			for _, bead := range tc.appends {
				r := sampleGateRun(bead)
				r.TS = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
				err = errors.Join(err, appendGateRunLimit(fsys, "/town", r, 1))
			}
			if tc.wantErr == nil && err != nil || tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("%s: error = %v, want %v", tc.name, err, tc.wantErr)
			}
			if tc.wantMonth == nil {
				if !bytes.Equal(fsys.files[month], before) {
					t.Errorf("%s: the month file changed", tc.name)
				}
				if got := beadsIn(t, fsys.files[cur], false); !slices.Contains(got, "gt-o1") || !slices.Contains(got, "gt-o2") {
					t.Errorf("%s: log = %v, want the unarchived rows kept", tc.name, got)
				}
				return
			}
			got := beadsIn(t, fsys.files[month], true)
			slices.Sort(got)
			if !slices.Equal(got, tc.wantMonth) {
				t.Errorf("%s: month file = %v, want %v", tc.name, got, tc.wantMonth)
			}
		})
	}
}

// TestGateRunStoreKeepsWhatItCannotDecode: a line the rotation cannot decode is
// appended byte for byte, with its newline, to gate-runs-rejects.jsonl before the
// log is removed, the good rows are archived as before, and a rejects write that
// fails (open, write or close) is an error that removes nothing (gt-a78yu0).
func TestGateRunStoreKeepsWhatItCannotDecode(t *testing.T) {
	t.Parallel()
	boom := errors.New("disk says no")
	cur := gateRunsPath("/town")
	rejects := filepath.Join(filepath.Dir(cur), "gate-runs-rejects.jsonl")
	month := filepath.Join(filepath.Dir(cur), "gate-runs-2026-10.jsonl.gz")
	good := sampleGateRun("gt-good")
	good.TS = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	// A whole gate run whose ts has no zone, a line that is not JSON, and a torn last line.
	noZone := strings.Replace(mustLine(t, good), `"ts":"2026-10-01T12:00:00Z"`, `"ts":"2026-10-01T12:00:00"`, 1)
	const plain, torn = "not json at all", `{"ts":"2026-10-01T12:00:00Z","bead":"gt-to`
	if noZone == mustLine(t, good) {
		t.Fatal("the fixture's ts was not rewritten")
	}
	for _, tc := range []struct {
		name  string
		setup func(*memGateRunFS)
		fail  bool
	}{
		{"every line it cannot decode is kept and the log removed", func(*memGateRunFS) {}, false},
		{"a rejects file that cannot be opened", func(m *memGateRunFS) { m.openFailOn, m.err = "rejects", boom }, true},
		{"a rejects write that fails", func(m *memGateRunFS) { m.writeFailOn, m.err = "rejects", boom }, true},
		{"a rejects file that cannot be closed", func(m *memGateRunFS) { m.clsOn, m.err = "rejects", boom }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fsys := newMemGateRunFS()
			log := mustLine(t, good) + "\n" + noZone + "\n" + plain + "\n" + torn
			fsys.files[cur] = []byte(log)
			tc.setup(fsys)
			err := rotateGateRuns(fsys, cur)
			if got := beadsIn(t, fsys.files[month], true); !slices.Equal(got, []string{"gt-good"}) {
				t.Errorf("month file = %v, want the good row archived", got)
			}
			if tc.fail {
				if !errors.Is(err, boom) || !strings.Contains(err.Error(), "rejects") {
					t.Errorf("error = %v, want it to name the rejects file and wrap %v", err, boom)
				}
				if string(fsys.files[cur]) != log {
					t.Errorf("the log was changed by a failed rejects write: %q", fsys.files[cur])
				}
				return
			}
			if err != nil {
				t.Fatalf("rotateGateRuns: %v", err)
			}
			if want := noZone + "\n" + plain + "\n" + torn + "\n"; string(fsys.files[rejects]) != want {
				t.Errorf("rejects file = %q, want the three lines byte for byte with their newlines %q", fsys.files[rejects], want)
			}
			if _, ok := fsys.files[cur]; ok {
				t.Error("the log was not removed after the rotation")
			}
		})
	}
}

// TestGateRunStoreACleanLogMakesNoRejectsFile: a rotation of a log whose lines all
// decode never opens the rejects file, so no empty one is left beside the month
// files (gt-pvw9j1).
func TestGateRunStoreACleanLogMakesNoRejectsFile(t *testing.T) {
	t.Parallel()
	fsys := newMemGateRunFS()
	cur := gateRunsPath("/town")
	r := sampleGateRun("gt-clean")
	r.TS = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	fsys.files[cur] = []byte(mustLine(t, r) + "\n")
	if err := rotateGateRuns(fsys, cur); err != nil {
		t.Fatalf("rotateGateRuns: %v", err)
	}
	rejects := filepath.Join(filepath.Dir(cur), gateRunRejectsFile)
	if slices.Contains(fsys.opened, rejects) || fsys.files[rejects] != nil {
		t.Errorf("the rejects file was opened or written for a clean log: opened %v", fsys.opened)
	}
	if got := beadsIn(t, fsys.files[filepath.Join(filepath.Dir(cur), "gate-runs-2026-10.jsonl.gz")], true); !slices.Equal(got, []string{"gt-clean"}) {
		t.Errorf("month file = %v, want the clean row archived", got)
	}
}
