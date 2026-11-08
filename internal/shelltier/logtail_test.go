package shelltier

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFailedLogTailsPrintsTheLastLinesOfEachFailedScript(t *testing.T) {
	t.Parallel()
	logDir := t.TempDir()
	lines := make([]string, 100)
	for i := range lines {
		lines[i] = fmt.Sprintf("line %d", i+1)
	}
	writeLog(t, logDir, "scripts/a_test.sh", strings.Join(lines, "\n")+"\n")

	var b strings.Builder
	WriteFailedLogTails(&b, logDir, []string{"scripts/a_test.sh"})

	got := b.String()
	head := "tier-sweep: shell log tail scripts/a_test.sh\n"
	if !strings.HasPrefix(got, head) {
		t.Fatalf("output = %q, want it to start with %q", got, head)
	}
	tail := strings.Split(strings.TrimSuffix(got[len(head):], "\n"), "\n")
	if len(tail) != 80 || tail[0] != "line 21" || tail[len(tail)-1] != "line 100" {
		t.Fatalf("tail has %d lines, from %q to %q; want the last 80, line 21 to line 100", len(tail), tail[0], tail[len(tail)-1])
	}
}

// TestWriteFailedLogTailsStreamsALogLargerThanTheRing requires only the last
// LogTailLines lines of a log far larger than the ring to print: the file is
// read through a ring of that many lines, not held whole.
func TestWriteFailedLogTailsStreamsALogLargerThanTheRing(t *testing.T) {
	t.Parallel()
	logDir := t.TempDir()
	const total = 5000
	var body strings.Builder
	for i := 1; i <= total; i++ {
		fmt.Fprintf(&body, "line %d\n", i)
	}
	writeLog(t, logDir, "scripts/big_test.sh", body.String())

	var b strings.Builder
	WriteFailedLogTails(&b, logDir, []string{"scripts/big_test.sh"})

	head := "tier-sweep: shell log tail scripts/big_test.sh\n"
	if !strings.HasPrefix(b.String(), head) {
		t.Fatalf("output = %q, want it to start with %q", b.String(), head)
	}
	tail := strings.Split(strings.TrimSuffix(b.String()[len(head):], "\n"), "\n")
	if len(tail) != 80 || tail[0] != "line 4921" || tail[len(tail)-1] != "line 5000" {
		t.Fatalf("tail has %d lines, from %q to %q; want the last 80, line 4921 to line 5000", len(tail), tail[0], tail[len(tail)-1])
	}
}

// TestWriteFailedLogTailsRedactsSecretsInTheLog requires a script's secret to
// be scrubbed before it reaches the job log: the raw tail is new output there,
// and internal/jev/scrub is the town's secret-pattern scrubber.
func TestWriteFailedLogTailsRedactsSecretsInTheLog(t *testing.T) {
	t.Parallel()
	logDir := t.TempDir()
	const secret = "ghp_0123456789abcdefghijABCDEFGHIJ"
	writeLog(t, logDir, "scripts/a_test.sh", "the assertion failed\nexport GITHUB_TOKEN="+secret+"\n")

	var b strings.Builder
	WriteFailedLogTails(&b, logDir, []string{"scripts/a_test.sh"})
	if got := b.String(); strings.Contains(got, secret) {
		t.Fatalf("output = %q, want the script's secret redacted", got)
	}
	if got := b.String(); !strings.Contains(got, "the assertion failed") || !strings.Contains(got, "[REDACTED") {
		t.Fatalf("output = %q, want the failing line kept and a redaction marker in the secret's place", got)
	}
}

func TestWriteFailedLogTailsPrintsAShortLogWhole(t *testing.T) {
	t.Parallel()
	logDir := t.TempDir()
	writeLog(t, logDir, "scripts/a_test.sh", "one\nthe assertion failed\n")

	var b strings.Builder
	WriteFailedLogTails(&b, logDir, []string{"scripts/a_test.sh"})
	want := "tier-sweep: shell log tail scripts/a_test.sh\none\nthe assertion failed\n"
	if got := b.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestWriteFailedLogTailsPrintsNothingForNoFailedScript(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	WriteFailedLogTails(&b, t.TempDir(), nil)
	if got := b.String(); got != "" {
		t.Fatalf("output = %q, want nothing for a tier with no failed script", got)
	}
}

func TestWriteFailedLogTailsNamesAnUnreadableLogInOneLine(t *testing.T) {
	t.Parallel()
	logDir := t.TempDir()
	var b strings.Builder
	WriteFailedLogTails(&b, logDir, []string{"scripts/gone_test.sh"})
	lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("output = %q, want the header and one line saying the log cannot be read", b.String())
	}
	if want := "tier-sweep: shell log tail scripts/gone_test.sh"; lines[0] != want {
		t.Fatalf("header = %q, want %q", lines[0], want)
	}
	if !strings.Contains(lines[1], "cannot read") {
		t.Fatalf("second line = %q, want it to say the log cannot be read", lines[1])
	}
}

// TestWriteFailedLogTailsNamesALogThatCannotBeRead drives the read failure: a
// directory where the log should be opens but cannot be read, and the failure
// is one named line, not a dropped tail.
func TestWriteFailedLogTailsNamesALogThatCannotBeRead(t *testing.T) {
	t.Parallel()
	logDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(logDir, logName("scripts/dir_test.sh")+".log"), 0o700); err != nil {
		t.Fatalf("make the log path a directory: %v", err)
	}

	var b strings.Builder
	WriteFailedLogTails(&b, logDir, []string{"scripts/dir_test.sh"})
	lines := strings.Split(strings.TrimSuffix(b.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("output = %q, want the header and one line saying the log cannot be read", b.String())
	}
	if want := "tier-sweep: shell log tail scripts/dir_test.sh"; lines[0] != want {
		t.Fatalf("header = %q, want %q", lines[0], want)
	}
	if !strings.Contains(lines[1], "cannot read") {
		t.Fatalf("second line = %q, want it to say the log cannot be read", lines[1])
	}
}

// writeLog writes path's log the way the runner names and fills it.
func writeLog(t *testing.T, logDir, path, body string) {
	t.Helper()
	name := filepath.Join(logDir, logName(path)+".log")
	if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
