package resourcesampler

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/session"
)

// peakStarted and peakEnded bound the seat's life: ten minutes.
var (
	peakStarted = time.Date(2026, 11, 15, 8, 0, 0, 0, time.UTC)
	peakEnded   = peakStarted.Add(10 * time.Minute)
)

// peakSeat is a seat whose record names its rig, polecat and bead.
func peakSeat() Seat {
	return Seat{Name: "gt-sbx-nux", Rig: "gastown", Polecat: "nux", Bead: "gt-7l5fbu"}
}

// peakFiles is the text of a healthy seat's peak files, keyed by file name:
// 300 s of CPU over a 600 s life is 50 percent of one core.
func peakFiles() map[string]string {
	return map[string]string{
		"cpu.stat":    "usage_usec 300000000\nnr_periods 0\n",
		"memory.peak": "2147483648\n",
		"memory.max":  "4294967296\n",
	}
}

// peakRead renders the text one peak read prints: each file under its marker,
// then the end marker. A file the test leaves out prints no section.
func peakRead(files map[string]string) string {
	var text strings.Builder
	for _, name := range seatPeakFiles {
		body, ok := files[name]
		if !ok {
			continue
		}
		text.WriteString("@@ " + name + " @@\n" + body)
	}
	text.WriteString("@@ end @@\n")
	return text.String()
}

// peakJSON encodes row as the line the peaks file would hold and decodes it
// into the keys a reader sees.
func peakJSON(t *testing.T, row PeakRow) map[string]any {
	t.Helper()
	data, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("encode the peak row: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("decode the peak row: %v", err)
	}
	return fields
}

func TestSeatPeakRowCarriesThePeaksFields(t *testing.T) {
	t.Parallel()
	row, err := SeatPeakRow(peakSeat(), peakStarted, peakEnded, peakRead(peakFiles()))
	if err != nil {
		t.Fatalf("SeatPeakRow: %v", err)
	}
	fields := peakJSON(t, row)
	want := map[string]any{
		"bead":            "gt-7l5fbu",
		"ended_at":        "2026-11-15T08:10:00Z",
		"peak_mem_bytes":  float64(2147483648),
		"peak_cpu_pct":    50.0,
		"mem_limit_bytes": float64(4294967296),
	}
	for key, value := range want {
		if got := fields[key]; got != value {
			t.Errorf("%s = %v, want %v", key, got, value)
		}
	}
}

func TestSeatPeakRowWithNoBeadWritesANullBead(t *testing.T) {
	t.Parallel()
	seat := peakSeat()
	seat.Bead = ""
	row, err := SeatPeakRow(seat, peakStarted, peakEnded, peakRead(peakFiles()))
	if err != nil {
		t.Fatalf("SeatPeakRow: %v", err)
	}
	fields := peakJSON(t, row)
	bead, present := fields["bead"]
	if !present || bead != nil {
		t.Errorf("bead = %v (present %v), want a null bead", bead, present)
	}
	if got := fields["peak_mem_bytes"]; got != float64(2147483648) {
		t.Errorf("peak_mem_bytes = %v, want the seat's memory.peak still written", got)
	}
}

func TestSeatPeakRowWritesTheMemoryLimitNullWhenUnlimited(t *testing.T) {
	t.Parallel()
	files := peakFiles()
	files["memory.max"] = "max\n"
	row, err := SeatPeakRow(peakSeat(), peakStarted, peakEnded, peakRead(files))
	if err != nil {
		t.Fatalf("SeatPeakRow: %v", err)
	}
	if got := peakJSON(t, row)["mem_limit_bytes"]; got != nil {
		t.Errorf("mem_limit_bytes = %v, want null for a seat with no limit", got)
	}
}

