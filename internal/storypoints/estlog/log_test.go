package estlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// sampleRecord returns a record with every field set, so a round trip covers
// each one.
func sampleRecord(bead string) Record {
	shape, embed, jev := 9.25, 11.5, 8.0
	return Record{
		BeadID:          bead,
		DescriptionHash: "30224847817fb3d48ae7743dc2be0698c82644fd8cd758c9675f9d161b238fd0",
		Shape: Shape{
			Boxes: 3, Paths: 4, Pkgs: 2, DLen: 7.6, NCons: 2, NLab: 5,
			PodEng: 1, Tests: 3, NBt: 13, RigGT: 1, RigDV: 0, IsHaiku: 0.5,
		},
		Embedding:      []float32{0.125, -1.5, 3, 0.1},
		Minutes:        Minutes{Shape: &shape, Embed: &embed, Jev: &jev},
		BlendedMinutes: 9.6,
		WeightsVersion: "runtime v1",
		Time:           time.Date(2026, 11, 14, 10, 30, 0, 0, time.UTC),
	}
}

// shapeOnlyRecord is an estimate whose only input was the shape minutes.
func shapeOnlyRecord(bead string) Record {
	r := sampleRecord(bead)
	shape := 9.25
	r.Embedding = nil
	r.Minutes = Minutes{Shape: &shape}
	r.BlendedMinutes = shape
	return r
}

// assertRecordEqual reports the first field that differs.
func assertRecordEqual(t *testing.T, got, want Record) {
	t.Helper()
	if got.BeadID != want.BeadID {
		t.Errorf("BeadID = %q, want %q", got.BeadID, want.BeadID)
	}
	if got.DescriptionHash != want.DescriptionHash {
		t.Errorf("DescriptionHash = %q, want %q", got.DescriptionHash, want.DescriptionHash)
	}
	if got.Shape != want.Shape {
		t.Errorf("Shape = %+v, want %+v", got.Shape, want.Shape)
	}
	if !slices.Equal(got.Embedding, want.Embedding) {
		t.Errorf("Embedding = %v, want %v", got.Embedding, want.Embedding)
	}
	if !equalMinutes(got.Minutes, want.Minutes) {
		t.Errorf("Minutes = %+v, want %+v", got.Minutes, want.Minutes)
	}
	if got.BlendedMinutes != want.BlendedMinutes {
		t.Errorf("BlendedMinutes = %v, want %v", got.BlendedMinutes, want.BlendedMinutes)
	}
	if got.WeightsVersion != want.WeightsVersion {
		t.Errorf("WeightsVersion = %q, want %q", got.WeightsVersion, want.WeightsVersion)
	}
	if !got.Time.Equal(want.Time) {
		t.Errorf("Time = %v, want the same instant as %v", got.Time, want.Time)
	}
}

func equalMinutes(a, b Minutes) bool {
	return equalPtr(a.Shape, b.Shape) && equalPtr(a.Embed, b.Embed) && equalPtr(a.Jev, b.Jev)
}

func equalPtr(a, b *float64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// assertOpensForAppend checks every open asked for writing at the log's end and
// never truncation: O_APPEND is what keeps a second writer's lines.
func assertOpensForAppend(t *testing.T, flagCalls []int) {
	t.Helper()
	if len(flagCalls) == 0 {
		t.Fatal("the log was never opened")
	}
	for i, flags := range flagCalls {
		if flags&os.O_APPEND == 0 {
			t.Errorf("open %d asked for %#x, want O_APPEND", i+1, flags)
		}
		if flags&os.O_WRONLY == 0 {
			t.Errorf("open %d asked for %#x, want O_WRONLY", i+1, flags)
		}
		if flags&os.O_CREATE == 0 {
			t.Errorf("open %d asked for %#x, want O_CREATE", i+1, flags)
		}
		if flags&os.O_TRUNC != 0 {
			t.Errorf("open %d asked for %#x, want no O_TRUNC", i+1, flags)
		}
	}
}

func TestPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		townRoot string
		want     string
	}{
		{"town root", "/town", "/town/.runtime/storypoints/estimates.jsonl"},
		{"empty town root names no file", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Path(tt.townRoot); got != tt.want {
				t.Fatalf("Path(%q) = %q, want %q", tt.townRoot, got, tt.want)
			}
		})
	}
}

