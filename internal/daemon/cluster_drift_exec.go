package daemon

import (
	"fmt"
	"strings"
)

// clusterDriftCommand is the command whose one run this parse reads, named in
// every error it raises so the reader knows which answer was refused.
const clusterDriftCommand = "gt town cluster drift"

// The exit statuses a drift run answers with: 0 when the cluster is in step and
// 1 when the render differs. Every other status is refused (gt-hhsi66).
const (
	clusterDriftExitInStep  = 0
	clusterDriftExitDiffers = 1
)

// parseClusterDriftRun reads one run of gt town cluster drift: its exit status,
// its stdout, and whether the child ran at all. Exit 0 returns an empty result
// and no error, whatever stdout holds. Exit 1 carries one "<resource>: <JSON
// path ...>" line per difference, and the non-empty lines are the result. Exit 1
// with no line, any other status, and a non-nil runErr are errors with an empty
// result, whatever the status: only a run that exited 0 and did run is a cluster
// in step (gt-hhsi66).
func parseClusterDriftRun(exit int, stdout []byte, runErr error) (clusterDriftResult, error) {
	if runErr != nil {
		return clusterDriftResult{}, fmt.Errorf("running %s: %w", clusterDriftCommand, runErr)
	}
	switch exit {
	case clusterDriftExitInStep:
		return clusterDriftResult{}, nil
	case clusterDriftExitDiffers:
		lines := clusterDriftLines(stdout)
		if len(lines) == 0 {
			return clusterDriftResult{}, fmt.Errorf("%s exited 1 with no difference line, so no drift can be named", clusterDriftCommand)
		}
		return clusterDriftResult{Differing: lines}, nil
	default:
		return clusterDriftResult{}, fmt.Errorf("%s exited %d, which is neither in step (0) nor differing (1)", clusterDriftCommand, exit)
	}
}

// clusterDriftLines is the non-empty lines of stdout, kept exactly as printed:
// the command writes one line per difference, and a blank line names nothing.
func clusterDriftLines(stdout []byte) []string {
	var lines []string
	for _, line := range strings.Split(string(stdout), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}
