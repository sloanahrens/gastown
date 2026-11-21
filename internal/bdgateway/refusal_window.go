package bdgateway

import (
	"sync"
	"time"
)

const refusalLinesPerWindow = 10        // refusal lines one window writes
const refusalWindowLength = time.Minute // a window's span, from its first call

// refusalWindow bounds the pre-auth refusal lines. The handler is concurrent, so
// the window carries its own lock.
type refusalWindow struct {
	mu         sync.Mutex
	start      time.Time
	started    bool
	calls      int
	suppressed uint64
}

// allow reports whether the call at now writes its line. suppressed is the count
// the previous window dropped, returned once on the first call after it.
func (w *refusalWindow) allow(now time.Time) (write bool, suppressed uint64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.started || !now.Before(w.start.Add(refusalWindowLength)) {
		suppressed = w.suppressed
		w.suppressed, w.start, w.started, w.calls = 0, now, true, 0
	}
	w.calls++
	if w.calls <= refusalLinesPerWindow {
		return true, suppressed
	}
	w.suppressed++
	return false, suppressed
}
