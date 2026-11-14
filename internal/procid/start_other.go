//go:build !linux

package procid

// StartToken has no reader on this platform: ok is always false, so no ID
// can be taken and none ever matches (see ID.Running). Darwin is build-only
// since the gt-wrap cutover (gt-qq5kv4): nothing runs procid there, so this
// stub result is not a supported runtime value.
func StartToken(int) (string, bool) { return "", false }
