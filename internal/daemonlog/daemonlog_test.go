package daemonlog

import (
	"compress/gzip"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testLoc = time.FixedZone("CDT", -5*3600)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestParserKeepsTheEntrysTimeForAnUnstampedLine(t *testing.T) {
	t.Parallel()
	p := &Parser{Loc: testLoc}
	if at, text := p.Parse("not stamped"); !at.IsZero() || text != "not stamped" {
		t.Errorf("a line before any stamp = (%v, %q), want zero time and its whole text", at, text)
	}
	at, text := p.Parse("2026/10/02 12:52:05 landing_worker: [land] gt-x: merged")
	if want := time.Date(2026, 10, 2, 12, 52, 5, 0, testLoc); !at.Equal(want) || text != "landing_worker: [land] gt-x: merged" {
		t.Errorf("stamped line = (%v, %q)", at, text)
	}
	if at2, text2 := p.Parse("  continuation"); !at2.Equal(at) || text2 != "  continuation" {
		t.Errorf("continuation = (%v, %q), want the entry's time", at2, text2)
	}
}

func TestReadSpansBackupsAndTheLiveFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "daemon-2026-10-02T17-00-00.000.log"),
		"2026/10/02 11:00:00 old: before the cutoff\n2026/10/02 12:55:00 a: in the backup\n")
	gz, err := os.Create(filepath.Join(dir, "daemon-2026-10-02T18-00-00.000.log.gz"))
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(gz)
	_, _ = zw.Write([]byte("2026/10/02 13:30:00 b: in the gzipped backup\n"))
	_ = zw.Close()
	_ = gz.Close()
	// The rotation stamp is UTC: a backup rotated before the cutoff holds only
	// older lines and is not opened.
	writeFile(t, filepath.Join(dir, "daemon-2026-10-02T10-00-00.000.log"), "2026/10/02 12:56:00 skipped: never read\n")
	writeFile(t, filepath.Join(dir, "daemon-notes.txt"), "2026/10/02 13:00:00 not a log\n")
	writeFile(t, filepath.Join(dir, FileName),
		"2026/10/02 13:40:00 c: live\ncontinued under c\n2026/10/02 13:50:00 d: half-written")

	cutoff := time.Date(2026, 10, 2, 12, 0, 0, 0, testLoc)
	lines, err := Read(dir, cutoff, testLoc)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	var got []string
	for _, l := range lines {
		got = append(got, l.At.In(testLoc).Format("15:04")+" "+l.Text)
	}
	want := []string{
		"12:55 a: in the backup",
		"13:30 b: in the gzipped backup",
		"13:40 c: live",
		"13:40 continued under c",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("lines =\n%s\nwant\n%s\n(the half-written last line is left for the next read)",
			strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestParserReadsTheThreeRegions covers the live log's two zone steps: the
// container cutover, whose start sequence must be dated UTC including the
// lines before its owner line, and the timezone marker that returns the log to
// local time. The zone marker a parser cannot load leaves the previous zone.
func TestParserReadsTheThreeRegions(t *testing.T) {
	t.Parallel()
	cst := time.FixedZone("CST", -6*3600)
	chicago := time.FixedZone("America/Chicago", -6*3600)
	load := func(name string) (*time.Location, error) {
		if name == "America/Chicago" {
			return chicago, nil
		}
		return nil, errors.New("no such zone")
	}
	p := &Parser{
		Loc:      cst,
		LoadZone: load,
		// The bogus marker's report is asserted on its own in
		// TestParserUnloadableMarkerIsReportedOnce.
		Logf: func(string, ...any) {},
		// Past every stamp below, so the future guard never moves a line.
		Now: func() time.Time { return time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC) },
	}
	raws := []string{
		// Region A: the Mac daemon, stamps in the reader's zone.
		"2026/11/04 10:55:00 Loaded patrol config from /Users/me/gt/settings/config.json",
		"2026/11/04 10:55:01 Daemon starting (PID 4242)",
		"2026/11/04 10:55:01 Daemon owner: launchd \"com.gastown.daemon\"",
		"2026/11/04 11:01:00 landing_worker: [land] gt-a: landed abc on main",
		// Region B: the container cutover. The failed starts at 18:08:19 and
		// 18:09:20 carry no owner line; the owner at 18:10:02 makes the whole
		// sequence, and all lines after it, UTC.
		"2026/11/04 18:08:19 Loaded patrol config from /gt/settings/config.json",
		"2026/11/04 18:08:19 Daemon startup failed (PID 1): refusing to start: another owner holds the town",
		"2026/11/04 18:09:20 Loaded patrol config from /gt/settings/config.json",
		"2026/11/04 18:09:20 Daemon startup failed (PID 1): refusing to start: another owner holds the town",
		"2026/11/04 18:10:02 Loaded patrol config from /gt/settings/config.json",
		"2026/11/04 18:10:02 Patrols disabled via town settings: compactor",
		"2026/11/04 18:10:02 Daemon starting (PID 1)",
		"2026/11/04 18:10:02 Daemon owner: container \"gt-daemon-1\"",
		"2026/11/04 20:00:00 landing_worker: [land] gt-b: landed def on main",
		// Region C: the image carries tzdata, and its start names the zone.
		"2026/11/05 16:37:53 Loaded patrol config from /gt/settings/config.json",
		"2026/11/05 16:37:53 Daemon starting (PID 1)",
		"2026/11/05 16:37:53 Daemon owner: container \"gt-daemon-1\"",
		"2026/11/05 16:37:53 log timezone: America/Chicago (UTC-06:00)",
		"2026/11/05 16:38:00 landing_worker: [land] gt-c: landed 123 on main",
		// A zone the parser cannot load leaves the previous one in force.
		"2026/11/05 17:00:00 log timezone: Nowhere/Atlantis (UTC-06:00)",
		"2026/11/05 17:00:05 landing_worker: [land] gt-d: landed 456 on main",
	}
	want := []time.Time{
		time.Date(2026, 11, 4, 16, 55, 0, 0, time.UTC),
		time.Date(2026, 11, 4, 16, 55, 1, 0, time.UTC),
		time.Date(2026, 11, 4, 16, 55, 1, 0, time.UTC),
		time.Date(2026, 11, 4, 17, 1, 0, 0, time.UTC),
		time.Date(2026, 11, 4, 18, 8, 19, 0, time.UTC),
		time.Date(2026, 11, 4, 18, 8, 19, 0, time.UTC),
		time.Date(2026, 11, 4, 18, 9, 20, 0, time.UTC),
		time.Date(2026, 11, 4, 18, 9, 20, 0, time.UTC),
		time.Date(2026, 11, 4, 18, 10, 2, 0, time.UTC),
		time.Date(2026, 11, 4, 18, 10, 2, 0, time.UTC),
		time.Date(2026, 11, 4, 18, 10, 2, 0, time.UTC),
		time.Date(2026, 11, 4, 18, 10, 2, 0, time.UTC),
		time.Date(2026, 11, 4, 20, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 5, 22, 37, 53, 0, time.UTC),
		time.Date(2026, 11, 5, 22, 37, 53, 0, time.UTC),
		time.Date(2026, 11, 5, 22, 37, 53, 0, time.UTC),
		time.Date(2026, 11, 5, 22, 37, 53, 0, time.UTC),
		time.Date(2026, 11, 5, 22, 38, 0, 0, time.UTC),
		time.Date(2026, 11, 5, 23, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 5, 23, 0, 5, 0, time.UTC),
	}
	var lines []Line
	for _, raw := range raws {
		lines = append(lines, p.Feed(raw)...)
	}
	lines = append(lines, p.Flush()...)
	if len(lines) != len(raws) {
		t.Fatalf("the parser returned %d lines, want one per raw line (%d)", len(lines), len(raws))
	}
	for i := range want {
		if !lines[i].At.Equal(want[i]) {
			t.Errorf("line %d (%q) = %v, want %v", i, raws[i], lines[i].At.UTC(), want[i])
		}
	}
}

// TestParserNeverReturnsAFutureTime is the tail reader's backstop: a read that
// began inside the UTC region has seen no marker, but a stamp past now plus
// the skew is re-read as UTC, and one within the skew stays local.
func TestParserNeverReturnsAFutureTime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 5, 22, 46, 0, 0, time.UTC) // 16:46 CST
	p := &Parser{Loc: time.FixedZone("CST", -6*3600), Now: func() time.Time { return now }}
	cases := []struct {
		raw  string
		want time.Time
	}{
		{"2026/11/05 22:34:05 landing_worker: [land] gt-r: rejected (review): om requested changes", time.Date(2026, 11, 5, 22, 34, 5, 0, time.UTC)},
		{"2026/11/05 16:48:00 landing_worker: [land] gt-s: landed abc on main", time.Date(2026, 11, 5, 22, 48, 0, 0, time.UTC)},
		// Two seconds ahead is clock skew, not a UTC region: the local reading
		// stands and is not moved back by the offset.
		{"2026/11/05 16:46:02 landing_worker: [land] gt-u: landed ghi on main", time.Date(2026, 11, 5, 22, 46, 2, 0, time.UTC)},
		// One second past the tolerance is re-read as UTC.
		{"2026/11/05 16:48:01 landing_worker: [land] gt-v: landed jkl on main", time.Date(2026, 11, 5, 16, 48, 1, 0, time.UTC)},
	}
	for _, c := range cases {
		at, _ := p.Parse(c.raw)
		if !at.Equal(c.want) {
			t.Errorf("Parse(%q) = %v, want %v", c.raw, at.UTC(), c.want)
		}
		if at.After(now.Add(FutureSkew)) {
			t.Errorf("Parse(%q) = %v, after now plus the skew", c.raw, at.UTC())
		}
	}
}

