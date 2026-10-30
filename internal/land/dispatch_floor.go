package land

import (
	"time"

	"github.com/steveyegge/gastown/internal/events"
)

// dispatchLogBytes bounds the event log a dispatch-floor reading scans. The
// daemon prunes the log at 16 MiB, so this is half a pruned log: a bead
// dispatched within it is found, and the read is one bounded pass rather than a
// whole file on every landing that asks.
const dispatchLogBytes int64 = 8 << 20

// SlungAtIn answers when the run a bead was working under was dispatched at
// end, and the seat that dispatch named, read from the town's event log: the
// earliest sling event of that run, with the run ends and seat moves before end
// folded in (events.DispatchedBy) (gt-v7ptp).
//
// The log is read on the call rather than kept: only a landing whose seat wrote
// no start event asks, which is rare, and a log pruned between two landings
// would leave a kept clock naming a run it no longer holds. The read is capped
// at dispatchLogBytes, and a log past that is an error rather than a window of
// it: a window can begin inside the run, where a repeat sling would date the
// bead later than its dispatch and the interval would come out short — the one
// thing an upper bound must not do.
//
// A log that cannot be read is an error, not an empty answer, so the record is
// written without a time and the landing log says why.
func SlungAtIn(townRoot string) func(bead string, end time.Time) (time.Time, string, error) {
	return func(bead string, end time.Time) (time.Time, string, error) {
		evs, err := events.ReadBounded(townRoot, dispatchLogBytes)
		if err != nil {
			return time.Time{}, "", err
		}
		at, seat, ok := events.DispatchedBy(evs, bead, end)
		if !ok {
			return time.Time{}, "", nil
		}
		return at, seat, nil
	}
}
