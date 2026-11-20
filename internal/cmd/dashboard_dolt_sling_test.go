package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The daemon's logger writes with log.LstdFlags, so a daemon-log line opens
// with this stamp. The tests spell it out rather than reading the reader's own
// layout, so a reader that drifts from the logger's format fails here.
const doltSlingTestLayout = "2006/01/02 15:04:05"

// doltSlingTestLog writes a daemon.log holding lines and returns its path. The
// file is built in the test: no live town and no network.
func doltSlingTestLog(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "daemon.log")
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// doltSlingTestLine is one line the dispatcher writes after a sling, as
// internal/daemon/spec_slings.go writes it: "spec_dispatch: <entry> (sling
// took <duration>)".
func doltSlingTestLine(at time.Time, entry, took string) string {
	return at.Format(doltSlingTestLayout) + " spec_dispatch: " + entry + " (sling took " + took + ")"
}

// doltSlingTestNow is the clock every test reads at, in the zone the log's
// stamps are read in.
func doltSlingTestNow() time.Time {
	return time.Date(2026, 11, 20, 21, 43, 56, 0, time.Local)
}

// Five sling lines inside the hour give the count, median, p90 and worst, with
// the worst line's bead and time; a sling line older than the hour is left out.
func TestDoltSlingSectionReadsTheLastHour(t *testing.T) {
	t.Parallel()
	now := doltSlingTestNow()
	path := doltSlingTestLog(t,
		doltSlingTestLine(now.Add(-50*time.Minute), "dispatched: gt-a: slung to gastown/p", "10s"),
		doltSlingTestLine(now.Add(-40*time.Minute), "dispatched: gt-b: slung to gastown/p", "30s"),
		doltSlingTestLine(now.Add(-30*time.Minute), "dispatched: gt-c: slung to gastown/p", "1m0s"),
		doltSlingTestLine(now.Add(-20*time.Minute), "dispatched: gt-d: slung to gastown/p", "2m0s"),
		doltSlingTestLine(now.Add(-10*time.Minute), "dispatched: gt-e: slung to gastown/p", "3m0s"),
		// Older than the hour: its 4m is not the worst the hour saw.
		doltSlingTestLine(now.Add(-2*time.Hour), "dispatched: gt-old: slung to gastown/p", "4m0s"),
	)

	sec, err := doltSlingSection(path, now)
	if err != nil {
		t.Fatalf("the section failed: %v", err)
	}
	if sec.Err != "" || sec.Note != "" {
		t.Errorf("the section carries Err %q and Note %q, want neither", sec.Err, sec.Note)
	}
	if len(sec.Rows) != 1 || len(sec.Rows[0]) != len(sec.Head) {
		t.Fatalf("the section is head %v rows %v, want one row of %d cells", sec.Head, sec.Rows, len(sec.Head))
	}
	want := []string{"5", "1m0s", "3m0s", "3m0s", "gt-e", "21:33:56"}
	if got := strings.Join(sec.Rows[0], "|"); got != strings.Join(want, "|") {
		t.Errorf("the hour reads %v, want %v", sec.Rows[0], want)
	}
}

// A log that reaches back past the hour but holds no sling line draws a row of
// zero and dashes, not an empty table and not a failure.
func TestDoltSlingSectionDrawsAnHourWithNoSlings(t *testing.T) {
	t.Parallel()
	now := doltSlingTestNow()
	path := doltSlingTestLog(t,
		now.Add(-3*time.Hour).Format(doltSlingTestLayout)+" spec_dispatch: tick: 0 candidate(s), 0 dispatched",
		now.Add(-5*time.Minute).Format(doltSlingTestLayout)+" spec_dispatch: tick: 0 candidate(s), 0 dispatched",
	)

	sec, err := doltSlingSection(path, now)
	if err != nil {
		t.Fatalf("the section failed: %v", err)
	}
	if sec.Err != "" {
		t.Errorf("the section carries Err %q, want a quiet hour drawn as zero", sec.Err)
	}
	if len(sec.Rows) != 1 || len(sec.Rows[0]) != len(sec.Head) {
		t.Fatalf("the section is head %v rows %v, want one row of %d cells", sec.Head, sec.Rows, len(sec.Head))
	}
	want := []string{"0", "—", "—", "—", "—", "—"}
	if got := strings.Join(sec.Rows[0], "|"); got != strings.Join(want, "|") {
		t.Errorf("the quiet hour reads %v, want %v", sec.Rows[0], want)
	}
}

