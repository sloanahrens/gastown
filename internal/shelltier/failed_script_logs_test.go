package shelltier

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// failedScriptLogs returns each failed script's runner log, so the proof's
// failure carries the script's own verdict: the runner writes a script's
// combined output only to its log file, and the sweep pod's log file is gone
// by the time anyone reads the failure. A log that cannot be read is reported
// as such rather than left out.
func failedScriptLogs(t *testing.T, logDir string, failed []string) string {
	t.Helper()
	var b strings.Builder
	for _, path := range failed {
		logPath := filepath.Join(logDir, logName(path)+".log")
		log, err := os.ReadFile(logPath)
		if err != nil {
			fmt.Fprintf(&b, "%s: runner log unreadable: %v\n", path, err)
			continue
		}
		fmt.Fprintf(&b, "%s (%s):\n%s\n", path, logPath, log)
	}
	if b.Len() == 0 {
		return "none: no script failed\n"
	}
	return b.String()
}

// TestFailedScriptLogsNamesAnUnreadableLog requires a log that cannot be read
// to be reported, not dropped: a failure that prints nothing proves nothing
// about the script that failed. It is untagged so the default build runs it.
func TestFailedScriptLogsNamesAnUnreadableLog(t *testing.T) {
	t.Parallel()
	got := failedScriptLogs(t, t.TempDir(), []string{"scripts/gone_test.sh"})
	if !strings.Contains(got, "runner log unreadable") {
		t.Fatalf("failed script logs = %q, want the unreadable log named", got)
	}
}
