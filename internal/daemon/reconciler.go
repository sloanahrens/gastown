package daemon

import (
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/reconcile"
)

// runReconciler is one pass of the reconciler patrol. It does nothing when the
// patrol is off, so a disabled town neither logs nor records a run. An active
// pass logs whether it is shadow (judges only) or live, keeps the observations
// it judged for dutyObservations to read, logs one line for each duty it finds
// due or overdue, writes them to the duty state record the jobs list reads, then
// records its completion time, which is what the patrol's due-ness check reads.
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
	limits := dutyLimits(rc, obs)
	d.logWouldRun(mode, obs, limits)
	d.recordDutyState(now, obs, limits)
	d.recordPatrolRun(&d.reconcilerRun, "reconciler", now)
}

// logWouldRun logs one line for each duty a pass finds due or overdue: the
// duty's name and verdict, how old its last good result is, and the limit it
// was judged by. A shadow pass names the mode and marks the line "(would run)",
// so the log says no duty ran; a live pass logs plain. An ok duty logs nothing,
// and an unknown duty logs nothing here.
func (d *Daemon) logWouldRun(mode string, obs []reconcile.Observation, limits map[string]time.Duration) {
	for _, o := range obs {
		if o.Verdict != reconcile.VerdictDue && o.Verdict != reconcile.VerdictOverdue {
			continue
		}
		if mode == "shadow" {
			d.logger.Printf("reconciler: shadow: %s %s age %s limit %s (would run)", o.Duty, o.Verdict, o.Age, limits[o.Duty])
			continue
		}
		d.logger.Printf("reconciler: %s %s age %s limit %s", o.Duty, o.Verdict, o.Age, limits[o.Duty])
	}
}

// recordDutyState writes the pass's observations and their limits to the duty
// state record under the town root. A failed write is logged once for the pass
// and never stops it.
func (d *Daemon) recordDutyState(at time.Time, obs []reconcile.Observation, limits map[string]time.Duration) {
	townRoot := ""
	if d.config != nil {
		townRoot = d.config.TownRoot
	}
	if err := WriteDutyState(townRoot, at, obs, limits); err != nil {
		d.logger.Printf("reconciler: write duty state: %v", err)
	}
}

// dutyLimits maps each observed duty to the limit its family reads from rc. A
// gc observation is named gc:<db> and reads the gc limit. A limit ParseLimit
// refuses leaves its duty out, so the record holds 0 for it; the observation
// already carries the refused setting as its error, so nothing is logged here.
func dutyLimits(rc *config.ReconcilerConfig, obs []reconcile.Observation) map[string]time.Duration {
	limits := make(map[string]time.Duration, len(obs))
	for _, o := range obs {
		family, _, _ := strings.Cut(o.Duty, ":")
		limit, err := rc.ParseLimit(family)
		if err != nil {
			continue
		}
		limits[o.Duty] = limit
	}
	return limits
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
