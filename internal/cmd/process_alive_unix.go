//go:build !windows

package cmd

// processAlive is the liveness check the capacity and sling paths use. It is
// the same predicate as isProcessRunning, so the two cannot disagree.
func processAlive(pid int) bool {
	return isProcessRunning(pid)
}
