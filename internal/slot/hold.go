package slot

import "sync"

// liveHolds keeps every flock this process holds reachable until its holder
// releases it (gt-1okjo).
//
// lock.FlockTryAcquire's unlock func is the only reference to the lock file it
// opened, and os.File closes itself when it is garbage collected: closing the
// descriptor drops the flock. So a Handle or MarkerHandle its holder stopped
// referencing without calling Release used to release its slot at whatever
// moment the collector reached it — silently, with the owner file and the
// history entry still claiming a hold, and a second suite free to start
// beside the first. A hold ends on Release or on the holder's death, never on
// a collection cycle; this registry is what makes that so.
var liveHolds sync.Map // *holdKey -> func() (the flock's unlock)

// holdKey identifies one entry of liveHolds.
type holdKey struct{ _ byte }

// pinHold registers unlock in liveHolds and returns the release to use in its
// place: it unregisters the hold and unlocks, once, however often it is called.
func pinHold(unlock func()) (release func()) {
	key := new(holdKey)
	liveHolds.Store(key, unlock)
	var once sync.Once
	return func() {
		once.Do(func() {
			liveHolds.Delete(key)
			unlock()
		})
	}
}
