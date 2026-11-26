package cmd

import "github.com/steveyegge/gastown/internal/daemon"

// seatNameAvoider returns the name pool's avoider for seats: it answers true for
// a seat whose sandbox the guard lists as stale, so the pool does not draw that
// name. sandboxName maps a seat to its sandbox's name. A nil guard avoids no
// seat. With a guard but no sandboxName, no stale sandbox can be ruled out, so
// every seat is avoided.
func seatNameAvoider(guard *daemon.SeatNameGuard, sandboxName func(seat string) string) func(seat string) bool {
	return func(seat string) bool {
		if guard == nil {
			return false
		}
		if sandboxName == nil {
			return true
		}
		_, stale := guard.Stale(sandboxName(seat))
		return stale
	}
}
