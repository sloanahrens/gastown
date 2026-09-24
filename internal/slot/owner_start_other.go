//go:build !darwin && !linux

package slot

// processStartToken has no reader on this platform: ok is always false, and
// owner verdicts fall back to the age/reaper rules for old containers.
func processStartToken(int) (string, bool) { return "", false }
