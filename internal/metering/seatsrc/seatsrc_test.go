package seatsrc

import (
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/steveyegge/gastown/internal/metering/export"
)

func utc(year int, month time.Month, day, hour, minute int) time.Time {
	return time.Date(year, month, day, hour, minute, 0, 0, time.UTC)
}

// TestSeatsMapsLinesToIntervals covers the mapping: every valid line becomes
// one export.Seat with the workspace, profile and end instants it names.
func TestSeatsMapsLinesToIntervals(t *testing.T) {
	t.Parallel()
	ledger := `{"session":"s1","rig":"gastown","polecat":"angharad","workspace":"acme","profile":"opus","created_at":"2026-10-01T00:00:00Z","ended_at":"2026-10-01T02:30:00Z"}
{"session":"s2","rig":"gastown","polecat":"dag","workspace":"customer-zero","profile":"sonnet","created_at":"2026-10-02T12:00:00Z","ended_at":"2026-10-02T13:00:00Z","work_bead":"gt-abc"}
`

	res, err := Seats(strings.NewReader(ledger))
	if err != nil {
		t.Fatalf("Seats: %v", err)
	}

	want := []export.Seat{
		{
			Workspace: "acme",
			Profile:   "opus",
			Start:     utc(2026, time.October, 1, 0, 0),
			End:       utc(2026, time.October, 1, 2, 30),
		},
		{
			Workspace: "customer-zero",
			Profile:   "sonnet",
			Start:     utc(2026, time.October, 2, 12, 0),
			End:       utc(2026, time.October, 2, 13, 0),
		},
	}
	if len(res.Seats) != len(want) {
		t.Fatalf("Seats returned %d intervals, want %d", len(res.Seats), len(want))
	}
	for i, got := range res.Seats {
		w := want[i]
		if got.Workspace != w.Workspace || got.Profile != w.Profile ||
			!got.Start.Equal(w.Start) || !got.End.Equal(w.End) {
			t.Errorf("interval %d = %+v, want %+v", i, got, w)
		}
	}
	if len(res.Skipped) != 0 {
		t.Errorf("Skipped = %v, want no entries for two valid lines", res.Skipped)
	}
}

// TestSeatsSkipsAndCountsBadLines covers every refusal branch: each line that
// names no usable interval is skipped under its own reason, and a blank line
// changes nothing.
func TestSeatsSkipsAndCountsBadLines(t *testing.T) {
	t.Parallel()
	lines := []string{
		// Kept: the only usable line in this ledger.
		`{"workspace":"acme","profile":"opus","created_at":"2026-10-01T00:00:00Z","ended_at":"2026-10-01T01:00:00Z"}`,
		"",
		`this is not JSON`,
		`{"workspace":"","profile":"opus","created_at":"2026-10-01T00:00:00Z","ended_at":"2026-10-01T01:00:00Z"}`,
		`{"workspace":"acme","profile":"","created_at":"2026-10-01T00:00:00Z","ended_at":"2026-10-01T01:00:00Z"}`,
		`{"workspace":"acme","profile":"opus","created_at":"0001-01-01T00:00:00Z","ended_at":"2026-10-01T01:00:00Z"}`,
		`{"workspace":"acme","profile":"opus","created_at":"2026-10-01T00:00:00Z","ended_at":"0001-01-01T00:00:00Z"}`,
		`{"workspace":"acme","profile":"opus","created_at":"2026-10-01T02:00:00Z","ended_at":"2026-10-01T01:00:00Z"}`,
	}

	res, err := Seats(strings.NewReader(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatalf("Seats: %v", err)
	}

	want := map[string]int{
		SkipInvalidJSON:    1,
		SkipEmptyWorkspace: 1,
		SkipEmptyProfile:   1,
		SkipZeroTime:       2,
		SkipEndBeforeStart: 1,
	}
	if !reflect.DeepEqual(res.Skipped, want) {
		t.Errorf("Skipped = %v, want %v", res.Skipped, want)
	}
	if len(res.Seats) != 1 {
		t.Errorf("Seats returned %d intervals, want the one valid line", len(res.Seats))
	}
}

// TestSeatsReadsALongLine covers a line past bufio.Scanner's default limit: it
// must not end the read or lose the lines around it.
func TestSeatsReadsALongLine(t *testing.T) {
	t.Parallel()
	pad := strings.Repeat("x", 200*1024)
	long := fmt.Sprintf(
		`{"workspace":"long","profile":"opus","created_at":"2026-10-02T00:00:00Z","ended_at":"2026-10-02T01:00:00Z","pad":%q}`,
		pad,
	)
	ledger := `{"workspace":"first","profile":"sonnet","created_at":"2026-10-01T00:00:00Z","ended_at":"2026-10-01T01:00:00Z"}
` + long + `
{"workspace":"last","profile":"haiku","created_at":"2026-10-03T00:00:00Z","ended_at":"2026-10-03T01:00:00Z"}
`

	res, err := Seats(strings.NewReader(ledger))
	if err != nil {
		t.Fatalf("Seats: %v", err)
	}

	var workspaces []string
	for _, seat := range res.Seats {
		workspaces = append(workspaces, seat.Workspace)
	}
	if got, want := workspaces, []string{"first", "long", "last"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Seats workspaces = %v, want %v", got, want)
	}
	if len(res.Skipped) != 0 {
		t.Errorf("Skipped = %v, want no entries for three valid lines", res.Skipped)
	}
}

// TestSeatsReturnsReaderError covers a failing reader: the error comes back and
// no partial result is handed to the caller, even though one valid line was
// read before the failure.
func TestSeatsReturnsReaderError(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	src := io.MultiReader(
		strings.NewReader(`{"workspace":"acme","profile":"opus","created_at":"2026-10-01T00:00:00Z","ended_at":"2026-10-01T01:00:00Z"}`+"\n"),
		iotest.ErrReader(boom),
	)

	res, err := Seats(src)
	if !errors.Is(err, boom) {
		t.Fatalf("Seats error = %v, want a wrapped %v", err, boom)
	}
	if len(res.Seats) != 0 || len(res.Skipped) != 0 {
		t.Errorf("Seats returned %d intervals and skipped %v with the error, want the zero Result", len(res.Seats), res.Skipped)
	}
}

// TestSeatsCountsAnUnparseableTimestampAsInvalidJSON covers a line the JSON
// decoder rejects for its time rather than its syntax: a well-formed line whose
// created_at or ended_at is not RFC 3339 is counted under invalid JSON, never
// guessed into an interval.
func TestSeatsCountsAnUnparseableTimestampAsInvalidJSON(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"created_at": `{"workspace":"acme","profile":"opus","created_at":"yesterday","ended_at":"2026-10-01T01:00:00Z"}`,
		"ended_at":   `{"workspace":"acme","profile":"opus","created_at":"2026-10-01T00:00:00Z","ended_at":"tomorrow"}`,
	}
	for field, line := range cases {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			res, err := Seats(strings.NewReader(line + "\n"))
			if err != nil {
				t.Fatalf("Seats: %v", err)
			}
			if want := map[string]int{SkipInvalidJSON: 1}; !reflect.DeepEqual(res.Skipped, want) {
				t.Errorf("Skipped = %v, want %v", res.Skipped, want)
			}
			if len(res.Seats) != 0 {
				t.Errorf("Seats returned %d intervals, want none from an unparseable timestamp", len(res.Seats))
			}
		})
	}
}
