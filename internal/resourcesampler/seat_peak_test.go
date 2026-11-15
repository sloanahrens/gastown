package resourcesampler

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
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
