package tiersweep

import (
	"fmt"
	"io"
)

// cgroupNote reports a sweep's first cgroup sampling failure: a missing
// reading costs its timings line, not the run's verdict (gt-3wb27k).
type cgroupNote struct {
	reported bool
}

// report writes err's line once. A nil error or writer leaves the one line
// unspent; every later error is silent. The caller serializes calls.
func (n *cgroupNote) report(w io.Writer, err error) {
	if err == nil || w == nil || n.reported {
		return
	}
	n.reported = true
	fmt.Fprintf(w, "%s: timings: cgroup: %v (%s)\n", Name, err, timingsGoesOn)
}
