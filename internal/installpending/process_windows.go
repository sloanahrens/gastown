//go:build windows

package installpending

// processAlive answers true for every positive pid: Windows has no signal-0
// probe, so Ceiling is the only bound there, and reading a live writer as dead
// would let a sling start beside the install (gt-vc34c).
func processAlive(pid int) bool { return pid > 0 }
