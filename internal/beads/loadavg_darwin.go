//go:build darwin || linux

package beads

import "syscall"

// getLoadavg returns the 1-minute average load. Returns -1 on platforms
// that don't expose load averages (e.g. Windows).
func getLoadavg() float64 {
	var loadavg [3]float64
	if _, err := syscall.Getloadavg(); err == nil {
		return loadavg[0]
	}
	return -1
}
