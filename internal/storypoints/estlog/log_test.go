package estlog

import (
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

// TestAppendWritesOneWholeLine: one Append call appends one newline-terminated
// JSON line, and that line decodes to the record in every field, including the
// absent embedding and component of a shape-only estimate.
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
			town := t.TempDir()
			if err := Append(town, tt.record); err != nil {
				t.Fatalf("Append: %v", err)
			}
			raw, err := os.ReadFile(Path(town))
			if err != nil {
				t.Fatalf("read the log: %v", err)
			}
			if len(raw) == 0 || raw[len(raw)-1] != '\n' {
				t.Fatalf("log = %q, want one line ending in a newline", raw)
			}
			body := string(raw[:len(raw)-1])
			if strings.Contains(body, "\n") {
				t.Fatalf("log holds more than one line: %q", raw)
			}
			var got Record
			if err := json.Unmarshal([]byte(body), &got); err != nil {
				t.Fatalf("the line does not decode: %v\nline: %s", err, body)
			}
			assertRecordEqual(t, got, tt.record)
		})
	}
}

// TestAppendTwoWritersKeepWholeLines: two writers appending at once leave every
// line whole and lose none.
func TestAppendTwoWritersKeepWholeLines(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	const writers, each = 2, 40

	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := Append(town, sampleRecord(fmt.Sprintf("gt-%d-%d", w, i))); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("Append: %v", err)
	}

	raw, err := os.ReadFile(Path(town))
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		t.Fatalf("log does not end in a newline: %q", raw)
	}
	lines := strings.Split(string(raw[:len(raw)-1]), "\n")
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

// TestAppendErrorsNameTheLogPath: a log write that cannot happen returns an
// error naming the log file. A read-only directory does not drive this: a test
// run as root writes into one anyway, so the rows put a file or directory where
// the log needs a writable path.
func TestAppendErrorsNameTheLogPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// block makes the log path or one of its parents unwritable.
		block func(t *testing.T, town string)
	}{
		{
			name: "the log path is a directory",
			block: func(t *testing.T, town string) {
				if err := os.MkdirAll(Path(town), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "a parent of the log path is a file",
			block: func(t *testing.T, town string) {
				if err := os.WriteFile(filepath.Join(town, ".runtime"), []byte("not a directory"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			tt.block(t, town)
			err := Append(town, sampleRecord("gt-p4aqut"))
			if err == nil {
				t.Fatalf("Append = nil, want an error for an unwritable log")
			}
			if !strings.Contains(err.Error(), Path(town)) {
				t.Fatalf("error = %q, want it to name %q", err, Path(town))
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
// value, so the record is refused before any file is created.
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
			town := t.TempDir()
			r := sampleRecord("gt-p4aqut")
			tt.change(&r)
			err := Append(town, r)
			if err == nil {
				t.Fatalf("Append = nil, want the encoding error")
			}
			if !strings.Contains(err.Error(), Path(town)) {
				t.Errorf("error = %q, want it to name %q", err, Path(town))
			}
			if _, statErr := os.Stat(Path(town)); !errors.Is(statErr, os.ErrNotExist) {
				t.Errorf("the log exists after a refused record: stat error = %v", statErr)
			}
		})
	}
}

// fakeFile is the handle appendLineWith writes through; each field makes one
// step of a healthy write fail.
type fakeFile struct {
	writeErr error
	closeErr error
	shortBy  int
}

func (f *fakeFile) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return len(p) - f.shortBy, nil
}

func (f *fakeFile) Close() error { return f.closeErr }

// TestAppendLineFailuresNameThePath: the write, short-write and close failures a
// real file does not offer are reported, each naming the log path, and a close
// error never hides the write error beside it.
func TestAppendLineFailuresNameThePath(t *testing.T) {
	t.Parallel()
	errWrite := errors.New("disk full")
	errClose := errors.New("close failed")
	path := filepath.Join(t.TempDir(), "estimates.jsonl")
	tests := []struct {
		name     string
		file     fakeFile
		wantAll  []error
		wantText string
	}{
		{"write fails", fakeFile{writeErr: errWrite}, []error{errWrite}, ""},
		{"write and close fail", fakeFile{writeErr: errWrite, closeErr: errClose}, []error{errWrite, errClose}, ""},
		{"short write", fakeFile{shortBy: 3}, nil, "short write"},
		{"close fails", fakeFile{closeErr: errClose}, []error{errClose}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := tt.file
			open := func(string) (io.WriteCloser, error) { return &file, nil }
			err := appendLineWith(open, path, []byte("{\"bead_id\":\"gt-p4aqut\"}\n"))
			if err == nil {
				t.Fatalf("appendLineWith = nil, want an error")
			}
			if !strings.Contains(err.Error(), path) {
				t.Errorf("error = %q, want it to name %q", err, path)
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
