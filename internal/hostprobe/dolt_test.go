package hostprobe

import (
	"errors"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

// doltConflict is a conflict line carrying every field the parser reads.
const doltConflict = `time="2026-11-19T00:01:15Z" level=warning msg="error running query" connectionDb=gt connectionID=4845089 error="serialization failure: this transaction conflicts with a committed transaction from another client, try restarting transaction."`

// doltLine builds a conflict line stamped at stamp, counting under db; an empty
// db leaves the key out, as a line the server logged outside a database.
func doltLine(stamp, db string) string {
	line := `time="` + stamp + `" level=warning msg="error running query"`
	if db != "" {
		line += " connectionDb=" + db
	}
	return line + ` error="serialization failure: this transaction conflicts with a committed transaction from another client, try restarting transaction."`
}

// checkDoltBuckets compares a parse result with want bucket by bucket, so a
// failure names the bucket that differs.
func checkDoltBuckets(t *testing.T, got, want []DoltBucket) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d buckets, want %d", len(got), len(want))
	}
	for i := range want {
		if !got[i].At.Equal(want[i].At) {
			t.Errorf("bucket %d At = %v, want %v", i, got[i].At, want[i].At)
		}
		if got[i].Total != want[i].Total {
			t.Errorf("bucket %d Total = %d, want %d", i, got[i].Total, want[i].Total)
		}
		if len(got[i].ByDB) == 0 && len(want[i].ByDB) == 0 {
			continue
		}
		for db, n := range want[i].ByDB {
			if got[i].ByDB[db] != n {
				t.Errorf("bucket %d ByDB[%q] = %d, want %d", i, db, got[i].ByDB[db], n)
			}
		}
		if len(got[i].ByDB) != len(want[i].ByDB) {
			t.Errorf("bucket %d ByDB = %v, want %v", i, got[i].ByDB, want[i].ByDB)
		}
	}
}

func doltAt(hour, min, sec int) time.Time {
	return time.Date(2026, 11, 19, hour, min, sec, 0, time.UTC)
}

func TestParseDoltConflicts(t *testing.T) {
	t.Parallel()
	// now sits in the 00:20 bucket, so three buckets end there: 00:00, 00:10,
	// 00:20. The stamp at 00:09:59 belongs to 00:00 and the one at 00:10:00 to
	// 00:10, which is where a boundary error shows up.
	in := strings.Join([]string{
		doltConflict,
		doltLine("2026-11-19T00:09:59Z", "gt"),
		doltLine("2026-11-19T00:10:00Z", "devops"),
	}, "\n")

	got, err := ParseDoltConflicts(strings.NewReader(in), time.Date(2026, 11, 19, 0, 25, 0, 0, time.UTC), 3)
	if err != nil {
		t.Fatalf("ParseDoltConflicts: %v", err)
	}
	want := []DoltBucket{
		{At: doltAt(0, 0, 0), Total: 2, ByDB: map[string]int{"gt": 2}},
		{At: doltAt(0, 10, 0), Total: 1, ByDB: map[string]int{"devops": 1}},
		{At: doltAt(0, 20, 0), Total: 0, ByDB: map[string]int{}},
	}
	checkDoltBuckets(t, got.Buckets, want)
}

func TestParseDoltConflictsSkipsLines(t *testing.T) {
	t.Parallel()
	// Five lines that must not count: a line of another message, a line of the
	// right message whose error is another one, a line of another message that
	// holds the conflict text, a conflict line with no time, and a conflict
	// line whose time is not a time.
	in := strings.Join([]string{
		`time="2026-11-19T00:05:00Z" level=info msg="starting server"`,
		`time="2026-11-19T00:06:00Z" level=warning msg="error running query" connectionDb=gt error="context canceled"`,
		`time="2026-11-19T00:07:00Z" level=info msg="transaction retry succeeded" connectionDb=gt error="serialization failure: this transaction conflicts with a committed transaction from another client, try restarting transaction."`,
		`level=warning msg="error running query" connectionDb=gt error="serialization failure: this transaction conflicts with a committed transaction from another client, try restarting transaction."`,
		`time="not-a-time" level=warning msg="error running query" connectionDb=gt error="serialization failure: this transaction conflicts with a committed transaction from another client, try restarting transaction."`,
	}, "\n")

	got, err := ParseDoltConflicts(strings.NewReader(in), time.Date(2026, 11, 19, 0, 25, 0, 0, time.UTC), 3)
	if err != nil {
		t.Fatalf("ParseDoltConflicts: %v", err)
	}
	want := []DoltBucket{
		{At: doltAt(0, 0, 0), Total: 0, ByDB: map[string]int{}},
		{At: doltAt(0, 10, 0), Total: 0, ByDB: map[string]int{}},
		{At: doltAt(0, 20, 0), Total: 0, ByDB: map[string]int{}},
	}
	checkDoltBuckets(t, got.Buckets, want)
}