func TestSeatPeakRowLeavesCPUNullWhenTheLifeIsUnknown(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		startedAt time.Time
		endedAt   time.Time
	}{
		{name: "no start time", startedAt: time.Time{}, endedAt: peakEnded},
		{name: "end equals start", startedAt: peakEnded, endedAt: peakEnded},
		{name: "end before start", startedAt: peakEnded, endedAt: peakStarted},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row, err := SeatPeakRow(peakSeat(), tc.startedAt, tc.endedAt, peakRead(peakFiles()))
			if err != nil {
				t.Fatalf("SeatPeakRow: %v", err)
			}
			if got := peakJSON(t, row)["peak_cpu_pct"]; got != nil {
				t.Errorf("peak_cpu_pct = %v, want null with no life to divide by", got)
			}
		})
	}
}

func TestSeatPeakRowRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		seat    Seat
		endedAt time.Time
		text    string
		// want is a substring of the refusal, so each row is refused by its
		// own branch and not by a neighbour that happens to catch it.
		want string
	}{
		{name: "no seat name", seat: Seat{}, endedAt: peakEnded, text: peakRead(peakFiles()), want: "has no name"},
		{name: "a name that is not a sandbox name", seat: Seat{Name: "Gt_Sbx"}, endedAt: peakEnded, text: peakRead(peakFiles()), want: "is not a sandbox name"},
		{name: "no end time", seat: peakSeat(), endedAt: time.Time{}, text: peakRead(peakFiles()), want: "has no end time"},
		{name: "a read cut before its end marker", seat: peakSeat(), endedAt: peakEnded,
			text: "@@ cpu.stat @@\nusage_usec 1\n@@ memory.peak @@\n1\n@@ memory.max @@\nmax\n", want: "ends before its end marker"},
		{name: "a file the peak read does not name", seat: peakSeat(), endedAt: peakEnded,
			text: strings.Replace(peakRead(peakFiles()), "@@ end @@\n", "@@ memory.current @@\n1\n@@ end @@\n", 1), want: "is not a peak file"},
		{name: "a file printed twice", seat: peakSeat(), endedAt: peakEnded,
			text: "@@ cpu.stat @@\nusage_usec 1\n@@ cpu.stat @@\nusage_usec 1\n@@ memory.peak @@\n1\n@@ memory.max @@\nmax\n@@ end @@\n", want: "twice"},
		{name: "text after the end marker", seat: peakSeat(), endedAt: peakEnded,
			text: peakRead(peakFiles()) + "stray\n", want: "after its end marker"},
		{name: "a file marker after the end marker", seat: peakSeat(), endedAt: peakEnded,
			text: peakRead(peakFiles()) + "@@ memory.peak @@\n1\n", want: "after its end marker"},
		{name: "text before any file marker", seat: peakSeat(), endedAt: peakEnded,
			text: "stray\n" + peakRead(peakFiles()), want: "before any file marker"},
		{name: "memory.peak missing", seat: peakSeat(), endedAt: peakEnded,
			text: peakRead(withoutFile(peakFiles(), "memory.peak")), want: "prints no memory.peak"},
		{name: "memory.max missing", seat: peakSeat(), endedAt: peakEnded,
			text: peakRead(withoutFile(peakFiles(), "memory.max")), want: "prints no memory.max"},
		{name: "memory.peak empty", seat: peakSeat(), endedAt: peakEnded,
			text: peakRead(withFile(peakFiles(), "memory.peak", "")), want: "is not a byte count"},
		{name: "memory.peak not a count", seat: peakSeat(), endedAt: peakEnded,
			text: peakRead(withFile(peakFiles(), "memory.peak", "lots\n")), want: "is not a byte count"},
		{name: "memory.max not a limit", seat: peakSeat(), endedAt: peakEnded,
			text: peakRead(withFile(peakFiles(), "memory.max", "many\n")), want: "memory.max"},
		{name: "memory.max empty", seat: peakSeat(), endedAt: peakEnded,
			text: peakRead(withFile(peakFiles(), "memory.max", "")), want: "memory.max is empty"},
		{name: "memory.max only a newline", seat: peakSeat(), endedAt: peakEnded,
			text: peakRead(withFile(peakFiles(), "memory.max", "\n")), want: "memory.max is empty"},
		{name: "cpu.stat without usage_usec", seat: peakSeat(), endedAt: peakEnded,
			text: peakRead(withFile(peakFiles(), "cpu.stat", "nr_periods 0\n")), want: "carries no usage_usec"},
		{name: "cpu.stat with a negative usage_usec", seat: peakSeat(), endedAt: peakEnded,
			text: peakRead(withFile(peakFiles(), "cpu.stat", "usage_usec -5\n")), want: "is not a count of microseconds"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			row, err := SeatPeakRow(tc.seat, peakStarted, tc.endedAt, tc.text)
			if err == nil {
				t.Fatalf("SeatPeakRow wrote %+v, want a refusal", row)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal = %q, want it to say %q", err, tc.want)
			}
		})
	}
}