// TestAppendWritesOneWholeLine: one append is one write of one
// newline-terminated JSON line, and that line decodes to the record in every
// field, including the absent embedding and component of a shape-only
// estimate.
func TestAppendWritesOneWholeLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		record Record
	}{
		{"all components", sampleRecord("gt-p4aqut")},
		{"shape only", shapeOnlyRecord("gt-p4aqut")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fsys := &fakeLogFS{}
			if err := appendRecord(fsys, "/town", tt.record); err != nil {
				t.Fatalf("appendRecord: %v", err)
			}
			writes := fsys.writeCalls()
			if len(writes) != 1 {
				t.Fatalf("appendRecord made %d writes, want one", len(writes))
			}
			line := writes[0]
			if len(line) == 0 || line[len(line)-1] != '\n' {
				t.Fatalf("write = %q, want one line ending in a newline", line)
			}
			body := string(line[:len(line)-1])
			if strings.Contains(body, "\n") {
				t.Fatalf("write holds more than one line: %q", line)
			}
			var got Record
			if err := json.Unmarshal([]byte(body), &got); err != nil {
				t.Fatalf("the line does not decode: %v\nline: %s", err, body)
			}
			assertRecordEqual(t, got, tt.record)
			if opened := fsys.openCalls(); !slices.Equal(opened, []string{Path("/town")}) {
				t.Errorf("opened %q, want once on %q", opened, Path("/town"))
			}
			assertOpensForAppend(t, fsys.openFlagCalls())
		})
	}
}

// TestAppendKeepsEveryLineOfTheTownLog: Append goes through the host
// filesystem and the path Path names, opening the log for append rather than
// truncation, so a second call keeps the first line. This is the single
// real-disk success test: a fake cannot show that the host really appends
// across two opens, and the open flags it would take on trust are what this
// drives. TestAppendReportsTheHostRefusingTheDirectoryOrTheOpen covers the
// host's refusals.
func TestAppendKeepsEveryLineOfTheTownLog(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	records := []Record{sampleRecord("gt-p4aqut"), shapeOnlyRecord("gt-p4aqut")}
	for _, r := range records {
		if err := Append(town, r); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	raw, err := os.ReadFile(Path(town))
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		t.Fatalf("log = %q, want every line ending in a newline", raw)
	}
	lines := strings.Split(string(raw[:len(raw)-1]), "\n")
	if len(lines) != len(records) {
		t.Fatalf("log holds %d lines, want the %d appends: %q", len(lines), len(records), raw)
	}
	for i, line := range lines {
		var got Record
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("line %d does not decode: %v", i+1, err)
		}
		assertRecordEqual(t, got, records[i])
	}
}

