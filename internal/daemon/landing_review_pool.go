package daemon

import (
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landworker"
)

// wireReviewPool is where the daemon wires the review pool into a rig's
// landing worker. The pool is per-rig and off by default: a rig the config
// does not list leaves the Lander and the Worker exactly as they were, because
// the pool may only add a queue-time review and never change how a landing
// runs (gt-eduozs).
func (d *Daemon) wireReviewPool(rig string, cfg *config.LandingWorkerConfig, l *land.Lander, w *landworker.Worker, workRoot string) { //nolint:unparam // l, w and workRoot are the pool's wiring targets, used by the on path a later bead of gt-s47qp4 adds
	on, _, _, err := cfg.ReviewPoolFor(rig)
	if err != nil {
		// A bad review_pool block fails loud and leaves the pool off: the rig
		// keeps landing without one until the config is fixed, because the next
		// worker construction rereads the config and this function never retries
		// (gt-eduozs).
		d.logger.Printf("landing_worker: %s: review pool off: %v", rig, err)
		return
	}
	if !on {
		return
	}
	// On for this rig: wiring the pool into l, w and workRoot is a later bead
	// of the map, gt-s47qp4.
}
