//go:build !linux && !windows

package daemon

// loadAverage1Sysctl is a no-op on unsupported platforms. Darwin is build-only
// since the gt-wrap cutover (gt-qq5kv4): nothing runs the pressure probes
// there, so this 0 is not a supported runtime value.
func loadAverage1Sysctl() float64 {
	return 0
}

// availableMemoryGB is a no-op on unsupported platforms. Darwin is build-only
// since the gt-wrap cutover (gt-qq5kv4): nothing runs the pressure probes
// there, so this 0 is not a supported runtime value.
func availableMemoryGB() float64 {
	return 0
}
