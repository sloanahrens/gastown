package land

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
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
func (f *fakeGateRunFS) Close() error { return f.closeErr }