// TestAppendReportsTheHostRefusingTheDirectoryOrTheOpen: a refusal from the
// host filesystem, not the fake, must reach the caller naming the log, so each
// one is planted on a temp directory and driven through the exported Append:
// a regular file where the log's directory belongs, then a directory where the
// log itself belongs.
func TestAppendReportsTheHostRefusingTheDirectoryOrTheOpen(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		plant func(t *testing.T, townRoot string)
	}{
		{
			name: "a file where the log directory belongs",
			plant: func(t *testing.T, townRoot string) {
				if err := os.WriteFile(filepath.Join(townRoot, ".runtime"), nil, 0o644); err != nil {
					t.Fatalf("plant the file that blocks the directory: %v", err)
				}
			},
		},
		{
			name: "a directory where the log belongs",
			plant: func(t *testing.T, townRoot string) {
				if err := os.MkdirAll(Path(townRoot), 0o755); err != nil {
					t.Fatalf("plant the directory that blocks the open: %v", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			tt.plant(t, townRoot)
			err := Append(townRoot, sampleRecord("gt-p4aqut"))
			if err == nil {
				t.Fatalf("Append = nil, want the host's refusal")
			}
			if !strings.Contains(err.Error(), Path(townRoot)) {
				t.Errorf("error = %q, want it to name %q", err, Path(townRoot))
			}
		})
	}
}

// TestAppendTwoWritersAskForAppendAndWriteOneLineEach: under two writers, every
// append opens the log for append and hands its record to the file in one Write
// call ending in a newline, and no line is lost. A fake cannot tear a write the
// way the kernel can, so this pins the part the code decides — append mode and
// one write per record — and TestAppendKeepsEveryLineOfTheTownLog drives the
// real O_APPEND.
func TestAppendTwoWritersAskForAppendAndWriteOneLineEach(t *testing.T) {
	t.Parallel()
	fsys := &fakeLogFS{}
	const writers, each = 2, 40

	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := appendRecord(fsys, "/town", sampleRecord(fmt.Sprintf("gt-%d-%d", w, i))); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("appendRecord: %v", err)
	}

	assertOpensForAppend(t, fsys.openFlagCalls())
	writes := fsys.writeCalls()
	if len(writes) != writers*each {
		t.Fatalf("appendRecord made %d writes, want one per record (%d)", len(writes), writers*each)
	}
	for i, write := range writes {
		if bytes.Count(write, []byte("\n")) != 1 || !bytes.HasSuffix(write, []byte("\n")) {
			t.Fatalf("write %d is not exactly one line: %q", i+1, write)
		}
	}
	lines := fsys.logLines()
	if len(lines) != writers*each {
		t.Fatalf("lines = %d, want %d", len(lines), writers*each)
	}
	seen := make(map[string]bool, len(lines))
	for i, line := range lines {
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("line %d is not one whole record: %v\nline: %q", i+1, err, line)
		}
		if seen[r.BeadID] {
			t.Fatalf("bead %s was appended twice", r.BeadID)
		}
		seen[r.BeadID] = true
	}
}

// TestAppendFailuresNameTheLogPath: each step of an append that fails is
// reported, every error naming the log file, and a close error never hides the
// write error beside it.
func TestAppendFailuresNameTheLogPath(t *testing.T) {
	t.Parallel()
	errMkdir := errors.New("mkdir refused")
	errOpen := errors.New("open refused")
	errWrite := errors.New("disk full")
	errClose := errors.New("close failed")
	tests := []struct {
		name     string
		fsys     *fakeLogFS
		wantAll  []error
		wantText string
	}{
		{"the log directory cannot be made", &fakeLogFS{mkdirErr: errMkdir}, []error{errMkdir}, ""},
		{"the log cannot be opened", &fakeLogFS{openErr: errOpen}, []error{errOpen}, ""},
		{"write fails", &fakeLogFS{writeErr: errWrite}, []error{errWrite}, ""},
		{"write and close fail", &fakeLogFS{writeErr: errWrite, closeErr: errClose}, []error{errWrite, errClose}, ""},
		{"short write", &fakeLogFS{shortBy: 3}, nil, "short write"},
		{"close fails", &fakeLogFS{closeErr: errClose}, []error{errClose}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := appendRecord(tt.fsys, "/town", sampleRecord("gt-p4aqut"))
			if err == nil {
				t.Fatalf("appendRecord = nil, want an error")
			}
			if !strings.Contains(err.Error(), Path("/town")) {
				t.Errorf("error = %q, want it to name %q", err, Path("/town"))
			}
			for _, want := range tt.wantAll {
				if !errors.Is(err, want) {
					t.Errorf("error = %v, want it to wrap %v", err, want)
				}
			}
			if tt.wantText != "" && !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantText)
			}
		})
	}
}

// TestAppendRefusesNoTownRoot: an empty town root names no log, so Append
// refuses instead of writing beside the working directory.
func TestAppendRefusesNoTownRoot(t *testing.T) {
	t.Parallel()
	if err := Append("", sampleRecord("gt-p4aqut")); !errors.Is(err, ErrNoTownRoot) {
		t.Fatalf("Append with no town root = %v, want ErrNoTownRoot", err)
	}
}

