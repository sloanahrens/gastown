package daemon

import (
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// runReconciler is one pass of the reconciler patrol. It does nothing when the
// patrol is off, so a disabled town neither logs nor records a run. An active
// pass logs whether it is shadow (judges only) or live, keeps the observations
// it judged for dutyObservations to read, then records its completion time,
// which is what the patrol's due-ness check reads.
func (d *Daemon) runReconciler() {
	if !d.isPatrolActive("reconciler") {
		return
	}

	mode := "live"
	var rc *config.ReconcilerConfig
	if pc := d.patrol(); pc != nil && pc.Patrols != nil {
		rc = pc.Patrols.Reconciler
	}
	if rc.ParseShadow() {
		mode = "shadow"
	}
	d.logger.Printf("reconciler: pass (%s)", mode)

	now := time.Now()
	obs := d.observeDuties(d.ctx, now)
	d.dutyObsMu.Lock()
	d.dutyObs = obs
	d.dutyObsMu.Unlock()
	d.recordPatrolRun(&d.reconcilerRun, "reconciler", now)
}

// reconcilerInterval returns how often the reconciler patrol runs. It is the
// Reconciler block's interval when the block sets a valid one; a missing config,
// a missing block, or an interval ParseInterval refuses reads the config
// default, so a bad setting never stops the patrol.
func reconcilerInterval(pc *config.DaemonPatrolConfig) time.Duration {
	if pc == nil || pc.Patrols == nil {
		return config.DefaultReconcilerInterval
	}
	d, err := pc.Patrols.Reconciler.ParseInterval()
	if err != nil {
		return config.DefaultReconcilerInterval
	}
	return d
}