// A missing log is a failure the section reports, never a zero row that passes
// for an hour with no slings.
func TestDoltSlingSectionRefusesAMissingLog(t *testing.T) {
	t.Parallel()
	sec, err := doltSlingSection(filepath.Join(t.TempDir(), "daemon.log"), doltSlingTestNow())
	if err == nil {
		t.Fatalf("a missing log drew %+v, want an error", sec)
	}
	if !strings.Contains(err.Error(), "daemon log unreadable") {
		t.Errorf("the error is %q, want it to say the daemon log is unreadable", err)
	}
	if sec.Rows != nil || sec.Head != nil {
		t.Errorf("the failed section carries head %v rows %v, want none", sec.Head, sec.Rows)
	}
}

// doltSlingTestFile is an open log whose every operation a test can make fail,
// so the section's unreadable-file branches are driven without a real file.
type doltSlingTestFile struct {
	size    int64
	statErr error
	seekErr error
	readErr error
}

func (f doltSlingTestFile) Stat() (os.FileInfo, error) {
	if f.statErr != nil {
		return nil, f.statErr
	}
	return doltSlingTestFileInfo{size: f.size}, nil
}

func (f doltSlingTestFile) Seek(int64, int) (int64, error) {
	if f.seekErr != nil {
		return 0, f.seekErr
	}
	return 0, nil
}

func (f doltSlingTestFile) Read([]byte) (int, error) {
	if f.readErr != nil {
		return 0, f.readErr
	}
	return 0, io.EOF
}

// doltSlingTestFileInfo is the size the fake log reports.
type doltSlingTestFileInfo struct{ size int64 }

func (i doltSlingTestFileInfo) Name() string       { return "daemon.log" }
func (i doltSlingTestFileInfo) Size() int64        { return i.size }
func (i doltSlingTestFileInfo) Mode() os.FileMode  { return 0o600 }
func (i doltSlingTestFileInfo) ModTime() time.Time { return time.Time{} }
func (i doltSlingTestFileInfo) IsDir() bool        { return false }
func (i doltSlingTestFileInfo) Sys() any           { return nil }

// A log whose size, seek or read fails is a failure the section reports, never
// an hour drawn from the bytes it could read.
func TestDoltSlingSectionRefusesAnUnreadableLogFile(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		file doltSlingTestFile
	}{
		{name: "stat", file: doltSlingTestFile{statErr: errors.New("stat refused")}},
		{name: "seek", file: doltSlingTestFile{size: 16, seekErr: errors.New("seek refused")}},
		{name: "read", file: doltSlingTestFile{size: 16, readErr: errors.New("read refused")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := doltSlingSectionFrom(tc.file, doltSlingTestNow()); err == nil {
				t.Fatalf("a log whose %s failed was read as a section", tc.name)
			} else if !strings.Contains(err.Error(), "daemon log unreadable") {
				t.Errorf("the error is %q, want it to say the daemon log is unreadable", err)
			}
		})
	}
}

// A log whose oldest line is inside the hour says it did not reach back a full
// hour, while still drawing the slings it did hold.
func TestDoltSlingSectionNotesATailShorterThanTheHour(t *testing.T) {
	t.Parallel()
	now := doltSlingTestNow()
	path := doltSlingTestLog(t,
		doltSlingTestLine(now.Add(-20*time.Minute), "dispatched: gt-a: slung to gastown/p", "10s"),
		doltSlingTestLine(now.Add(-10*time.Minute), "dispatched: gt-b: slung to gastown/p", "20s"),
	)

	sec, err := doltSlingSection(path, now)
	if err != nil {
		t.Fatalf("the section failed: %v", err)
	}
	if !strings.Contains(sec.Note, "reached back") && !strings.Contains(sec.Note, "reach back") {
		t.Errorf("the note is %q, want it to say the log did not reach back a full hour", sec.Note)
	}
	if len(sec.Rows) != 1 || sec.Rows[0][0] != "2" {
		t.Errorf("the section rows are %v, want the two slings it held", sec.Rows)
	}
}

// A log holding a line from before the hour needs no note: the reader saw the
// hour's whole start.
func TestDoltSlingSectionNotesNothingForAFullHour(t *testing.T) {
	t.Parallel()
	now := doltSlingTestNow()
	path := doltSlingTestLog(t,
		now.Add(-90*time.Minute).Format(doltSlingTestLayout)+" spec_dispatch: tick: 0 candidate(s), 0 dispatched",
		doltSlingTestLine(now.Add(-10*time.Minute), "dispatched: gt-a: slung to gastown/p", "10s"),
	)

	sec, err := doltSlingSection(path, now)
	if err != nil {
		t.Fatalf("the section failed: %v", err)
	}
	if sec.Note != "" {
		t.Errorf("the note is %q, want none: the log reaches back past the hour", sec.Note)
	}
}

