package daemon

// hm_sync is the town's periodic `hm sync`: history-man's index was built only
// when someone ran the command by hand, so a town with the patrol on gets it on
// an interval instead (gt-z6qre.15).
//
// The patrol is opt-in. It runs only when daemon.patrols.hm_sync.enabled is
// true, and a sync that fails is logged and does not stop it: the next interval
// tries again, because a stale index and a silent patrol are the same outage.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	agentconfig "github.com/steveyegge/gastown/internal/config"
)

const (
	// hmSyncRunTimeout bounds one sync. The command indexes a repository, so a
	// sync still running after ten minutes is stuck rather than slow.
	hmSyncRunTimeout = 10 * time.Minute

	// hmSyncDefaultInterval is the run interval when the town names none.
	// config.HMSyncDefaultInterval is the same default as the config file
	// spells it, and TestHMSyncDefaultsItsIntervalAndCommand holds the two
	// together.
	hmSyncDefaultInterval = 30 * time.Minute
)

// hmSyncSeams are the patrol's clock and side effect, replaceable in tests
// through Daemon.hmSync. Each nil field is the production behavior (see
// hmSyncSeamsOf).
type hmSyncSeams struct {
	// now is the clock that decides when the interval has passed.
	now func() time.Time
	// run runs the configured program; nil is hmSyncRun.
	run func(ctx context.Context, name string, args ...string) ([]byte, error)
	// dispatch runs a sync off the loop goroutine, so a slow index does not
	// hold the heartbeat. Nil means one goroutine per sync.
	dispatch func(fn func())
}

// hmSyncSeamsOf returns d's patrol seams with every unset one filled in with
// its production behavior.
func (d *Daemon) hmSyncSeamsOf() hmSyncSeams {
	s := d.hmSync
	if s.now == nil {
		s.now = func() time.Time { return d.clk().Now() }
	}
	if s.run == nil {
		s.run = d.hmSyncRun
	}
	if s.dispatch == nil {
		s.dispatch = func(fn func()) { go fn() }
	}
	return s
}

// hmSyncRun runs one hm command through the daemon's exec seam, so the unit
// tier never launches the program. A failure carries the program's exit status,
// never its output: hm prints the repository it indexed, which an error line
// has no use for.
func (d *Daemon) hmSyncRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: the program is the operator's configured command
	stdout, _, err := d.runCmd(cmd)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return stdout, nil
}

// hmSyncSettings returns the patrol's config block, or nil when the town
// declares none.
func hmSyncSettings(cfg *DaemonPatrolConfig) *HMSyncConfig {
	if cfg == nil || cfg.Patrols == nil {
		return nil
	}
	return cfg.Patrols.HMSync
}

// hmSyncInterval returns how often the patrol runs, or the refusal for a
// setting it cannot schedule. An empty setting is the default rather than no
// schedule: a town that enabled the patrol without repeating the default still
// gets one. A value that is not a positive Go duration is refused, never read
// as the default: decoding refuses one from the config file, and this is the
// same refusal for a config built in code, so a mistyped interval cannot put
// the patrol on a cadence the town never asked for.
func hmSyncInterval(cfg *DaemonPatrolConfig) (time.Duration, error) {
	c := hmSyncSettings(cfg)
	if c == nil || c.Interval == "" {
		return hmSyncDefaultInterval, nil
	}
	every, err := time.ParseDuration(c.Interval)
	if err != nil || every <= 0 {
		return 0, fmt.Errorf("patrols.hm_sync.interval = %q: want a positive Go duration such as %q",
			c.Interval, agentconfig.HMSyncDefaultInterval)
	}
	return every, nil
}

// hmSyncCommand returns the program the patrol runs, or the default when the
// town names none.
func hmSyncCommand(cfg *DaemonPatrolConfig) string {
	if c := hmSyncSettings(cfg); c != nil && c.Command != "" {
		return c.Command
	}
	return agentconfig.HMSyncDefaultCommand
}

