package daemon

import "github.com/jonboulle/clockwork"

// clk returns the clock the daemon's waits run on: the injected one, or the
// real clock for a Daemon built without one (New sets none; neither do most
// test literals).
func (d *Daemon) clk() clockwork.Clock {
	if d.clock == nil {
		return clockwork.NewRealClock()
	}
	return d.clock
}
