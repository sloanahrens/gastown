package installpending

import "errors"

// aliveOnOpenError classifies an OpenProcess failure. Only an invalid pid is
// a "gone" answer; access denied and every other error read as alive, because
// a live writer read as dead would let a reader start work beside the install
// (gt-vc34c). errInvalidID is the platform's invalid-pid error, passed in so
// the decision is unit-tested on every host.
func aliveOnOpenError(err, errInvalidID error) bool {
	return !errors.Is(err, errInvalidID)
}

// aliveOnExitCode classifies the second Windows probe. An exit-code read that
// failed leaves the process alive (fail safe), and a code the platform does not
// report for a live process means it has exited.
func aliveOnExitCode(err error, code, stillActiveCode uint32) bool {
	if err != nil {
		return true
	}
	return code == stillActiveCode
}
