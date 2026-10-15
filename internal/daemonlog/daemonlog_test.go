package daemonlog

import (
	"compress/gzip"
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
