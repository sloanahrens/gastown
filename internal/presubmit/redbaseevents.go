package presubmit

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strings"
)

// testEvent is the part of a `go test -json` event that RedBasePassed reads.
type testEvent struct {
	Action string
	Test   string
}

// RedBasePassed names, sorted and once each, the top-level tests with a pass event in out, the bytes `go test -json` printed (gt-32afsw). A subtest's pass (a Test holding a /) names nothing, and so does a package-level pass, which has no Test. fail and skip events name nothing. A line that is not JSON, as a build failure prints, is skipped. The output is split on newlines rather than read with a bufio.Scanner, so one long line cannot end the scan early.
func RedBasePassed(out []byte) []string {
	passed := map[string]bool{}
	for _, line := range bytes.Split(out, []byte("\n")) {
		var ev testEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Action == "pass" && ev.Test != "" && !strings.Contains(ev.Test, "/") {
			passed[ev.Test] = true
		}
	}
	return slices.Sorted(maps.Keys(passed))
}
