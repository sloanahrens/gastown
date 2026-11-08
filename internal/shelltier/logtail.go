package shelltier

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// LogTailLines is the number of a failed script's log lines the tail prints:
// enough to hold the failing assertion, bounded so one chatty script cannot
// fill the job log.
const LogTailLines = 80

// WriteFailedLogTails writes each failed script's log tail to w: a header line
// `tier-sweep: shell log tail <script>` and the log's last LogTailLines lines.
// The runner calls it after a RED run's summary, so the job log carries the
// failed assertion, which otherwise stays in a temp directory on the runner.
// A log that cannot be read is named and skipped; nothing here changes the
// run's verdict. With no failed script it writes nothing.
func WriteFailedLogTails(w io.Writer, logDir string, failed []string) {
	for _, path := range failed {
		fmt.Fprintf(w, "tier-sweep: shell log tail %s\n", path)
		logPath := filepath.Join(logDir, logName(path)+".log")
		log, err := os.ReadFile(logPath)
		if err != nil {
			fmt.Fprintf(w, "tier-sweep: shell log tail: cannot read %s: %v\n", logPath, err)
			continue
		}
		fmt.Fprintf(w, "%s", tailLines(string(log), LogTailLines))
	}
}

// tailLines is the last n lines of text, each ending in a newline. Text with at
// most n lines is returned whole; empty text is no lines.
func tailLines(text string, n int) string {
	if text == "" {
		return ""
	}
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n") + "\n"
}
