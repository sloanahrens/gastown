package daemon

import (
	"github.com/steveyegge/gastown/internal/landworker"
)

// gateAheadRedCheck returns the question the landing worker asks before it gates
// ahead of a rig's main: whether that main reads red. A nil state loader is the
// switch off and returns nil, so the worker asks nothing and behaves as before.
// A state or revert bead that cannot be read answers true and logs one line naming
// the rig and the error, so an unreadable record never lets a landing gate ahead.
func gateAheadRedCheck(rig string, state func(rig string) (landworker.MainState, error), revertOpen func(rig, beadID string) (bool, error), logf func(format string, args ...any)) func() bool {
	if state == nil {
		return nil
	}
	return func() bool {
		st, err := state(rig)
		if err != nil {
			logf("gate ahead: %s: main state unreadable, counting as red: %v", rig, err)
			return true
		}
		red, err := redHoldMainRedOf(rig, st, revertOpen)
		if err != nil {
			logf("gate ahead: %s: revert bead unreadable, counting as red: %v", rig, err)
			return true
		}
		return red
	}
}
