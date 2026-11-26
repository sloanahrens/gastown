package daemon

import (
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landworker"
)

// wireBookQueue gives a rig's landing worker deferred bookkeeping (gt-jz0tp6).
// The switch is per rig, so a rig the config does not list keeps the inline
// path with its Lander and Worker untouched. A listed rig's Lander returns
// after the durable writes, and its queue runs the rest, logging a panicked
// job through the daemon's logger (gt-maqaag).
func (d *Daemon) wireBookQueue(rig string, cfg *config.LandingWorkerConfig, l *land.Lander, w *landworker.Worker) {
	if !cfg.DeferredBookkeepingFor(rig) {
		return
	}
	l.DeferBookkeeping = true
	w.Books = &landworker.BookQueue{Logf: d.logger.Printf}
}
