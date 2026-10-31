package daemon

import (
	"time"

	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/ring"
)

// standbyReason is the one standby check the daemon's background work reads: why
// the town stands by, or "" when it runs. The town ESTOP and a Waned Ring stand
// every caller by. The operator's hold file stands by only the callers that ask
// for it (holdFile): the tier sweep and the maintenance gc. The heartbeat's
// lifecycle steps leave it out, because the drain runs on the hold file alone
// (gt-6mlik.6). The Ring's key resolves through the daemon's environment seam.
func (d *Daemon) standbyReason(now time.Time, holdFile bool) string {
	key := ring.TownKey(d.env)
	if holdFile {
		return dispatch.OperatorHoldWithKey(d.config.TownRoot, key, now)
	}
	if estop.IsActive(d.config.TownRoot) {
		return "town ESTOP active (" + estop.FilePath(d.config.TownRoot) + ")"
	}
	return ring.Hold(d.config.TownRoot, key, now)
}

// startStandbyBackup takes the window's backup, and no gc, while a standby
// holds the window's cycle (gt-6mlik.6). maintenanceBackup is idempotent per
// day, and a deferral retries on the next tick. Any other end consumes the
// window through maintenanceGCFinishedAt, as a finished cycle does, so a
// failed backup escalates once per window rather than once per tick.
func (d *Daemon) startStandbyBackup(window string) {
	if !d.maintenanceGCRunning.CompareAndSwap(false, true) {
		return
	}
	dataDir := d.maintenanceDataDir()
	databases, err := d.maintenance().gcDatabases(dataDir)
	if err != nil {
		d.maintenanceGCRunning.Store(false)
		d.logger.Printf("scheduled_maintenance: standby backup for window %s skipped: discovering databases in %s: %v", window, dataDir, err)
		return
	}
	if len(databases) == 0 {
		d.maintenanceGCRunning.Store(false)
		return
	}
	d.maintenance().dispatch(func() {
		defer d.maintenanceGCRunning.Store(false)
		res := d.maintenanceBackup(databases)
		d.logger.Printf("scheduled_maintenance: standby backup %s (window %s)", res.outcome, window)
		if res.outcome == gcOutcomeDeferred || res.outcome == gcOutcomeInterrupted {
			return
		}
		d.maintenanceGCFinishedAt.Store(d.maintenance().now().UnixNano())
	})
}

// logStandby logs a standby change once: the reason when it appears or changes,
// and the lift when it clears. source names the site that logs it.
func (d *Daemon) logStandby(latch *dispatch.HoldLatch, source, reason string) {
	if !latch.Changed(reason) {
		return
	}
	if reason == "" {
		d.logger.Printf("%s: standby lifted; resuming", source)
		return
	}
	d.logger.Printf("%s: standing by: %s", source, reason)
}