// withFile is files with one file's text replaced.
func withFile(files map[string]string, name, body string) map[string]string {
	out := make(map[string]string, len(files))
	for k, v := range files {
		out[k] = v
	}
	out[name] = body
	return out
}

// withoutFile is files with one file left out: a read that prints no section.
func withoutFile(files map[string]string, name string) map[string]string {
	out := make(map[string]string, len(files))
	for k, v := range files {
		if k != name {
			out[k] = v
		}
	}
	return out
}

// peakRecord is the sandbox record of the seat peakSeat names: its creation
// stamp starts the seat's life.
func peakRecord() session.SandboxRecord {
	return session.SandboxRecord{
		Session:   peakSeat().Name,
		Rig:       "gastown",
		Polecat:   "nux",
		WorkBead:  "gt-7l5fbu",
		CreatedAt: peakStarted,
	}
}

// peakReadText is the text one run of seatPeakReadArgv prints for files: the
// argv is run in the in-process shell the live read's test uses, so the
// recorder's test reads a text the production command really prints.
func peakReadText(t *testing.T, files map[string]string) string {
	t.Helper()
	text, err := seatShell(t, seatPeakReadArgv(), files)
	if err != nil {
		t.Fatalf("running the seat's peak read: %v", err)
	}
	return text
}

// peakExec is the sandbox exec seam recordSeatPeak reads through: it answers
// with text, or with err, and records the argv it was asked to run.
type peakExec struct {
	text  string
	err   error
	calls [][]string
}

func (e *peakExec) Exec(_ context.Context, _ string, env map[string]string, argv []string, _ []byte) ([]byte, []byte, error) {
	e.calls = append(e.calls, append([]string(nil), argv...))
	if len(env) > 0 {
		return nil, nil, errors.New("the peak read takes no env")
	}
	if e.err != nil {
		return nil, nil, e.err
	}
	return []byte(e.text), nil, nil
}

