//go:build windows

package slot

// processGone never claims a process is gone on Windows: there is no probe
// here that is certain, and a false "gone" would delete a live suite's
// containers. Labeled containers are judged by the age/reaper rules instead.
func processGone(int) bool { return false }