func TestParseDoltConflictsDBKey(t *testing.T) {
	t.Parallel()
	// The key a line counts under: unknown when it names no database or names
	// an empty one, and the name itself when logrus quoted a value with a
	// space in it.
	tests := []struct {
		name string
		line string
		want string
	}{
		{"no connectionDb", doltLine("2026-11-19T00:21:00Z", ""), "unknown"},
		{"empty connectionDb", `time="2026-11-19T00:21:00Z" level=warning msg="error running query" connectionDb="" error="serialization failure: this transaction conflicts with a committed transaction from another client, try restarting transaction."`, "unknown"},
		{"quoted connectionDb", `time="2026-11-19T00:21:00Z" level=warning msg="error running query" connectionDb="two words" error="serialization failure: this transaction conflicts with a committed transaction from another client, try restarting transaction."`, "two words"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseDoltConflicts(strings.NewReader(tt.line), time.Date(2026, 11, 19, 0, 25, 0, 0, time.UTC), 1)
			if err != nil {
				t.Fatalf("ParseDoltConflicts: %v", err)
			}
			want := []DoltBucket{{At: doltAt(0, 20, 0), Total: 1, ByDB: map[string]int{tt.want: 1}}}
			checkDoltBuckets(t, got.Buckets, want)
		})
	}
}

func TestParseDoltConflictsFractionalStamp(t *testing.T) {
	t.Parallel()
	// A stamp with fractional seconds is still a time, and a stamp the parser
	// refused would drop the line it should have counted.
	in := doltLine("2026-11-19T00:21:15.123456789Z", "gt")

	got, err := ParseDoltConflicts(strings.NewReader(in), time.Date(2026, 11, 19, 0, 25, 0, 0, time.UTC), 1)
	if err != nil {
		t.Fatalf("ParseDoltConflicts: %v", err)
	}
	want := []DoltBucket{{At: doltAt(0, 20, 0), Total: 1, ByDB: map[string]int{"gt": 1}}}
	checkDoltBuckets(t, got.Buckets, want)
}

func TestParseDoltConflictsWindow(t *testing.T) {
	t.Parallel()
	// 144 buckets of ten minutes are 24 hours: the first starts 143 buckets
	// before the one holding now, and both ends are UTC.
	got, err := ParseDoltConflicts(strings.NewReader(""), time.Date(2026, 11, 19, 0, 25, 0, 0, time.UTC), 144)
	if err != nil {
		t.Fatalf("ParseDoltConflicts: %v", err)
	}
	if len(got.Buckets) != 144 {
		t.Fatalf("got %d buckets, want 144", len(got.Buckets))
	}
	first := time.Date(2026, 11, 18, 0, 30, 0, 0, time.UTC)
	if !got.Buckets[0].At.Equal(first) {
		t.Errorf("first bucket At = %v, want %v", got.Buckets[0].At, first)
	}
	if loc := got.Buckets[0].At.Location(); loc != time.UTC {
		t.Errorf("first bucket At is in %v, want UTC", loc)
	}
	last := doltAt(0, 20, 0)
	if !got.Buckets[143].At.Equal(last) {
		t.Errorf("last bucket At = %v, want %v", got.Buckets[143].At, last)
	}
}

func TestParseDoltConflictsOutOfWindow(t *testing.T) {
	t.Parallel()
	// Two buckets: 00:10 and 00:20. A line before the window and a line after
	// it are outside every bucket the result holds, and counting either would
	// write past the slice.
	in := strings.Join([]string{
		doltLine("2026-11-19T00:05:00Z", "gt"),
		doltLine("2026-11-19T00:35:00Z", "gt"),
		doltLine("2026-11-19T00:21:00Z", "gt"),
	}, "\n")

	got, err := ParseDoltConflicts(strings.NewReader(in), time.Date(2026, 11, 19, 0, 25, 0, 0, time.UTC), 2)
	if err != nil {
		t.Fatalf("ParseDoltConflicts: %v", err)
	}
	want := []DoltBucket{
		{At: doltAt(0, 10, 0), Total: 0, ByDB: map[string]int{}},
		{At: doltAt(0, 20, 0), Total: 1, ByDB: map[string]int{"gt": 1}},
	}
	checkDoltBuckets(t, got.Buckets, want)
}

func TestParseDoltConflictsRefusals(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 19, 0, 25, 0, 0, time.UTC)

	t.Run("no buckets", func(t *testing.T) {
		t.Parallel()
		if _, err := ParseDoltConflicts(strings.NewReader(""), now, 0); err == nil {
			t.Error("ParseDoltConflicts with 0 buckets returned no error")
		}
	})
	t.Run("negative buckets", func(t *testing.T) {
		t.Parallel()
		if _, err := ParseDoltConflicts(strings.NewReader(""), now, -1); err == nil {
			t.Error("ParseDoltConflicts with -1 buckets returned no error")
		}
	})
	t.Run("reader error", func(t *testing.T) {
		t.Parallel()
		broken := iotest.ErrReader(errors.New("disk read error"))
		if _, err := ParseDoltConflicts(broken, now, 3); err == nil {
			t.Error("ParseDoltConflicts over a failing reader returned no error")
		}
	})
	t.Run("line too long", func(t *testing.T) {
		t.Parallel()
		long := strings.Repeat("x", doltMaxLine+1)
		if _, err := ParseDoltConflicts(strings.NewReader(long), now, 3); err == nil {
			t.Error("ParseDoltConflicts past a line longer than it reads returned no error")
		}
	})
}
