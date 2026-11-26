package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/constants"
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

// sweepSmokeLaunch starts the first sweep stage of rig the runner takes and
// reports what the launch did, never waiting for the run's result: the
// launch's own start steps are the evidence (gt-ohgh0v).
//
// The run is taken down once the launch returns, except a busy one: it found
// a sweep Job of the rig already running and created nothing, so its teardown
// could end a run the smoke did not start.
func (d *Daemon) sweepSmokeLaunch(ctx context.Context, rig string) sweepSmokeRecord {
	l := d.sweepSmokeRun(ctx, rig)
	rec := classifySweepSmoke(l)
	if l.stop != nil && rec.Outcome != "busy" {
		l.stop()
	}
	rec.Time = d.clk().Now()
	return rec
}

// sweepSmokeRun renders the rig's sweep stages at origin/main and launches the
// first one the runner takes: the shell stage when runner.shell sends it off
// the host, the integration stage when it does not. A stage the config keeps
// on the host has no Spec and is skipped, never launched off the host. A
// failure before the launch is a failed launch that created nothing.
func (d *Daemon) sweepSmokeRun(ctx context.Context, rig string) tierSweepLaunch {
	read := d.tierSweepSeams.localMainSHA
	if read == nil {
		read = tierSweepLocalMainSHA
	}
	sha, err := read(constants.RigBareRepoPath(filepath.Join(d.config.TownRoot, rig)))
	if err != nil {
		return tierSweepLaunch{exitCode: -1, err: err}
	}
	for _, st := range tierSweepStages(rig) {
		st.tested = sha
		spec, offHost, err := d.tierSweepRunnerSpec(st)
		if err != nil {
			return tierSweepLaunch{exitCode: -1, err: err}
		}
		if !offHost {
			continue
		}
		launch := d.tierSweepSeams.launch
		if launch == nil {
			launch = d.tierSweepLaunchRunner
		}
		return launch(ctx, st, spec)
	}
	return tierSweepLaunch{exitCode: -1, err: errors.New("no runner is configured for the rig's sweep")}
}
