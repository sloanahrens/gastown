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

// sweepSmokeLaunch starts one sweep stage of rig and reports what the launch
// did, never waiting for the run's result: the launch's own start steps are
// the evidence. It exists so the update Job's verify can ask whether the new
// daemon's sweeper can start a run at all, which only a real launch exercises
// (gt-ohgh0v).
//
// The run is taken down once the launch returns, except a busy one: it found
// a sweep Job of the rig already running and created nothing, so its teardown
// could end a run the smoke did not start.
func (d *Daemon) sweepSmokeLaunch(ctx context.Context, rig string) sweepSmokeRecord {
	failed := func(err error) sweepSmokeRecord {
		rec := classifySweepSmoke(tierSweepLaunch{exitCode: -1, err: err})
		rec.Time = d.clk().Now()
		return rec
	}
	read := d.tierSweepSeams.localMainSHA
	if read == nil {
		read = tierSweepLocalMainSHA
	}
	repo := constants.RigBareRepoPath(filepath.Join(d.config.TownRoot, rig))
	sha, err := read(repo)
	if err != nil {
		return failed(err)
	}
	st, spec, err := d.sweepSmokeStage(rig, sha)
	if err != nil {
		return failed(err)
	}
	launch := d.tierSweepSeams.launch
	if launch == nil {
		launch = d.tierSweepLaunchRunner
	}
	started := launch(ctx, st, spec)
	rec := classifySweepSmoke(started)
	if started.stop != nil && rec.Outcome != "busy" {
		started.stop()
	}
	rec.Time = d.clk().Now()
	return rec
}

// sweepSmokeStage returns the first stage of the rig's sweep the runner takes,
// rendered at sha: the shell stage when runner.shell sends it off the host, the
// integration stage when it does not. A stage the config keeps on the host has
// no Spec, so it is skipped rather than launched off the host.
func (d *Daemon) sweepSmokeStage(rig, sha string) (tierSweepStage, sweeprunner.Spec, error) {
	for _, st := range tierSweepStages(rig) {
		st.tested = sha
		spec, offHost, err := d.tierSweepRunnerSpec(st)
		if err != nil {
			return tierSweepStage{}, sweeprunner.Spec{}, err
		}
		if offHost {
			return st, spec, nil
		}
	}
	return tierSweepStage{}, sweeprunner.Spec{}, errors.New("no runner is configured for the rig's sweep")
}
