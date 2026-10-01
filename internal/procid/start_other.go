//go:build !darwin && !linux

package procid

// StartToken has no reader on this platform: ok is always false, so no ID
// can be taken and none ever matches (see ID.Running).
func StartToken(int) (string, bool) { return "", false }
