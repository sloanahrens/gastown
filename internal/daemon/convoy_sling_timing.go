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
