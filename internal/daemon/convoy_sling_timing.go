package daemon

import (
	"strings"

	"github.com/steveyegge/gastown/internal/sling"
)

// slingStepPrefix is the per-step timing line a dispatch writes
// (internal/sling/timer.go, gt-llg8).
const slingStepPrefix = sling.StepPrefix

// slingTimingLines returns the timing lines from a dispatch's output, in
// order, with everything else dropped. The feeder logs them so an 11-minute
// convoy-fed sling can be attributed from daemon.log alone.
//
// The output is the engine's step timer buffer on the in-process path, and the
// child's stderr on the paths that still exec gt (scheduled slings).
func slingTimingLines(output string) []string {
	var lines []string
	for _, l := range strings.Split(output, "\n") {
		if strings.HasPrefix(l, slingStepPrefix) {
			lines = append(lines, l)
		}
	}
	return lines
}

// slingErrorLine is the one-line summary of a failed dispatch's output: the
// first line that is not a timing line, falling back to the first line so a
// failure is never logged as an empty string.
func slingErrorLine(output string) string {
	first := ""
	for i, l := range strings.Split(output, "\n") {
		if i == 0 {
			first = l
		}
		if l != "" && !strings.HasPrefix(l, slingStepPrefix) {
			return l
		}
	}
	return first
}
