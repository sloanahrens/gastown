package daemon

// cloud_check is the town's interval cloud check: the devops `cloudcheck`
// binary runs in the daemon's process tree, writes the status file the
// dashboard's Cloud pane reads, and raises one attention item while it fails
// (gt-c57pax).
//
// The patrol is opt-in. It runs a program on a timer, so only an explicit
// daemon.patrols.cloud_check.enabled turns it on. The GCP service-account key
// is read from the file the daemon-gcp-key Secret mounts; the patrol stats the
// file and passes its path to the child's environment, and never reads, logs or
// reports the key itself. A run without the key does nothing and says so once,
// because the check cannot reach the cloud without it.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
)

const (
	// cloudCheckDefaultInterval is the run interval when the town names none:
	// the ten minutes the Mac plugin ran on (gt-im2c8).
	cloudCheckDefaultInterval = 10 * time.Minute

	// cloudCheckRunTimeout bounds one check. The binary probes one URL per app
	// and reads the control cluster through kubectl, each with its own timeout,
	// so a run still going after five minutes is stuck rather than slow.
	cloudCheckRunTimeout = 5 * time.Minute

	// cloudCheckStatusRelPath is the status file's path inside the town root:
	// the path the dashboard's CLOUD pane reads (internal/cmd/dashboard.go
	// dashboardCloudChecksPath). The binary writes it through --json, so the
	// schema stays the one the readers already trust.
	cloudCheckStatusRelPath = ".runtime/cloud-check/status.json"

	// cloudCheckJSONArg is the binary's flag naming the status file.
	cloudCheckJSONArg = "--json"

	// cloudCheckQuietArg keeps the run's own report off stdout: the patrol
	// reads the status file, not the printed report.
	cloudCheckQuietArg = "--quiet"

	// cloudCheckKeyEnv is the variable the child reads its GCP service-account
	// key from. The key's path is the only thing set; the file's contents stay
	// in the file.
	cloudCheckKeyEnv = "GOOGLE_APPLICATION_CREDENTIALS"

	// cloudCheckAlertKey is the one escalation a failing run raises. A repeated
	// failure refreshes it instead of opening another; a good run clears it.
	cloudCheckAlertKey = "cloud_check:failed"
)

// cloudCheckSeams are the patrol's clock, runner and side effects, replaceable
// in tests through Daemon.cloudCheck. Each nil field is the production
// behavior (see cloudCheckSeamsOf).
type cloudCheckSeams struct {
	// now is the clock that decides when the interval has passed and how long a
	// run took.
	now func() time.Time
	// run runs the configured program with argv and the extra environment
	// entries; nil is cloudCheckRun.
	run func(ctx context.Context, name string, args, env []string) error
	// stat reports the key file; nil is os.Stat.
	stat func(name string) (fs.FileInfo, error)
	// keyFile resolves the key path from the setting; nil is KeyFilePath.
	keyFile func(c *agentconfig.CloudCheckConfig) string
	// dispatch runs a check off the loop goroutine, so a slow run does not hold
	// the heartbeat. Nil means one goroutine per check.
	dispatch func(fn func())
	// escalate raises or refreshes the failure item; nil is the daemon's alert
	// helper.
	escalate func(key, message string) error
	// clear closes the failure item; nil is the daemon's alert helper.
	clear func(key string) error
}

// cloudCheckSeamsOf returns d's patrol seams with every unset one filled in with
// its production behavior.
func (d *Daemon) cloudCheckSeamsOf() cloudCheckSeams {
	s := d.cloudCheck
	if s.now == nil {
		s.now = func() time.Time { return d.clk().Now() }
	}
	if s.run == nil {
		s.run = d.cloudCheckRun
	}
	if s.stat == nil {
		s.stat = os.Stat
	}
	if s.keyFile == nil {
		s.keyFile = func(c *agentconfig.CloudCheckConfig) string { return c.KeyFilePath() }
	}
	if s.dispatch == nil {
		s.dispatch = func(fn func()) { go fn() }
	}
	if s.escalate == nil {
		s.escalate = func(key, message string) error { return d.escalateAlertErr(key, "cloud_check", message) }
	}
	if s.clear == nil {
		s.clear = func(key string) error { return d.clearAlertsErr("cloud check recovered", key) }
	}
	return s
}

// cloudCheckRun runs one check through the daemon's exec seam, so a unit test
// never starts the program. The child inherits the daemon's environment and
// gets the key path as its one extra entry: the key's value never leaves the
// mounted file. A failure carries the program's exit status, never its output:
// the output is the check's report, which the status file already holds.
func (d *Daemon) cloudCheckRun(ctx context.Context, name string, args, env []string) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: the program is the operator's configured command
	cmd.Env = append(os.Environ(), env...)
	_, _, err := d.runCmd(cmd)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	return nil
}

// cloudCheckSettings returns the patrol's config block, or nil when the town
// declares none.
func cloudCheckSettings(cfg *DaemonPatrolConfig) *agentconfig.CloudCheckConfig {
	if cfg == nil || cfg.Patrols == nil {
		return nil
	}
	return cfg.Patrols.CloudCheck
}

// cloudCheckInterval returns how often the patrol runs, or the refusal for a
// setting it cannot schedule. An empty setting is the default. A value that is
// not a positive Go duration is refused, never read as the default: decoding
// refuses one from the config file, and this is the same refusal for a config
// built in code.
func cloudCheckInterval(cfg *DaemonPatrolConfig) (time.Duration, error) {
	c := cloudCheckSettings(cfg)
	if c == nil || c.Interval == "" {
		return cloudCheckDefaultInterval, nil
	}
	every, err := time.ParseDuration(c.Interval)
	if err != nil || every <= 0 {
		return 0, fmt.Errorf("patrols.cloud_check.interval = %q: want a positive Go duration such as %q",
			c.Interval, agentconfig.CloudCheckDefaultInterval)
	}
	return every, nil
}