// TestParserFutureGuardDoesNotPushAnEasternStampForward covers a reader whose
// zone is east of UTC, where the same wall clock read as UTC is later still:
// the guard takes the local reading rather than moving the line by the offset.
func TestParserFutureGuardDoesNotPushAnEasternStampForward(t *testing.T) {
	t.Parallel()
	// 10:00 on 2026-11-05 in UTC+13 is 21:00 UTC the day before.
	east := time.FixedZone("NZDT", 13*3600)
	now := time.Date(2026, 11, 4, 21, 0, 0, 0, time.UTC)
	p := &Parser{Loc: east, Now: func() time.Time { return now }}
	at, _ := p.Parse("2026/11/05 10:03:00 landing_worker: [land] gt-a: landed abc on main")
	want := time.Date(2026, 11, 4, 21, 3, 0, 0, time.UTC)
	if !at.Equal(want) {
		t.Errorf("Parse = %v, want the local reading %v (re-read as UTC would be 13 hours later)", at.UTC(), want)
	}
}

// TestParserUnloadableMarkerIsReportedOnce holds the marker contract: a zone
// this host cannot load is reported once for its marker text, and the zone the
// parser was already reading in stays in force.
func TestParserUnloadableMarkerIsReportedOnce(t *testing.T) {
	t.Parallel()
	cst := time.FixedZone("CST", -6*3600)
	var reports []string
	p := &Parser{
		Loc:      cst,
		LoadZone: func(string) (*time.Location, error) { return nil, errors.New("no zone database") },
		Logf: func(format string, args ...any) {
			reports = append(reports, fmt.Sprintf(format, args...))
		},
		Now: func() time.Time { return time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC) },
	}
	const marker = "log timezone: Nowhere/Atlantis (UTC-06:00)"
	raws := []string{
		"2026/11/05 12:00:00 " + marker,
		"2026/11/05 12:00:01 landing_worker: [land] gt-a: landed abc on main",
		"2026/11/05 13:00:00 " + marker,
		"2026/11/05 13:00:01 landing_worker: [land] gt-b: landed def on main",
	}
	var lines []Line
	for _, raw := range raws {
		lines = append(lines, p.Feed(raw)...)
	}
	lines = append(lines, p.Flush()...)
	if len(reports) != 1 {
		t.Fatalf("reports = %q, want one for the two identical markers", reports)
	}
	if !strings.Contains(reports[0], marker) {
		t.Errorf("report %q does not name the marker text", reports[0])
	}
	want := []time.Time{
		time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 5, 18, 0, 1, 0, time.UTC),
		time.Date(2026, 11, 5, 19, 0, 0, 0, time.UTC),
		time.Date(2026, 11, 5, 19, 0, 1, 0, time.UTC),
	}
	if len(lines) != len(want) {
		t.Fatalf("lines = %+v, want %d", lines, len(want))
	}
	for i := range want {
		if !lines[i].At.Equal(want[i]) {
			t.Errorf("line %d (%q) = %v, want %v (the zone in force must not change)",
				i, raws[i], lines[i].At.UTC(), want[i])
		}
	}
}

