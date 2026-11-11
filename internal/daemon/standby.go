package daemon

import (
	"time"

	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/ring"
)

// standbyReason is the one standby check the daemon's background work reads: why
// the town stands by, or "" when it runs. The town ESTOP and a Waned Ring stand
// every caller by, and then the boot gate holds them while infrastructure is
// not answering (gt-niooz0). The operator's dispatch hold file parks dispatch
// only and is not read here, so the tier sweep and the maintenance gc keep
// running under it (gt-5cl4i). The Ring's key resolves through the daemon's
// environment seam.
func (d *Daemon) standbyReason(now time.Time) string {
	key := ring.TownKey(d.env)
	if estop.IsActive(d.config.TownRoot) {
		return "town ESTOP active (" + estop.FilePath(d.config.TownRoot) + ")"
	}
	if reason := ring.Hold(d.config.TownRoot, key, now); reason != "" {
		return reason
	}
	return d.bootReason()
}

// bootReason is the boot gate's answer: the probes the poller has not seen
// answer, or "" once it has seen them all. A daemon built with no boot probes
// (a test that does not set them) reads healthy at once.
func (d *Daemon) bootReason() string {
	if d.bootGate == nil {
		return ""
	}
	return d.bootGate.reason()
}

// startBackupOnly takes the window's backup, and no gc. It runs while a standby
// holds the window's cycle (gt-6mlik.6), and while an external Dolt server keeps
// the gc from running on this host. source names the caller in the log.
// maintenanceBackup is idempotent per day, and a deferral retries on the next
// tick. Any other end consumes the window through maintenanceGCFinishedAt, as
// a finished cycle does, so a failed backup escalates once per window rather
// than once per tick.
func (d *Daemon) startBackupOnly(window, source string) {
	if !d.maintenanceGCRunning.CompareAndSwap(false, true) {
		return
	}
	databases, err := d.backupDatabases()
	if err != nil {
		d.maintenanceGCRunning.Store(false)
		d.logger.Printf("scheduled_maintenance: %s for window %s skipped: listing databases: %v", source, window, err)
		return
	}
	if len(databases) == 0 {
		d.maintenanceGCRunning.Store(false)
		return
	}
	d.maintenance().dispatch(func() {
		defer d.maintenanceGCRunning.Store(false)
		res := d.maintenanceBackup(databases)
		d.logger.Printf("scheduled_maintenance: %s %s (window %s)", source, res.outcome, window)
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
