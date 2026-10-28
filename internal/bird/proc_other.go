//go:build !unix

package bird

import "errors"

// ErrUnsupportedPlatform is what liveness reads answer with where the platform
// has no start token: every agent reads as unknown, so nothing is pruned.
var ErrUnsupportedPlatform = errors.New("bird: process start tokens are unsupported on this platform")

// probeProcess has no probe to make here: the platform cannot say whether pid
// names a live process, so it says so rather than answering death.
func probeProcess(int) (bool, error) {
	return false, ErrUnsupportedPlatform
}