// startHMSyncTicker starts the patrol's check ticker, or returns nil for a
// setting it cannot schedule. The ticker only asks whether a sync is due, so
// its cadence is a fraction of the run interval. A refused setting starts no
// ticker and says why once, here: an interval the town cannot have is a config
// error to read, not a patrol that quietly never runs.
func (d *Daemon) startHMSyncTicker() *time.Ticker {
	every, err := hmSyncInterval(d.patrolConfig)
	if err != nil {
		// runHMSync makes the same refusal on every tick, so a ticker that
		// outlived this one could not start a sync either.
		d.logger.Printf("HM sync patrol not started: %v", err)
		return nil
	}
	tick := shortPatrolCheckTick(every)
	d.logger.Printf("HM sync ticker started (check every %v, run interval %v)", tick, every)
	return time.NewTicker(tick)
}

// runHMSync is the tick: it runs the configured program once per interval, off
// the loop goroutine, and does nothing while the previous sync is still
// running.
func (d *Daemon) runHMSync() {
	if !d.isPatrolActive("hm_sync") {
		return
	}
	s := d.hmSyncSeamsOf()
	now := s.now()

	every, err := hmSyncInterval(d.patrolConfig)
	if err != nil {
		// A setting the patrol cannot schedule runs nothing: syncing on the
		// default would use a cadence the town never asked for.
		d.logger.Printf("hm_sync: not running: %v", err)
		return
	}

	// Due-ness is measured from the end of the last sync, so a sync that takes
	// most of an interval does not run back to back.
	if last := d.hmSyncLastRunAt(); !last.IsZero() && now.Sub(last) < every {
		return
	}
	if !d.hmSyncRunning.CompareAndSwap(false, true) {
		return // a sync is still running
	}
	s.dispatch(func() {
		defer d.hmSyncRunning.Store(false)
		d.hmSyncPass(s, now)
	})
}

// hmSyncPass runs one sync and logs its outcome: one line with the exit status
// and the duration, in every case. The completion time is recorded whether the
// sync succeeded or not, so a failing command is retried at the next interval
// rather than on every check tick.
func (d *Daemon) hmSyncPass(s hmSyncSeams, started time.Time) {
	ctx, cancel := d.hmSyncContext()
	defer cancel()

	command := hmSyncCommand(d.patrolConfig)
	_, err := s.run(ctx, command, "sync")
	elapsed := s.now().Sub(started).Round(time.Millisecond)
	d.hmSyncLastRun.Store(s.now().UnixNano())

	status := hmSyncStatus(err)
	if err != nil {
		d.logger.Printf("hm_sync: %s sync: %s in %v: %v", command, status, elapsed, err)
		return
	}
	d.logger.Printf("hm_sync: %s sync: %s in %v", command, status, elapsed)
}

// hmSyncContext bounds one sync and stops it when the daemon shuts down. The
// caller must cancel it: that is what releases the timer.
func (d *Daemon) hmSyncContext() (context.Context, context.CancelFunc) {
	ctx := d.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, hmSyncRunTimeout)
}

// hmSyncStatus names the exit status of a sync, which is what the summary line
// reports alongside the duration. ExitCode reports -1 for a command that never
// exited on its own — one the run timeout killed, or one a shutdown stopped —
// and that is a killed sync, not a status of -1.
func hmSyncStatus(err error) string {
	if err == nil {
		return "exit 0"
	}
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) {
		if code := exit.ExitCode(); code >= 0 {
			return fmt.Sprintf("exit %d", code)
		}
		return "killed"
	}
	return "no exit status"
}

// hmSyncLastRunAt is when the patrol last finished a sync; the zero time when
// it has none, which reads as due.
func (d *Daemon) hmSyncLastRunAt() time.Time {
	if at := d.hmSyncLastRun.Load(); at != 0 {
		return time.Unix(0, at)
	}
	return time.Time{}
}
