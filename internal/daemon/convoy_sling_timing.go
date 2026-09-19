package daemon

import "strings"

// slingStepPrefix is the per-step timing line `gt sling` prints on stderr
// (internal/cmd/sling_timing.go, gt-llg8).
const slingStepPrefix = "[sling] step "

// slingTimingLines returns the timing lines from a sling subprocess's stderr,
// in order, with everything else dropped. The feeder logs them on success so
// an 11-minute convoy-fed sling can be attributed from daemon.log alone.
func slingTimingLines(stderr string) []string {
	var lines []string
	for _, l := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(l, slingStepPrefix) {
			lines = append(lines, l)
		}
	}
	return lines
}

// slingErrorLine is the one-line summary of a failed sling's stderr: the
// first line that is not a timing line, falling back to the first line so a
// failure is never logged as an empty string.
func slingErrorLine(stderr string) string {
	first := ""
	for i, l := range strings.Split(stderr, "\n") {
		if i == 0 {
			first = l
		}
		if l != "" && !strings.HasPrefix(l, slingStepPrefix) {
			return l
		}
	}
	return first
}
