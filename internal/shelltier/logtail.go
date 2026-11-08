package shelltier

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/jev/scrub"
	"github.com/steveyegge/gastown/internal/tiersweep"
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
//
// The tail is raw script output, and putting it in the job log is the trade the
// bead accepts: the assertion is what a reader needs, and the log itself is
// deleted with the runner's temp directory. The tail text is scrubbed through
// internal/jev/scrub, the town's secret-pattern scrubber (sk-, ghp_, PEM
// blocks, credential assignments, tokens, emails, home paths), before it is
// printed, so a secret a script printed is redacted rather than copied into the
// job log. The header comes from tiersweep.ShellTailHeader, the one literal the
// sweep's parser matches, so printing and parsing cannot drift apart.
func WriteFailedLogTails(w io.Writer, logDir string, failed []string) {
	scrubber := scrub.Default()
	for _, path := range failed {
		fmt.Fprintf(w, "%s%s\n", tiersweep.ShellTailHeader, path)
		logPath := filepath.Join(logDir, logName(path)+".log")
		tail, err := tailFile(logPath, LogTailLines)
		if err != nil {
			// A colon after the header's words, where the header has a space,
			// keeps this line out of the prefix the sweep splits on.
			line, _ := scrubber.Scrub(fmt.Sprintf("%s: cannot read %s: %v", strings.TrimSuffix(tiersweep.ShellTailHeader, " "), logPath, err))
			fmt.Fprintln(w, line)
			continue
		}
		clean, _ := scrubber.Scrub(tail)
		// A failed write to the job log stream is not the script's verdict and
		// nothing here can act on it, as the header writes above.
		_, _ = fmt.Fprint(w, clean)
	}
}

// tailFile returns the last n lines of path, each ending in a newline. The file
// is streamed once through a ring of n lines, so a log far larger than the tail
// is never read into memory. Text with at most n lines is returned whole; an
// empty file is no lines.
func tailFile(path string, n int) (string, error) {
	if n < 1 {
		n = 1
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	ring := make([]string, n)
	read := 0
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			ring[read%n] = strings.TrimSuffix(line, "\n")
			read++
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return "", err
		}
	}
	if read == 0 {
		return "", nil
	}
	size, start := read, 0
	if read > n {
		size, start = n, read%n
	}
	var b strings.Builder
	for i := 0; i < size; i++ {
		b.WriteString(ring[(start+i)%n])
		b.WriteByte('\n')
	}
	return b.String(), nil
}
