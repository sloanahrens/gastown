package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/sweeprunner"
)

// SweepSmokeRecord is what the daemon's startup smoke launch proved about
// launching the sweeper: the outcome, the first line of the error when it
// failed, when the launch was classified, and the daemon start it belongs to,
// so the verify probe can tell a stale record from this daemon's (gt-ohgh0v).
type SweepSmokeRecord struct {
	Time        time.Time `json:"time"`
	DaemonStart time.Time `json:"daemon_start"`
	Outcome     string    `json:"outcome"`
	Detail      string    `json:"detail"`
}

type sweepSmokeRecord = SweepSmokeRecord

// classifySweepSmoke names the outcome a sweep launch reported: busy while a
// sweep Job of the rig holds the claim guard, declined when the capacity
// preflight refused it, failed with the error's first line as Detail, and
// started otherwise. It fills only Outcome and Detail; the caller stamps Time
// and DaemonStart (gt-ohgh0v).
func classifySweepSmoke(l tierSweepLaunch) sweepSmokeRecord {
	var busy *sweeprunner.ActiveJob
	if errors.As(l.err, &busy) {
		return sweepSmokeRecord{Outcome: "busy"}
	}
	if l.declined {
		return sweepSmokeRecord{Outcome: "declined"}
	}
	if l.err != nil {
		detail, _, _ := strings.Cut(l.err.Error(), "\n")
		return sweepSmokeRecord{Outcome: "failed", Detail: detail}
	}
	return sweepSmokeRecord{Outcome: "started"}
}

// sweepSmokeLaunch launches the rig's first sweep stage the runner takes and
// reports the outcome without waiting for the run (gt-ohgh0v). The stop runs
// once the launch returns, except for a busy launch: it created nothing, and
// its stop could end the run already going.
func (d *Daemon) sweepSmokeLaunch(ctx context.Context, rig string) sweepSmokeRecord {
	l := d.sweepSmokeRun(ctx, rig)
	rec := classifySweepSmoke(l)
	if l.stop != nil && rec.Outcome != "busy" {
		l.stop()
	}
	rec.Time = d.clk().Now()
	return rec
}

// sweepSmokeRun renders each sweep stage at origin/main and launches the first
// the runner takes off the host. A stage the config keeps on the host has no
// Spec. A failure before the launch is a failed launch that created nothing.
func (d *Daemon) sweepSmokeRun(ctx context.Context, rig string) tierSweepLaunch {
	sha, err := d.tierSweepLocalMain(rig)
	if err != nil {
		return tierSweepLaunch{exitCode: -1, err: err}
	}
	for _, st := range tierSweepStages(rig) {
		st.tested = sha
		spec, offHost, err := d.tierSweepRunnerSpec(st)
		if err != nil {
			return tierSweepLaunch{exitCode: -1, err: err}
		}
		if offHost {
			launch := d.tierSweepSeams.launch
			if launch == nil {
				launch = d.tierSweepLaunchRunner
			}
			return launch(ctx, st, sweepSmokeSpec(spec))
		}
	}
	return tierSweepLaunch{exitCode: -1, err: errors.New("no runner is configured for the rig's sweep")}
}

// sweepSmokeSpec gives the smoke a run of its own: the rendered spec's RunID
// gains a "-smoke" suffix and its OutDir moves to that name beside the sweep's.
// A running sweep's Job is then never this run's Job, so the claim guard refuses
// it as a foreign Job (busy) instead of the smoke adopting that run and taking
// it down (gt-9ieuas).
func sweepSmokeSpec(spec sweeprunner.Spec) sweeprunner.Spec {
	spec.RunID += "-smoke"
	spec.OutDir = filepath.Join(filepath.Dir(spec.OutDir), spec.RunID)
	return spec
}

// sweepSmokeUnderGuard launches the smoke holding tierSweepRunning, the guard a
// cycle single-flights with (tier_sweep.go), and releases it after the launch
// and its stop. A cycle holding the guard is not run beside — both fill the
// rig's one repo claim and sync pod — so the smoke launches nothing and records
// busy, leaving the cycle to the heartbeat's retry (gt-mn57nd).
func (d *Daemon) sweepSmokeUnderGuard(ctx context.Context, rig string) sweepSmokeRecord {
	now := d.clk().Now()
	if !d.tierSweepRunning.CompareAndSwap(false, true) {
		return sweepSmokeRecord{Time: now, Outcome: "busy", Detail: "a sweep cycle holds the guard"}
	}
	// The start a cycle would stamp: the smoke holds the guard the same way.
	d.tierSweepStartedAt.Store(now.UnixNano())
	defer d.tierSweepRunning.Store(false)
	return d.sweepSmokeLaunch(ctx, rig)
}

// sweepSmokeRecordPath is the file the startup smoke launch's record is written
// to under the town root, read by the verify probe (gt-r2497d).
func sweepSmokeRecordPath(townRoot string) string {
	return filepath.Join(constants.TownRuntimePath(townRoot), "tier-sweep", "launch-smoke.json")
}

// recordSweepSmokeAtStart runs the smoke launch once for the first rig when
// tier_sweep is active with a runner, and writes its record stamped with the
// daemon's start. The daemon starts it in its own goroutine, so the launch never
// holds up the start. A write error is logged and the start goes on
// (gt-r2497d).
func (d *Daemon) recordSweepSmokeAtStart(ctx context.Context, daemonStart time.Time) {
	if d.patrolOffReason("tier_sweep") != "" || tierSweepRunnerConfig(d.patrol()) == nil {
		return
	}
	rigs := tierSweepRigs(d.patrol(), d.getKnownRigs())
	if len(rigs) == 0 {
		return
	}
	rec := d.sweepSmokeUnderGuard(ctx, rigs[0])
	rec.DaemonStart = daemonStart
	if err := atomicfile.EnsureDirAndWriteJSON(sweepSmokeRecordPath(d.config.TownRoot), rec); err != nil {
		d.logger.Printf("Warning: tier-sweep launch smoke record not written: %v", err)
	}
}
