package deepreview

import (
	"testing"
	"time"
)

func utc(year int, month time.Month, day, hour, min, sec, nsec int) time.Time {
	return time.Date(year, month, day, hour, min, sec, nsec, time.UTC)
}

func TestParseTimestamp(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   string
		want time.Time
	}{
		{"whole seconds in UTC", "2026-10-10T05:00:00Z", utc(2026, 10, 10, 5, 0, 0, 0)},
		{"a fraction is the instant", "2026-10-10T05:00:00.542005Z", utc(2026, 10, 10, 5, 0, 0, 542005000)},
		{"a short fraction is padded", "2026-10-10T05:00:00.9Z", utc(2026, 10, 10, 5, 0, 0, 900000000)},
		{"an offset shifts the instant", "2026-10-10T06:30:00+02:00", utc(2026, 10, 10, 4, 30, 0, 0)},
		{"a negative offset shifts the other way", "2026-10-10T00:30:00-05:00", utc(2026, 10, 10, 5, 30, 0, 0)},
	} {
		ts, err := ParseTimestamp(tc.in)
		if err != nil {
			t.Errorf("%s: ParseTimestamp(%q) error: %v", tc.name, tc.in, err)
			continue
		}
		if !ts.Time.Equal(tc.want) {
			t.Errorf("%s: ParseTimestamp(%q).Time = %s, want %s", tc.name, tc.in, ts.Time, tc.want)
		}
		if ts.Text != tc.in {
			t.Errorf("%s: ParseTimestamp(%q).Text = %q, want the ledger's spelling", tc.name, tc.in, ts.Text)
		}
	}
}

func TestParseTimestampRefusesWhatIsNotOne(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		"",
		"not-a-time",
		"yesterday",
		"2026-10-10",
		"2026-10-10T05:00:00",   // no offset
		"2026-10-10T05:00:00.Z", // a fraction with no digits
		"2026-13-10T05:00:00Z",  // month 13
		"2026-10-10 05:00:00Z",  // a space instead of T
	} {
		if ts, err := ParseTimestamp(in); err == nil {
			t.Errorf("ParseTimestamp(%q) = %v, want an error", in, ts)
		}
	}
}

func TestTimestampOrdering(t *testing.T) {
	t.Parallel()
	// The order the shell's ledger test checks: a fraction after its whole
	// second is newer, and +02:00 is 04:30Z, before both.
	for _, tc := range []struct {
		name string
		old  string
		new  string
	}{
		{"a fraction is after its whole second", "2026-10-10T05:00:00Z", "2026-10-10T05:00:00.542005Z"},
		{"a larger fraction is later", "2026-10-10T05:00:00.542005Z", "2026-10-10T05:00:00.9Z"},
		{"an offset compares as an instant", "2026-10-10T06:30:00+02:00", "2026-10-10T05:00:00Z"},
	} {
		older, err := ParseTimestamp(tc.old)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		newer, err := ParseTimestamp(tc.new)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if !newer.After(older) || !older.Before(newer) {
			t.Errorf("%s: want %s after %s", tc.name, tc.new, tc.old)
		}
		if newer.Before(older) || older.After(newer) {
			t.Errorf("%s: %s must not be before %s", tc.name, tc.new, tc.old)
		}
	}
}

func TestTimestampMarshalsAsTheLedgerSpeltIt(t *testing.T) {
	t.Parallel()
	ts, err := ParseTimestamp("2026-10-10T03:00:00.123456Z")
	if err != nil {
		t.Fatal(err)
	}
	got, err := ts.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if want := `"2026-10-10T03:00:00.123456Z"`; string(got) != want {
		t.Errorf("MarshalJSON() = %s, want %s", got, want)
	}
}

