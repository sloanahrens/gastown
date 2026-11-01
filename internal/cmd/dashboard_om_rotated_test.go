package cmd

import (
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A daemon restart rotates daemon.log to daemon-<UTC stamp>.log.gz, and the
// panels count 24 hours of rejections and stage times (gt-qp3zw). These tests
// build the town's daemon directory in a temp dir and pin the clock.

var omRotatedNow = time.Date(2026, 10, 31, 20, 0, 0, 0, time.UTC)

func omRotatedRejectLine(bead, stamp string) string {
	return stamp + " landing_worker: [land] " + bead + ": rejected (review): score 0.55 below threshold"
}

// omRotatedDir writes a live daemon.log and returns its path.
func omRotatedDir(t *testing.T, live ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "daemon.log")
	if err := os.WriteFile(path, []byte(strings.Join(live, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// omRotatedWrite gzips lines into daemon-<stamp>.log.gz beside logPath.
func omRotatedWrite(t *testing.T, logPath, stamp string, lines ...string) string {
	t.Helper()
	path := filepath.Join(filepath.Dir(logPath), "daemon-"+stamp+".log.gz")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := gzip.NewWriter(f)
	if _, err := zw.Write([]byte(strings.Join(lines, "\n") + "\n")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func omRotatedReader(logPath string) *omReader {
	return &omReader{logPath: logPath, now: func() time.Time { return omRotatedNow }}
}

func TestOMReaderReadsRotatedLogsInTheWindow(t *testing.T) {
	t.Parallel()
	log := omRotatedDir(t, omRotatedRejectLine("gt-live", "2026/10/31 19:30:00"))
	omRotatedWrite(t, log, "2026-10-31T23-19-47.000",
		omRotatedRejectLine("gt-old", "2026/10/31 12:00:00"),
		"2026/10/31 12:01:00 landing_worker: [land] gt-old: stages: lint 1s, gate 2m, om 30s")
	r := omRotatedReader(log)
	if err := r.scanLog(); err != nil {
		t.Fatal(err)
	}
	if len(r.rejects) != 2 || r.rejects[0].Bead != "gt-old" || r.rejects[1].Bead != "gt-live" {
		t.Fatalf("want the rotated rejection, then the live one: %+v", r.rejects)
	}
	if len(r.stages) != 1 || r.stages[0].Bead != "gt-old" {
		t.Fatalf("the rotated log's stage line must be kept: %+v", r.stages)
	}
}

func TestOMReaderReadsEachRotatedLogOnce(t *testing.T) {
	t.Parallel()
	log := omRotatedDir(t, omRotatedRejectLine("gt-live", "2026/10/31 19:30:00"))
	omRotatedWrite(t, log, "2026-10-31T23-19-47.000", omRotatedRejectLine("gt-old", "2026/10/31 12:00:00"))
	r := omRotatedReader(log)
	for i := 0; i < 3; i++ {
		if err := r.scanLog(); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.rejects) != 2 {
		t.Fatalf("three scans must count the rotated rejection once: %+v", r.rejects)
	}
}

func TestOMReaderSkipsRotatedLogsOutsideTheWindow(t *testing.T) {
	t.Parallel()
	log := omRotatedDir(t, omRotatedRejectLine("gt-live", "2026/10/31 19:30:00"))
	// rotated 40 hours before the pinned clock
	omRotatedWrite(t, log, "2026-10-30T04-00-00.000", omRotatedRejectLine("gt-stale", "2026/10/29 20:00:00"))
	omRotatedWrite(t, log, "2026-10-31T23-19-47.000", omRotatedRejectLine("gt-new", "2026/10/31 12:00:00"))
	r := omRotatedReader(log)
	if err := r.scanLog(); err != nil {
		t.Fatal(err)
	}
	if len(r.rejects) != 2 || r.rejects[0].Bead != "gt-new" || r.rejects[1].Bead != "gt-live" {
		t.Fatalf("a rotated log older than the window adds nothing: %+v", r.rejects)
	}
}

// A rotated log that cannot be decompressed is reported by every scan, its
// good lines before the damage stay, and it is not read a second time.
func TestOMReaderReportsAnUnreadableRotatedLog(t *testing.T) {
	t.Parallel()
	log := omRotatedDir(t, omRotatedRejectLine("gt-live", "2026/10/31 19:30:00"))
	bad := filepath.Join(filepath.Dir(log), "daemon-2026-10-31T22-00-00.000.log.gz")
	if err := os.WriteFile(bad, []byte("not gzip"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := omRotatedReader(log)
	for i := 0; i < 2; i++ {
		err := r.scanLog()
		if err == nil || !strings.Contains(err.Error(), "daemon-2026-10-31T22-00-00.000.log.gz") {
			t.Fatalf("scan %d must name the unreadable rotated log: %v", i, err)
		}
	}
	if len(r.rejects) != 1 {
		t.Fatalf("the live log is still read: %+v", r.rejects)
	}
}
