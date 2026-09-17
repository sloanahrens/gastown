//go:build darwin

package beads

import (
	"os/exec"
	"strconv"
	"strings"
)

// getLoadavg returns the 1-minute average load on darwin via sysctl
// (kern.loadavg). Returns -1 on failure.
func getLoadavg() float64 {
	out, err := exec.Command("sysctl", "kern.loadavg").CombinedOutput()
	if err != nil {
		return -1
	}
	// Output: kern.loadavg: 1.23 2.34 3.45
	parts := strings.Split(strings.TrimSpace(string(out)), ":")
	if len(parts) != 2 {
		return -1
	}
	fields := strings.Fields(parts[1])
	if len(fields) < 1 {
		return -1
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(fields[0]), 64)
	if err != nil {
		return -1
	}
	return v
}