// TestParserIgnoresAForgedMarkerInAMessage holds that only the daemon's own
// line shape switches the zone: a bead title or an error that contains the
// marker or owner text is dated in the zone already in force.
func TestParserIgnoresAForgedMarkerInAMessage(t *testing.T) {
	t.Parallel()
	cst := time.FixedZone("CST", -6*3600)
	chicago := time.FixedZone("America/Chicago", -6*3600)
	p := &Parser{
		Loc: cst,
		LoadZone: func(name string) (*time.Location, error) {
			if name == "Pacific/Auckland" {
				return time.FixedZone("Pacific/Auckland", 13*3600), nil
			}
			return chicago, nil
		},
		Now: func() time.Time { return time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC) },
	}
	raws := []string{
		`2026/11/05 12:00:00 daemon: bead gt-x titled "log timezone: Pacific/Auckland (UTC+13:00)"`,
		"2026/11/05 12:00:01 landing_worker: [land] gt-a: landed abc on main",
		"2026/11/05 12:00:02 error: Daemon owner: container is a bead title",
		"2026/11/05 12:00:03 landing_worker: [land] gt-b: landed def on main",
		`2026/11/05 12:00:04 Daemon owner: container "gt-daemon-1" in a bead title`,
		"2026/11/05 12:00:05 landing_worker: [land] gt-c: landed ghi on main",
	}
	var lines []Line
	for _, raw := range raws {
		lines = append(lines, p.Feed(raw)...)
	}
	lines = append(lines, p.Flush()...)
	if len(lines) != len(raws) {
		t.Fatalf("lines = %+v, want %d", lines, len(raws))
	}
	for i, raw := range raws {
		if want := time.Date(2026, 11, 5, 18, 0, i, 0, time.UTC); !lines[i].At.Equal(want) {
			t.Errorf("line %d (%q) = %v, want %v in the reader's zone", i, raw, lines[i].At.UTC(), want)
		}
	}
}

func TestReadMissingLogIsNoLines(t *testing.T) {
	t.Parallel()
	lines, err := Read(filepath.Join(t.TempDir(), "absent"), time.Time{}, testLoc)
	if err != nil || len(lines) != 0 {
		t.Errorf("Read of a missing directory = (%v, %v), want no lines and no error", lines, err)
	}
}

func TestReadReturnsACorruptBackup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "daemon-2026-10-02T18-00-00.000.log.gz"), "not gzip")
	writeFile(t, filepath.Join(dir, FileName), "2026/10/02 13:40:00 c: live\n")
	if _, err := Read(dir, time.Date(2026, 10, 2, 12, 0, 0, 0, testLoc), testLoc); err == nil {
		t.Error("a corrupt backup read without an error")
	}
}
