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

// writeLog writes path's log the way the runner names and fills it.
func writeLog(t *testing.T, logDir, path, body string) {
	t.Helper()
	name := filepath.Join(logDir, logName(path)+".log")
	if err := os.WriteFile(name, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
