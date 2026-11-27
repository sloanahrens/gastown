package daemon

import (
	"context"
	"sync"
	"time"
)

// clusterDriftInterval is the drift patrol's cadence: an answer is kept this
// long from the run that took it (gt-0cdmgq).
const clusterDriftInterval = 15 * time.Minute

// clusterDriftCache keeps the drift patrol's last answer for
// clusterDriftInterval: the compare runs once per cadence however many times the
// heartbeat asks in between, a failed run kept and returned like any answer
// until the next run. It starts no goroutine, timer or sleep, and never retries
// inside a call (gt-0cdmgq).
type clusterDriftCache struct {
	// mu covers the whole call, run included, so two callers at once share one
	// run.
	mu sync.Mutex
	// ran is false until the first run, since the zero at is no answer.
	ran bool
	at  time.Time
	res clusterDriftResult
	err error
	// started is when the daemon started, set once in New before the daemon
	// runs, so the patrol can wait clusterDriftStartDelay after it (gt-myyr6e).
	started time.Time
}

// Answer returns the compare's answer for now, running run only when no answer
// is held or the held one was taken clusterDriftInterval ago or more. A now
// before the last run, a clock stepped back, is answered from the cache rather
// than run again.
func (c *clusterDriftCache) Answer(ctx context.Context, now time.Time, run func(context.Context) (clusterDriftResult, error)) (clusterDriftResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ran && now.Sub(c.at) < clusterDriftInterval {
		return c.res, c.err
	}
	c.res, c.err = run(ctx)
	c.at = now
	c.ran = true
	return c.res, c.err
}