// A sling line whose shape, stamp or duration is not one the dispatcher writes
// is a failure, never a silent drop from the count.
func TestDoltSlingSectionRefusesAMalformedSlingLine(t *testing.T) {
	t.Parallel()
	now := doltSlingTestNow()
	cases := []struct {
		name string
		line string
	}{
		{name: "duration", line: doltSlingTestLine(now.Add(-10*time.Minute), "dispatched: gt-a: slung to gastown/p", "5x")},
		{name: "stamp", line: "2026/13/45 99:00:00 spec_dispatch: dispatched: gt-a: slung to gastown/p (sling took 10s)"},
		{name: "shape", line: now.Add(-10*time.Minute).Format(doltSlingTestLayout) + " spec_dispatch: dispatched: gt-a: slung to gastown/p (sling took 10s"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := doltSlingTestLog(t, tc.line)
			if _, err := doltSlingSection(path, now); err == nil {
				t.Fatalf("a sling line with an unreadable %s was read as a sling", tc.name)
			} else if !strings.Contains(err.Error(), "daemon log unreadable") {
				t.Errorf("the error is %q, want it to say the daemon log is unreadable", err)
			}
		})
	}
}

// A line outside the hour is not part of the reading, so its unreadable
// duration costs the hour nothing.
func TestDoltSlingSectionIgnoresAnOldLineItCannotRead(t *testing.T) {
	t.Parallel()
	now := doltSlingTestNow()
	path := doltSlingTestLog(t,
		doltSlingTestLine(now.Add(-2*time.Hour), "dispatched: gt-old: slung to gastown/p", "5x"),
		doltSlingTestLine(now.Add(-10*time.Minute), "dispatched: gt-a: slung to gastown/p", "10s"),
	)

	sec, err := doltSlingSection(path, now)
	if err != nil {
		t.Fatalf("the section failed on a line outside its window: %v", err)
	}
	if len(sec.Rows) != 1 || sec.Rows[0][0] != "1" {
		t.Errorf("the section rows are %v, want the hour's one sling", sec.Rows)
	}
}

// The reader loads only the log's last MiB: a sling line before that window is
// not counted, and the partial line the window starts in is dropped whole.
func TestDoltSlingSectionReadsOnlyTheLastMiB(t *testing.T) {
	t.Parallel()
	now := doltSlingTestNow()
	var b strings.Builder
	b.WriteString(doltSlingTestLine(now.Add(-30*time.Minute), "dispatched: gt-old: slung to gastown/p", "9m0s") + "\n")
	filler := now.Add(-25*time.Minute).Format(doltSlingTestLayout) + " spec_dispatch: tick: 0 candidate(s), 0 dispatched\n"
	for b.Len() <= 1<<20+len(filler) {
		b.WriteString(filler)
	}
	b.WriteString(doltSlingTestLine(now.Add(-5*time.Minute), "dispatched: gt-new: slung to gastown/p", "20s") + "\n")
	path := filepath.Join(t.TempDir(), "daemon.log")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	sec, err := doltSlingSection(path, now)
	if err != nil {
		t.Fatalf("the section failed: %v", err)
	}
	if len(sec.Rows) != 1 || sec.Rows[0][0] != "1" || sec.Rows[0][4] != "gt-new" {
		t.Errorf("the section rows are %v, want only the sling inside the last MiB (gt-new)", sec.Rows)
	}
}

// The section reads the log under the town root the panel hands its read, so
// the registered section draws the town's own daemon log.
func TestDoltSlingSectionReadsTheTownRootsLog(t *testing.T) {
	t.Parallel()
	now := doltSlingTestNow()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := doltSlingTestLine(now.Add(-10*time.Minute), "dispatched: gt-a: slung to gastown/p", "10s") + "\n"
	if err := os.WriteFile(filepath.Join(root, "daemon", "daemon.log"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	sec, err := readDoltSlingSection(withDoltTownRoot(context.Background(), root), now)
	if err != nil {
		t.Fatalf("the section failed: %v", err)
	}
	if len(sec.Rows) != 1 || sec.Rows[0][0] != "1" {
		t.Errorf("the section rows are %v, want the town log's one sling", sec.Rows)
	}
}

// A read handed no town root fails its section: it cannot fall back to reading
// the empty root.
func TestDoltSlingSectionRefusesAReadWithNoTownRoot(t *testing.T) {
	t.Parallel()
	if _, err := readDoltSlingSection(context.Background(), doltSlingTestNow()); err == nil {
		t.Fatal("a read with no town root drew a section, want an error")
	}
}

// The section is registered under "sling" when the package loads, so the panel
// draws it without another file naming it.
func TestDoltSlingSectionIsRegisteredWithThePanel(t *testing.T) {
	t.Parallel()
	for _, r := range doltSections.snapshot() {
		if r.key == "sling" {
			return
		}
	}
	t.Fatal(`no section is registered under "sling"`)
}
