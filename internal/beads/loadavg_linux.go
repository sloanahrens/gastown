//go:build linux

package beads

import (
	"fmt"
	"os"
	"strings"
)

// getLoadavg returns the 1-minute load average on Linux by reading /proc/loadavg.
// /proc/loadavg format: "0.00 0.01 0.05 1/123 12345"
func getLoadavg() (float64, error) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return -1, fmt.Errorf("read /proc/loadavg: %w", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return -1, fmt.Errorf("empty /proc/loadavg")
	}
	return parseLoadavgField(fields[0])
}