// peakLines reads the lines of town's bead-peaks.jsonl, or nil when the file
// was never written.
func peakLines(t *testing.T, townRoot string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(townRoot, ".runtime", "metrics", "resources", seatPeaksFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("reading the peaks file: %v", err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
}

// TestSeatPeakReadArgvPrintsTheShapeTheRowBuilderParses runs the command a
// seat's end read runs and hands what it printed to the row builder: the three
// files reach the row, and a file the kernel does not publish prints its
// marker with no text, which the builder refuses for the limit it must have.
func TestSeatPeakReadArgvPrintsTheShapeTheRowBuilderParses(t *testing.T) {
	t.Parallel()
	row, err := SeatPeakRow(peakSeat(), peakStarted, peakEnded, peakReadText(t, peakFiles()))
	if err != nil {
		t.Fatalf("the read's text: %v", err)
	}
	if row.PeakMemBytes != 1<<31 || row.MemLimitBytes == nil || *row.MemLimitBytes != 1<<32 {
		t.Errorf("memory = %d of %v bytes, want 2GiB of 4GiB", row.PeakMemBytes, row.MemLimitBytes)
	}
	if row.PeakCPUPct == nil || *row.PeakCPUPct != 50 {
		t.Errorf("peak_cpu_pct = %s, want 50", floatText(row.PeakCPUPct))
	}
	if _, err := SeatPeakRow(peakSeat(), peakStarted, peakEnded, peakReadText(t, withoutFile(peakFiles(), "memory.max"))); err == nil {
		t.Error("the read of a seat with no memory.max wrote a row, want a refusal")
	}
}

// TestSeatPeakEndWritesTheRow shows a seat's end read becoming one line of the
// peaks file: the row carries the seat's bead, its life and its peaks, and the
// recorder reads the files through the command the seat's read runs.
func TestSeatPeakEndWritesTheRow(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	exec := &peakExec{text: peakReadText(t, peakFiles())}
	if err := recordSeatPeak(context.Background(), town, exec, peakRecord(), peakEnded); err != nil {
		t.Fatalf("recordSeatPeak: %v", err)
	}
	if len(exec.calls) != 1 || !reflect.DeepEqual(exec.calls[0], seatPeakReadArgv()) {
		t.Fatalf("the recorder ran %v, want the seat read's argv", exec.calls)
	}
	lines := peakLines(t, town)
	if len(lines) != 1 {
		t.Fatalf("%d lines in the peaks file, want one: %v", len(lines), lines)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &fields); err != nil {
		t.Fatalf("the line is not a JSON object: %v: %q", err, lines[0])
	}
	want := map[string]any{
		"v":               float64(1),
		"bead":            "gt-7l5fbu",
		"sandbox":         peakSeat().Name,
		"started_at":      "2026-11-15T08:00:00Z",
		"ended_at":        "2026-11-15T08:10:00Z",
		"peak_mem_bytes":  float64(1 << 31),
		"peak_cpu_pct":    50.0,
		"mem_limit_bytes": float64(1 << 32),
		"source":          "sandbox",
	}
	for key, value := range want {
		if got := fields[key]; got != value {
			t.Errorf("%s = %v, want %v", key, got, value)
		}
	}
}

// TestSeatPeakEndWritesNoRowWhenItCannot shows every refusal writing no line:
// a read that failed, a text the row builder refuses, a seat name that must
// not reach the exec, and a town root that would put the row in the working
// directory.
func TestSeatPeakEndWritesNoRowWhenItCannot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		exec *peakExec
		rec  session.SandboxRecord
		// want is a substring of the refusal, so each row fails by its own
		// branch and not by a neighbour that happens to catch it.
		want string
		// wantCalls is how many times the read reached the exec seam.
		wantCalls int
	}{
		{
			name: "a read that failed", exec: &peakExec{err: errors.New("gateway refused the exec")},
			rec: peakRecord(), want: "reading its peak files", wantCalls: 1,
		},
		{
			name: "a text the row builder refuses", exec: &peakExec{text: peakRead(withoutFile(peakFiles(), "memory.peak"))},
			rec: peakRecord(), want: "prints no memory.peak", wantCalls: 1,
		},
		{
			name: "a seat name that is not a sandbox name", exec: &peakExec{text: peakReadText(t, peakFiles())},
			rec: session.SandboxRecord{Session: "Gt_Sbx", CreatedAt: peakStarted}, want: "is not a sandbox name",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			town := t.TempDir()
			err := recordSeatPeak(context.Background(), town, tc.exec, tc.rec, peakEnded)
			if err == nil {
				t.Fatal("recordSeatPeak = nil, want the refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal = %q, want it to say %q", err, tc.want)
			}
			if len(tc.exec.calls) != tc.wantCalls {
				t.Errorf("the read reached the exec seam %d times, want %d", len(tc.exec.calls), tc.wantCalls)
			}
			if lines := peakLines(t, town); len(lines) != 0 {
				t.Errorf("the peaks file holds %v, want no line", lines)
			}
		})
	}
}

