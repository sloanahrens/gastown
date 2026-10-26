package daemon

import "time"

// censusLogInterval is how often the daemon logs its own bd call census
// (gt-7h3vm).
const censusLogInterval = time.Minute

// startCensusLog starts the bd call census (gt-7h3vm): it logs one line per
// censusLogInterval through the daemon's logger until the daemon's context
// ends. The returned func stops the ticker.
func (d *Daemon) startCensusLog() (stop func()) {
	every, stopTicker := d.seams.startCensusTicker(censusLogInterval)
	go d.seams.runCensusLog(d.ctx, every, d.logger.Printf)
	return stopTicker
}