func TestParseCursor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		notes   string
		want    Cursor
		found   bool
		wantErr bool
	}{
		{
			name:  "no cursor line",
			notes: "reviewed: 2026-10-10 internal/a/one.go\nqueued: 2026-10-10T01:00:00Z gt-9",
		},
		{
			name:  "empty notes",
			notes: "",
		},
		{
			name:  "a cursor with a slice path",
			notes: "reviewed: x\ncursor: 2026-10-10T03:00:00.5Z internal/a/one.go\n",
			want:  Cursor{At: Timestamp{Time: utc(2026, 10, 10, 3, 0, 0, 500000000), Text: "2026-10-10T03:00:00.5Z"}, Path: "internal/a/one.go"},
			found: true,
		},
		{
			name:  "a cursor without a path",
			notes: "cursor: 2026-10-10T03:00:00.5Z\n",
			want:  Cursor{At: Timestamp{Time: utc(2026, 10, 10, 3, 0, 0, 500000000), Text: "2026-10-10T03:00:00.5Z"}},
			found: true,
		},
		{
			name:  "a cursor with a trailing space and no path",
			notes: "cursor: 2026-10-10T03:00:00.5Z \n",
			want:  Cursor{At: Timestamp{Time: utc(2026, 10, 10, 3, 0, 0, 500000000), Text: "2026-10-10T03:00:00.5Z"}},
			found: true,
		},
		{
			name: "the newest timestamp wins whatever the order",
			notes: "cursor: 2026-10-01T00:00:00Z internal/a/one.go\n" +
				"cursor: 2026-10-10T01:00:00.5Z internal/b/four.go\n" +
				"cursor: 2026-10-05T00:00:00Z internal/a/two.go\n",
			want:  Cursor{At: Timestamp{Time: utc(2026, 10, 10, 1, 0, 0, 500000000), Text: "2026-10-10T01:00:00.5Z"}, Path: "internal/b/four.go"},
			found: true,
		},
		{
			name: "a newer cursor without a path takes its empty path",
			notes: "cursor: 2026-10-10T01:00:00Z internal/a/one.go\n" +
				"cursor: 2026-10-10T03:00:00Z\n" +
				"cursor: 2026-10-10T02:00:00Z internal/b/two.go\n",
			want:  Cursor{At: Timestamp{Time: utc(2026, 10, 10, 3, 0, 0, 0), Text: "2026-10-10T03:00:00Z"}},
			found: true,
		},
		{
			name:  "a path holding spaces is carried whole",
			notes: "cursor: 2026-10-10T03:00:00Z internal/a dir/one.go\n",
			want:  Cursor{At: Timestamp{Time: utc(2026, 10, 10, 3, 0, 0, 0), Text: "2026-10-10T03:00:00Z"}, Path: "internal/a dir/one.go"},
			found: true,
		},
		{
			name:  "a line that only starts cursor: is not a cursor line",
			notes: "cursor:2026-10-10T03:00:00Z internal/a/one.go\n cursor: 2026-10-10T03:00:00Z internal/a/one.go\n",
		},
		{
			name:    "a cursor with no timestamp is malformed",
			notes:   "cursor: \n",
			wantErr: true,
		},
		{
			name:    "a cursor whose timestamp is not a time is malformed",
			notes:   "cursor: not-a-time internal/a/one.go\n",
			wantErr: true,
		},
		{
			name:    "a tab where the path's space goes is malformed",
			notes:   "cursor: 2026-10-10T03:00:00Z\tinternal/a/one.go\n",
			wantErr: true,
		},
		{
			name:    "a malformed line is refused even when a newer one parses",
			notes:   "cursor: 2026-10-10T03:00:00Z\ncursor: not-a-time\n",
			wantErr: true,
		},
	} {
		got, found, err := ParseCursor(tc.notes)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: ParseCursor(%q) = %+v, %v; want an error", tc.name, tc.notes, got, found)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: ParseCursor(%q) error: %v", tc.name, tc.notes, err)
			continue
		}
		if found != tc.found {
			t.Errorf("%s: ParseCursor(%q) found = %v, want %v", tc.name, tc.notes, found, tc.found)
			continue
		}
		if !found {
			continue
		}
		if !got.At.Time.Equal(tc.want.At.Time) || got.At.Text != tc.want.At.Text || got.Path != tc.want.Path {
			t.Errorf("%s: ParseCursor(%q) = %+v, want %+v", tc.name, tc.notes, got, tc.want)
		}
	}
}

func TestReviewWindow(t *testing.T) {
	t.Parallel()
	now := utc(2026, 10, 10, 10, 0, 0, 0)
	for _, tc := range []struct {
		name    string
		notes   string
		now     time.Time
		want    Window
		wantErr bool
	}{
		{
			name:  "no cursor reviews the day before now",
			notes: "reviewed: 2026-10-09 internal/a/one.go\n",
			now:   now,
			want:  Window{Since: Timestamp{Time: utc(2026, 10, 9, 10, 0, 0, 0), Text: "2026-10-09T10:00:00Z"}},
		},
		{
			name: "a sub-second now truncates as the shell's todate does",
			now:  utc(2026, 10, 10, 10, 0, 0, 750000000),
			want: Window{Since: Timestamp{Time: utc(2026, 10, 9, 10, 0, 0, 0), Text: "2026-10-09T10:00:00Z"}},
		},
		{
			name: "an offset now writes the window in UTC",
			now:  time.Date(2026, 10, 10, 12, 0, 0, 0, time.FixedZone("CEST", 2*60*60)),
			want: Window{Since: Timestamp{Time: utc(2026, 10, 9, 10, 0, 0, 0), Text: "2026-10-09T10:00:00Z"}},
		},
		{
			name:  "a cursor is the window start, spelling kept",
			notes: "cursor: 2026-10-10T03:00:00.123456Z internal/a/two.go\n",
			now:   now,
			want: Window{
				Since:  Timestamp{Time: utc(2026, 10, 10, 3, 0, 0, 123456000), Text: "2026-10-10T03:00:00.123456Z"},
				Cursor: "internal/a/two.go",
			},
		},
		{
			name:  "a cursor at now is not in the future",
			notes: "cursor: 2026-10-10T10:00:00Z internal/a/one.go\n",
			now:   now,
			want: Window{
				Since:  Timestamp{Time: now, Text: "2026-10-10T10:00:00Z"},
				Cursor: "internal/a/one.go",
			},
		},
		{
			name:    "a cursor after now is an error",
			notes:   "cursor: 2026-10-10T10:00:01Z internal/a/one.go\n",
			now:     now,
			wantErr: true,
		},
		{
			name:    "a malformed cursor is an error",
			notes:   "cursor: not-a-time\n",
			now:     now,
			wantErr: true,
		},
	} {
		got, err := ReviewWindow(tc.notes, tc.now)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: ReviewWindow(%q) = %+v; want an error", tc.name, tc.notes, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: ReviewWindow(%q) error: %v", tc.name, tc.notes, err)
			continue
		}
		if !got.Since.Time.Equal(tc.want.Since.Time) || got.Since.Text != tc.want.Since.Text || got.Cursor != tc.want.Cursor {
			t.Errorf("%s: ReviewWindow(%q) = %+v, want %+v", tc.name, tc.notes, got, tc.want)
		}
	}
}
