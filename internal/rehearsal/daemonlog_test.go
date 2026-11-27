package rehearsal

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// windowStart and windowEnd bound every window in this file: ten to eleven
// o'clock UTC on the rehearsal day.
var (
	windowStart = time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	windowEnd   = time.Date(2026, 11, 27, 11, 0, 0, 0, time.UTC)
)

// covering is an in-window line with no failure marker. Every input that is
// not meant to be refused as uncovered carries one, so the window counts as
// covered by the log.
const covering = "2026/11/27 10:30:00 daemon: heartbeat ok\n"

// errBoom is the read error the failing reader returns.
var errBoom = errors.New("disk gone")

// failingReader serves data, then returns err instead of EOF.
type failingReader struct {
	data string
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.data == "" {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestRehearsalFailedCallsEachMarker(t *testing.T) {
	t.Parallel()
	markers := []string{
		"i/o timeout",
		"invalid connection",
		"http2 error",
		"service is currently unavailable",
		"did not answer",
		"signal: terminated",
	}
	for _, marker := range markers {
		t.Run(marker, func(t *testing.T) {
			t.Parallel()
			failed := "2026/11/27 10:15:05 bd: call failed: " + marker + "\n"
			got, err := FailedCalls(strings.NewReader(covering+failed), windowStart, windowEnd)
			if err != nil {
				t.Fatalf("FailedCalls with marker %q: unexpected error %v", marker, err)
			}
			want := []string{strings.TrimSuffix(failed, "\n")}
			if !equal(got, want) {
				t.Fatalf("FailedCalls with marker %q returned %q, want %q", marker, got, want)
			}
		})
	}
}

func TestRehearsalFailedCallsWindowBounds(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		stamp string
		want  bool
	}{
		{name: "one second before start is outside", stamp: "2026/11/27 09:59:59", want: false},
		{name: "start is inside", stamp: "2026/11/27 10:00:00", want: true},
		{name: "one second before end is inside", stamp: "2026/11/27 10:59:59", want: true},
		{name: "end is outside", stamp: "2026/11/27 11:00:00", want: false},
		{name: "after end is outside", stamp: "2026/11/27 12:00:00", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			line := tc.stamp + " bd: call failed: i/o timeout"
			got, err := FailedCalls(strings.NewReader(covering+line+"\n"), windowStart, windowEnd)
			if err != nil {
				t.Fatalf("FailedCalls at %s: unexpected error %v", tc.stamp, err)
			}
			if tc.want && !equal(got, []string{line}) {
				t.Fatalf("FailedCalls at %s returned %q, want the line returned", tc.stamp, got)
			}
			if !tc.want && len(got) != 0 {
				t.Fatalf("FailedCalls at %s returned %q, want nothing outside the window", tc.stamp, got)
			}
		})
	}
}

func TestRehearsalFailedCallsSkipsOtherLines(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
	}{
		{
			name:  "in-window line with no marker",
			input: "2026/11/27 10:20:00 gateway: call ok\n",
		},
		{
			name:  "continuation line with a marker and no stamp",
			input: "i/o timeout on the second attempt\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := FailedCalls(strings.NewReader(covering+tc.input), windowStart, windowEnd)
			if err != nil {
				t.Fatalf("FailedCalls: unexpected error %v", err)
			}
			if len(got) != 0 {
				t.Fatalf("FailedCalls returned %q, want no failed calls", got)
			}
		})
	}
}

func TestRehearsalFailedCallsDedupesMessages(t *testing.T) {
	t.Parallel()
	input := "2026/11/27 10:10:00 dolt: invalid connection\n" +
		"2026/11/27 10:11:00 dolt: invalid connection\n" +
		"2026/11/27 10:12:00 bd: did not answer\n" +
		"2026/11/27 10:13:00 dolt: invalid connection\n"
	got, err := FailedCalls(strings.NewReader(input), windowStart, windowEnd)
	if err != nil {
		t.Fatalf("FailedCalls: unexpected error %v", err)
	}
	want := []string{
		"2026/11/27 10:10:00 dolt: invalid connection",
		"2026/11/27 10:12:00 bd: did not answer",
	}
	if !equal(got, want) {
		t.Fatalf("FailedCalls returned %q, want each message once at its first line %q", got, want)
	}
}