// TestSeatPeakEndRefusesAnEmptyTownRoot pins that a recorder with no town names
// no peaks file: the read is not even run, since its row could only land in the
// working directory.
func TestSeatPeakEndRefusesAnEmptyTownRoot(t *testing.T) {
	t.Parallel()
	exec := &peakExec{text: peakReadText(t, peakFiles())}
	err := recordSeatPeak(context.Background(), "", exec, peakRecord(), peakEnded)
	if err == nil || !strings.Contains(err.Error(), "town root is empty") {
		t.Fatalf("recordSeatPeak = %v, want a refusal naming the town root", err)
	}
	if len(exec.calls) != 0 {
		t.Errorf("the read ran %v before the town root was checked", exec.calls)
	}
}

// TestSeatPeakEndRefusesANilExec shows the seam itself is checked: a recorder
// with no exec cannot read anything, and says so instead of writing a row of
// nothing.
func TestSeatPeakEndRefusesANilExec(t *testing.T) {
	t.Parallel()
	err := recordSeatPeak(context.Background(), t.TempDir(), nil, peakRecord(), peakEnded)
	if err == nil || !strings.Contains(err.Error(), "no exec seam") {
		t.Fatalf("recordSeatPeak = %v, want a refusal naming the exec seam", err)
	}
}

// TestSeatPeakAppendRefusesWhatItCannotWrite pins every way the peaks file is
// not written: a row that cannot be encoded leaves no file, and an empty
// metrics directory, a resources path that is not a directory and a peaks file
// that cannot be opened are refusals rather than a silent loss.
func TestSeatPeakAppendRefusesWhatItCannotWrite(t *testing.T) {
	t.Parallel()
	nan := math.NaN()
	tests := []struct {
		name string
		// empty passes no metrics directory at all.
		empty bool
		row   PeakRow
		// prepare builds the tree the append must refuse.
		prepare func(t *testing.T, metricsDir string)
		want    string
	}{
		{
			name: "an empty metrics directory", empty: true, want: "metrics directory is empty",
		},
		{
			name: "a row JSON cannot encode", row: PeakRow{V: 1, PeakCPUPct: &nan}, want: "encode row",
		},
		{
			name: "a resources path that is a file", want: "rows directory",
			prepare: func(t *testing.T, metricsDir string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(metricsDir, rowsSubdir), []byte("not a directory\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "a peaks file that cannot be opened", want: "peaks file",
			prepare: func(t *testing.T, metricsDir string) {
				t.Helper()
				if err := os.MkdirAll(filepath.Join(metricsDir, rowsSubdir, seatPeaksFile), 0o755); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			metricsDir := t.TempDir()
			if tc.empty {
				metricsDir = ""
			}
			if tc.prepare != nil {
				tc.prepare(t, metricsDir)
			}
			err := appendSeatPeak(metricsDir, tc.row)
			if err == nil {
				t.Fatal("appendSeatPeak = nil, want the refusal")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("refusal = %q, want it to say %q", err, tc.want)
			}
		})
	}
}

// TestSeatPeakRecorderIsLinked pins the init that hands the session package
// its recorder: session cannot import this package, so a lost link would
// silently stop every seat's peak row.
func TestSeatPeakRecorderIsLinked(t *testing.T) {
	t.Parallel()
	if session.SeatPeakRecorder == nil {
		t.Fatal("session.SeatPeakRecorder is nil, want recordSeatPeak linked")
	}
	if got, want := reflect.ValueOf(session.SeatPeakRecorder).Pointer(), reflect.ValueOf(recordSeatPeak).Pointer(); got != want {
		t.Errorf("session.SeatPeakRecorder = %#x, want recordSeatPeak %#x", got, want)
	}
}