// cloudCheckCommand returns the program the patrol runs, or the default when the
// town names none.
func cloudCheckCommand(cfg *DaemonPatrolConfig) string {
	if c := cloudCheckSettings(cfg); c != nil && c.Command != "" {
		return c.Command
	}
	return agentconfig.CloudCheckDefaultCommand
}

// cloudCheckStatusPath is the status file for a town: the path the dashboard
// reads.
func cloudCheckStatusPath(townRoot string) string {
	return filepath.Join(townRoot, filepath.FromSlash(cloudCheckStatusRelPath))
}

// startCloudCheckTicker starts the patrol's check ticker, or returns nil for a
// setting it cannot schedule. The ticker only asks whether a check is due, so
// its cadence is a fraction of the run interval. A refused setting starts no
// ticker and says why once, here.
func (d *Daemon) startCloudCheckTicker() *time.Ticker {
	every, err := cloudCheckInterval(d.patrolConfig)
	if err != nil {
		d.logger.Printf("Cloud check patrol not started: %v", err)
		return nil
	}
	tick := shortPatrolCheckTick(every)
	d.logger.Printf("Cloud check ticker started (check every %v, run interval %v)", tick, every)
	return time.NewTicker(tick)
}

// runCloudCheckPatrol is the tick: it runs the check once per interval, off the
// loop goroutine, and does nothing while the previous check is still running.
func (d *Daemon) runCloudCheckPatrol() {
	if !d.isPatrolActive("cloud_check") {
		return
	}
	s := d.cloudCheckSeamsOf()
	now := s.now()

	every, err := cloudCheckInterval(d.patrolConfig)
	if err != nil {
		// A setting the patrol cannot schedule runs nothing: checking on the
		// default would use a cadence the town never asked for.
		d.logger.Printf("cloud_check: not running: %v", err)
		return
	}

	// Due-ness is measured from the end of the last check, so a run that takes
	// most of an interval does not run back to back.
	if last := d.cloudCheckLastRunAt(); !last.IsZero() && now.Sub(last) < every {
		return
	}
	if !d.cloudCheckRunning.CompareAndSwap(false, true) {
		return // a check is still running
	}
	s.dispatch(func() {
		defer d.cloudCheckRunning.Store(false)
		d.cloudCheckPass(s, now)
	})
}

// cloudCheckPass runs one check and logs its outcome: after a run, one line with
// the exit status and the duration. The completion time is recorded whether the
// check succeeded or not, so a failing run is retried at the next interval
// rather than on every check tick.
func (d *Daemon) cloudCheckPass(s cloudCheckSeams, started time.Time) {
	keyFile := s.keyFile(cloudCheckSettings(d.patrolConfig))
	// The check cannot reach the cloud without the key, and a run without it
	// would report every project as unreachable. Say so once and run nothing.
	_, err := s.stat(keyFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		d.cloudCheckUnconfigured(fmt.Sprintf("no key file at %s; not running (mount the daemon-gcp-key Secret, or set patrols.cloud_check.key_file)", keyFile))
		return
	case err != nil:
		d.cloudCheckUnconfigured(fmt.Sprintf("cannot read the key file %s: %v; not running", keyFile, err))
		return
	}

	ctx, cancel := d.cloudCheckContext()
	defer cancel()

	statusPath := cloudCheckStatusPath(d.config.TownRoot)
	command := cloudCheckCommand(d.patrolConfig)
	err = s.run(ctx, command, []string{cloudCheckQuietArg, cloudCheckJSONArg, statusPath}, []string{cloudCheckKeyEnv + "=" + keyFile})
	elapsed := s.now().Sub(started).Round(time.Millisecond)
	d.cloudCheckLastRun.Store(s.now().UnixNano())

	status := runExitStatus(err)
	if err != nil {
		// The message names the command, its exit status and the status file.
		// It never carries the child's output, which is the check's report and
		// can name an app URL that carries a credential.
		if raiseErr := s.escalate(cloudCheckAlertKey,
			fmt.Sprintf("cloud_check: %s %s in %v; the status is in %s", command, status, elapsed, statusPath)); raiseErr == nil {
			d.cloudCheckAlertRaised.Store(true)
		}
		d.logger.Printf("cloud_check: %s: %s in %v", command, status, elapsed)
		return
	}
	d.logger.Printf("cloud_check: %s: %s in %v", command, status, elapsed)
	if d.cloudCheckAlertRaised.CompareAndSwap(true, false) {
		if clearErr := s.clear(cloudCheckAlertKey); clearErr != nil {
			// The alert is still open; keep the marker so the next good run
			// clears it.
			d.cloudCheckAlertRaised.Store(true)
		}
	}
}

// cloudCheckUnconfigured logs the unconfigured line once per daemon run, so a
// town that never mounts the key gets one line rather than one per interval.
func (d *Daemon) cloudCheckUnconfigured(reason string) {
	d.cloudCheckNoKeyOnce.Do(func() {
		d.logger.Printf("cloud_check: %s", reason)
	})
}

// cloudCheckContext bounds one check and stops it when the daemon shuts down.
// The caller must cancel it: that is what releases the timer.
func (d *Daemon) cloudCheckContext() (context.Context, context.CancelFunc) {
	ctx := d.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, cloudCheckRunTimeout)
}

// cloudCheckLastRunAt is when the patrol last finished a check; the zero time
// when it has none, which reads as due.
func (d *Daemon) cloudCheckLastRunAt() time.Time {
	if at := d.cloudCheckLastRun.Load(); at != 0 {
		return time.Unix(0, at)
	}
	return time.Time{}
}