// TestAppendRefusesUnencodableRecord: a NaN or infinite minute has no JSON
// value, so the record is refused before the filesystem is touched.
func TestAppendRefusesUnencodableRecord(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		change func(*Record)
	}{
		{"NaN shape feature", func(r *Record) { r.Shape.DLen = math.NaN() }},
		{"infinite blended minutes", func(r *Record) { r.BlendedMinutes = math.Inf(1) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fsys := &fakeLogFS{}
			r := sampleRecord("gt-p4aqut")
			tt.change(&r)
			err := appendRecord(fsys, "/town", r)
			if err == nil {
				t.Fatalf("appendRecord = nil, want the encoding error")
			}
			if !strings.Contains(err.Error(), Path("/town")) {
				t.Errorf("error = %q, want it to name %q", err, Path("/town"))
			}
			if made := fsys.mkdirCalls(); len(made) != 0 {
				t.Errorf("the log directory was made for a record that cannot encode: %q", made)
			}
			if opened := fsys.openCalls(); len(opened) != 0 {
				t.Errorf("the log was opened for a record that cannot encode: %q", opened)
			}
		})
	}
}

// fakeLogFS is the filesystem appendRecord writes through, with the log kept in
// memory. Each error field makes one step of a healthy append fail; the
// recorded calls, open flags and writes let a test read what appendRecord asked
// of the filesystem.
type fakeLogFS struct {
	mu       sync.Mutex
	log      bytes.Buffer
	writes   [][]byte
	opened   []openCall
	mkdirs   []string
	mkdirErr error
	openErr  error
	writeErr error
	closeErr error
	shortBy  int
}

// openCall is one OpenAppend the fake was asked for.
type openCall struct {
	path  string
	flags int
}

func (f *fakeLogFS) MkdirAll(dir string, _ os.FileMode) error {
	if f.mkdirErr != nil {
		return f.mkdirErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mkdirs = append(f.mkdirs, dir)
	return nil
}

func (f *fakeLogFS) OpenAppend(path string, flags int) (io.WriteCloser, error) {
	if f.openErr != nil {
		return nil, f.openErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opened = append(f.opened, openCall{path: path, flags: flags})
	// A handle not opened for append, or opened to truncate, starts the file
	// over, as a host open with the same flags would.
	if flags&os.O_APPEND == 0 || flags&os.O_TRUNC != 0 {
		f.log.Reset()
	}
	return &fakeLogFile{fs: f}, nil
}

// fakeLogFile is one handle fakeLogFS hands out. Its writes land end to end in
// the shared log under the fake's lock, as appends to one file do.
type fakeLogFile struct{ fs *fakeLogFS }

func (f *fakeLogFile) Write(p []byte) (int, error) {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if f.fs.writeErr != nil {
		return 0, f.fs.writeErr
	}
	f.fs.writes = append(f.fs.writes, slices.Clone(p))
	n := max(0, len(p)-f.fs.shortBy)
	f.fs.log.Write(p[:n]) // a bytes.Buffer write cannot fail
	return n, nil
}

func (f *fakeLogFile) Close() error { return f.fs.closeErr }

func (f *fakeLogFS) writeCalls() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.writes))
	for i, write := range f.writes {
		out[i] = slices.Clone(write)
	}
	return out
}

func (f *fakeLogFS) openCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.opened))
	for i, open := range f.opened {
		out[i] = open.path
	}
	return out
}

// openFlagCalls returns the flags of every open, in the order they were asked.
func (f *fakeLogFS) openFlagCalls() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]int, len(f.opened))
	for i, open := range f.opened {
		out[i] = open.flags
	}
	return out
}

func (f *fakeLogFS) mkdirCalls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.mkdirs)
}

// logLines returns the log split into its lines.
func (f *fakeLogFS) logLines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	body := strings.TrimSuffix(f.log.String(), "\n")
	if body == "" {
		return nil
	}
	return strings.Split(body, "\n")
}
