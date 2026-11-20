package daemon

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// nodePruneReport is what one node-prune run's log says it planned or did.
type nodePruneReport struct {
	Lines   []string // the would-remove lines, in log order
	Planned int      // the dry run's planned images
	Removed int      // the apply run's removed images
	Failed  int      // the apply run's failed removals
}

// The node-prune helper's log lines this report reads (bead gt-i3qz0l).
const (
	nodePruneWouldRemove = "node-prune: would remove "
	nodePruneDryRun      = "node-prune: dry run planned "
	nodePruneApplied     = "node-prune: applied removed "
)

// parseNodePruneLog reads the log of one node-prune run into its report. A log
// with no summary line, more than one, a count that is not a non-negative
// integer or a truncated line is an error naming the problem, never a zero
// report: a run whose plan could not be read must not look like an empty one.
func parseNodePruneLog(out []byte) (nodePruneReport, error) {
	var rep nodePruneReport
	summary := ""
	for _, raw := range strings.Split(string(out), "\n") {
		line := strings.TrimRight(raw, " \t\r")
		if summary != "" && (strings.HasPrefix(line, nodePruneDryRun) || strings.HasPrefix(line, nodePruneApplied)) {
			return nodePruneReport{}, fmt.Errorf("node-prune: the log holds more than one summary, %q after %q", line, summary)
		}
		switch {
		case strings.HasPrefix(line, nodePruneWouldRemove):
			if f := strings.Fields(line); len(f) != 7 || f[5] != "created" {
				return nodePruneReport{}, fmt.Errorf("node-prune: cannot read the log line %q", line)
			}
			rep.Lines = append(rep.Lines, line)
		case strings.HasPrefix(line, nodePruneDryRun):
			rest, ok := strings.CutSuffix(line[len(nodePruneDryRun):], " images")
			n, counted := nodePruneCount(rest)
			if !ok || !counted {
				return nodePruneReport{}, fmt.Errorf("node-prune: cannot read the summary %q", line)
			}
			rep.Planned, summary = n, line
		case strings.HasPrefix(line, nodePruneApplied):
			f := strings.Fields(line[len(nodePruneApplied):])
			if len(f) != 3 || f[1] != "failed" {
				return nodePruneReport{}, fmt.Errorf("node-prune: cannot read the summary %q", line)
			}
			removed, removedOK := nodePruneCount(f[0])
			failed, failedOK := nodePruneCount(f[2])
			if !removedOK || !failedOK {
				return nodePruneReport{}, fmt.Errorf("node-prune: cannot read the summary %q", line)
			}
			rep.Removed, rep.Failed, summary = removed, failed, line
		}
	}
	if summary == "" {
		return nodePruneReport{}, errors.New("node-prune: the log has no summary line")
	}
	return rep, nil
}

// nodePruneCount reads s as a non-negative count of images.
func nodePruneCount(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil && n >= 0
}