func TestRehearsalFailedCallsCutsLines(t *testing.T) {
	t.Parallel()
	t.Run("long ascii line is cut to 300 bytes", func(t *testing.T) {
		t.Parallel()
		line := "2026/11/27 10:05:00 gateway: i/o timeout " + strings.Repeat("x", 400)
		got, err := FailedCalls(strings.NewReader(covering+line+"\n"), windowStart, windowEnd)
		if err != nil {
			t.Fatalf("FailedCalls: unexpected error %v", err)
		}
		if !equal(got, []string{line[:300]}) {
			t.Fatalf("FailedCalls returned %d bytes, want the first 300 bytes of the line", len(got[0]))
		}
	})

	t.Run("cut never splits a multibyte rune", func(t *testing.T) {
		t.Parallel()
		// Byte 300 falls inside a two-byte rune, so the cut backs off to 299.
		line := "2026/11/27 10:05:00 gateway: i/o timeout " + strings.Repeat("é", 200)
		got, err := FailedCalls(strings.NewReader(covering+line+"\n"), windowStart, windowEnd)
		if err != nil {
			t.Fatalf("FailedCalls: unexpected error %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("FailedCalls returned %q, want one line", got)
		}
		if len(got[0]) > 300 || !utf8.ValidString(got[0]) || !strings.HasPrefix(line, got[0]) {
			t.Fatalf("FailedCalls returned %q: want a valid UTF-8 prefix of at most 300 bytes", got[0])
		}
	})

	t.Run("marker past byte 300 still returns the cut line", func(t *testing.T) {
		t.Parallel()
		line := "2026/11/27 10:05:00 gateway: " + strings.Repeat("x", 350) + " i/o timeout"
		got, err := FailedCalls(strings.NewReader(covering+line+"\n"), windowStart, windowEnd)
		if err != nil {
			t.Fatalf("FailedCalls: unexpected error %v", err)
		}
		if !equal(got, []string{line[:300]}) {
			t.Fatalf("FailedCalls returned %d bytes, want the 300-byte cut of a line whose marker is past the cut", len(got[0]))
		}
	})
}

func TestRehearsalFailedCallsParsesInStartLocation(t *testing.T) {
	t.Parallel()
	// The stamp is the daemon's local wall clock: +03:00 here, so 12:30 local
	// is 09:30 UTC and lies outside a UTC window of 10:00 to 11:00.
	zone := time.FixedZone("daemon", 3*60*60)
	start := time.Date(2026, 11, 27, 12, 0, 0, 0, zone)
	end := time.Date(2026, 11, 27, 13, 0, 0, 0, zone)
	line := "2026/11/27 12:30:00 bd: call failed: i/o timeout"
	input := "2026/11/27 12:10:00 daemon: heartbeat ok\n" + line + "\n"

	got, err := FailedCalls(strings.NewReader(input), start, end)
	if err != nil {
		t.Fatalf("FailedCalls: unexpected error %v", err)
	}
	if !equal(got, []string{line}) {
		t.Fatalf("FailedCalls returned %q, want the line stamped in the start's zone", got)
	}
}

func TestRehearsalFailedCallsRefusesUncoveredWindow(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
	}{
		{
			name:  "empty log",
			input: "",
		},
		{
			name:  "only lines outside the window",
			input: "2026/11/27 09:00:00 bd: call failed: i/o timeout\n2026/11/27 12:00:00 bd: ok\n",
		},
		{
			name:  "only lines without a stamp",
			input: "i/o timeout with no stamp\nmore text\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := FailedCalls(strings.NewReader(tc.input), windowStart, windowEnd)
			if err == nil {
				t.Fatalf("FailedCalls returned %q with no error, want an error: the log does not cover the window", got)
			}
			if got != nil {
				t.Fatalf("FailedCalls returned %q with error %v, want no lines on error", got, err)
			}
		})
	}
}

func TestRehearsalFailedCallsReturnsReaderError(t *testing.T) {
	t.Parallel()
	// The reader serves one in-window failure, then fails. The error must come
	// back with no lines, never the partial list read before it.
	r := &failingReader{
		data: covering + "2026/11/27 10:15:05 bd: call failed: i/o timeout\n",
		err:  errBoom,
	}
	got, err := FailedCalls(r, windowStart, windowEnd)
	if !errors.Is(err, errBoom) {
		t.Fatalf("FailedCalls error = %v, want the reader's error %v", err, errBoom)
	}
	if got != nil {
		t.Fatalf("FailedCalls returned %q with a read error, want no lines", got)
	}
}

// equal reports whether two line lists hold the same strings in the same order.
func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
