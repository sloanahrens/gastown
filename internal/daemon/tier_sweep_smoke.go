package daemon

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/sweeprunner"
)

// sweepSmokeRecord is what the daemon's startup smoke launch proved about
// launching the sweeper: the outcome, the first line of the error when it
// failed, when the launch was classified, and the daemon start it belongs to,
// so the verify probe can tell a stale record from this daemon's (gt-ohgh0v).
type sweepSmokeRecord struct {
	Time        time.Time `json:"time"`
	DaemonStart time.Time `json:"daemon_start"`
	Outcome     string    `json:"outcome"`
	Detail      string    `json:"detail"`
}

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
			return launch(ctx, st, spec)
		}
	}
	return tierSweepLaunch{exitCode: -1, err: errors.New("no runner is configured for the rig's sweep")}
}
