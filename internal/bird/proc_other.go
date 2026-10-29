//go:build !unix

package bird

// probeProcess has no probe to make here: the platform cannot say whether pid
// names a live process, so it answers ErrUnsupportedPlatform instead of death.
// tokenFrom hands that answer to its caller, so every liveness read on such a
// platform fails: nothing is pruned and no name is handed out (see
// ErrUnsupportedPlatform).
func probeProcess(int) (bool, error) {
	return false, ErrUnsupportedPlatform
}
