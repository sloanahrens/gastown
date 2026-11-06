package daemon

// The jev_scorecard patrol (gt-jk0162): the scorecard is a periodic review,
// not a live panel (gt-hh07o2), so the daemon keeps its cache fresh on a
// schedule. Once a day at patrols.jev_scorecard.hour it runs
// `gt jev scorecard --refresh`, which scores every registered battery and
// writes <town>/.runtime/jev/scorecard.json; at start it runs once when the
// cache is older than patrols.jev_scorecard.interval, and a cache within that
// interval is left alone until the next hour.
//
// The refresh scores every registered battery, so the patrol runs only when
// patrols.jev_scorecard.batteries names at least one. A name the binary does
// not register is refused by gt config validate and gt jev scorecard
// (ValidateJevScorecardBatteries) and by the patrol's own settings read,
// naming it: a battery dropped from the binary after the 2026-11-20 review
// would otherwise cost nothing and say nothing.
//
// A refresh runs on its own goroutine, one at a time; a tick that finds one in
// flight is skipped with one line an hour. A refresh that exits non-zero is
// one line in the daemon log and raises no attention item: the cache keeps
// every battery's last good block (gt-hh07o2), and the next scheduled run
// still happens.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/steveyegge/gastown/internal/jev/scorecard"
)

const (
	// jevScorecardRefreshBudget bounds one refresh: the scoring of every
	// battery over the cache's window. A refresh still running after it is
	// stuck rather than slow.
	jevScorecardRefreshBudget = 30 * time.Minute

	// jevScorecardLogWindow is how long a line the patrol would otherwise
	// repeat every heartbeat is held back: the skip while a refresh is in
	// flight, and a settings refusal while the config stands.
	jevScorecardLogWindow = time.Hour
)

// jevScorecardSeams are the patrol's clock, zone, runner and dispatch,
// replaceable in tests through Daemon.jevScorecardSeams. A nil field is the
// production behavior (see jevScorecardSeamsOf).
type jevScorecardSeams struct {
	// now is the clock the schedule is judged against.
	now func() time.Time
	// loc is the zone the configured hour is read in.
	loc func() *time.Location
	// refresh runs one refresh of the town's scorecard cache. Nil means
	// runJevScorecardRefresh, which runs the command; a test's runner starts
	// no process.
	refresh func(ctx context.Context, townRoot string) error
	// dispatch runs a refresh off the heartbeat goroutine, so a long scoring
	// pass does not hold the tick. Nil means one goroutine per refresh.
	dispatch func(fn func())
}

// jevScorecardSeamsOf returns d's patrol seams with every unset one filled in
// with its production behavior.
func (d *Daemon) jevScorecardSeamsOf() jevScorecardSeams {
	s := d.jevScorecardSeams
	if s.now == nil {
		s.now = func() time.Time { return d.clk().Now() }
	}
	if s.loc == nil {
		s.loc = func() *time.Location { return time.Local }
	}
	if s.refresh == nil {
		s.refresh = d.runJevScorecardRefresh
	}
	if s.dispatch == nil {
		s.dispatch = func(fn func()) { go fn() }
	}
	return s
}

// jevScorecardSettings is the patrol's settings as one tick reads them: the
// local time of day a run is due, how old the cache may be at start, and the
// batteries the town declares.
type jevScorecardSettings struct {
	clockMinute int
	interval    time.Duration
	batteries   []string
}

// ValidateJevScorecardBatteries returns an error naming the first battery
// patrols.jev_scorecard.batteries lists that the binary does not register, or
// nil. gt config validate and gt jev scorecard call it for the block they
// own, and jevScorecardSettingsOf calls it so a config built in code is
// refused too.
func ValidateJevScorecardBatteries(cfg *DaemonPatrolConfig) error {
	var c *JevScorecardConfig
	if cfg != nil && cfg.Patrols != nil {
		c = cfg.Patrols.JevScorecard
	}
	known := scorecard.Registered()
	for _, name := range c.BatteriesValue() {
		if !slices.Contains(known, name) {
			return fmt.Errorf(
				"patrols.jev_scorecard.batteries: unknown battery %q; the binary registers %s",
				name, strings.Join(known, ", "))
		}
	}
	return nil
}

// jevScorecardSettingsOf reads the patrol's settings from the daemon patrol
// config, or the error for a setting it cannot act on.
func jevScorecardSettingsOf(cfg *DaemonPatrolConfig) (jevScorecardSettings, error) {
	var c *JevScorecardConfig
	if cfg != nil && cfg.Patrols != nil {
		c = cfg.Patrols.JevScorecard
	}
	clockMinute, err := c.ClockMinute()
	if err != nil {
		return jevScorecardSettings{}, err
	}
	interval, err := c.IntervalDuration()
	if err != nil {
		return jevScorecardSettings{}, err
	}
	if err := ValidateJevScorecardBatteries(cfg); err != nil {
		return jevScorecardSettings{}, err
	}
	return jevScorecardSettings{clockMinute: clockMinute, interval: interval, batteries: c.BatteriesValue()}, nil
}

// jevScorecardSchedule is the clock a due decision is made against: the local
// time of day a run is due, and the zone it is read in.
type jevScorecardSchedule struct {
	clockMinute int
	loc         *time.Location
}

// nextRunAfter returns the first instant of the configured time of day that is
// strictly after t, in the schedule's zone.
func (s jevScorecardSchedule) nextRunAfter(t time.Time) time.Time {
	l := t.In(s.loc)
	at := time.Date(l.Year(), l.Month(), l.Day(), s.clockMinute/60, s.clockMinute%60, 0, 0, s.loc)
	if !at.After(l) {
		at = at.AddDate(0, 0, 1)
	}
	return at
}

// jevScorecardDueAt reports whether a refresh is due at now. lastAttempt is
// when the last refresh attempt completed (zero when the town has none),
// startedAt is when this process first evaluated the patrol, and cachedAt is
// when the cache was scored (zero when it is missing or unreadable).
//
// With no attempt recorded — a town that has never run the patrol — only a
// cache older than the interval is worth a run, so a daemon that starts after
// the hour does not refresh a cache the interval still covers; the daily clock
// is then counted from startedAt. With a record, a run is due at the first
// clock instant after the later of the last attempt and this start, so an hour
// that passed while the daemon was down does not run it at startup, and a
// cache older than the interval since the last attempt is a catch-up.
func jevScorecardDueAt(now, lastAttempt, startedAt, cachedAt time.Time, s jevScorecardSchedule, interval time.Duration) bool {
	cacheStale := cachedAt.IsZero() || now.Sub(cachedAt) >= interval
	if lastAttempt.IsZero() {
		if cacheStale {
			return true
		}
		return !now.Before(s.nextRunAfter(startedAt))
	}
	if cacheStale && now.Sub(lastAttempt) >= interval {
		return true
	}
	baseline := lastAttempt
	if startedAt.After(baseline) {
		baseline = startedAt
	}
	return !now.Before(s.nextRunAfter(baseline))
}

// jevScorecardCachedAt returns when the town's scorecard cache was scored, or
// the zero time when the cache is missing or cannot be read: an age nobody
// knows is not a fresh one, so the start rule still runs.
func jevScorecardCachedAt(townRoot string) time.Time {
	doc, err := (scorecard.Cache{}).Read(scorecard.CachePath(townRoot))
	if err != nil {
		return time.Time{}
	}
	return doc.ScoredAt
}

// jevScorecardStarted returns the instant this process first evaluated the
// patrol, recording now on the first call.
func (d *Daemon) jevScorecardStarted(now time.Time) time.Time {
	d.jevScorecardStartMu.Lock()
	defer d.jevScorecardStartMu.Unlock()
	if d.jevScorecardStartedAt.IsZero() {
		d.jevScorecardStartedAt = now.Round(0)
	}
	return d.jevScorecardStartedAt
}

// triggerJevScorecard starts one refresh on its own goroutine when the patrol
// is on and due. A refresh scores every registered battery, so it never runs
// on the heartbeat. The single-flight guard keeps one refresh in flight; a
// tick that finds one running is skipped, and the line saying so is repeated
// once an hour rather than once a heartbeat.
func (d *Daemon) triggerJevScorecard() {
	if !d.isPatrolActive("jev_scorecard") {
		return
	}
	s := d.jevScorecardSeamsOf()
	now := s.now()
	settings, err := jevScorecardSettingsOf(d.patrolConfig)
	if err != nil {
		if d.jevScorecardLogs.allow("refused:"+err.Error(), now) {
			d.logger.Printf("jev_scorecard: not refreshing: %v", err)
		}
		return
	}
	if len(settings.batteries) == 0 {
		return
	}
	lastAttempt, _, warn, loadErr := latestPatrolLastRun(d.config.TownRoot, "jev_scorecard", d.jevScorecardRun.last())
	if loadErr != nil {
		if d.jevScorecardLogs.allow("last-run:"+loadErr.Error(), now) {
			d.logger.Printf("jev_scorecard: WARNING: last-run state unreadable (%v); deciding from the cache alone", loadErr)
		}
	} else if warn != "" && d.jevScorecardLogs.allow("last-run-warning", now) {
		d.logger.Printf("jev_scorecard: WARNING: %s", warn)
	}
	schedule := jevScorecardSchedule{clockMinute: settings.clockMinute, loc: s.loc()}
	if !jevScorecardDueAt(now, lastAttempt, d.jevScorecardStarted(now), jevScorecardCachedAt(d.config.TownRoot), schedule, settings.interval) {
		return
	}
	if !d.jevScorecardRunning.CompareAndSwap(false, true) {
		if d.jevScorecardLogs.allow("refresh-running", now) {
			d.logger.Printf("jev_scorecard: a refresh is still running; skipping this tick")
		}
		return
	}
	s.dispatch(func() {
		defer d.jevScorecardRunning.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), jevScorecardRefreshBudget)
		defer cancel()
		townRoot := d.config.TownRoot
		err := s.refresh(ctx, townRoot)
		concluded := s.now()
		switch {
		case err == nil:
			d.logger.Printf("jev_scorecard: refreshed the scorecard cache")
		case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
			d.logger.Printf("jev_scorecard: refresh timed out after %s: %v; the next scheduled run still happens", jevScorecardRefreshBudget, err)
		default:
			// A corrupt cache stops every refresh, so when the cache cannot be
			// read as a scorecard this line carries the path and the remedy an
			// operator needs; any other failure is named alone.
			line := fmt.Sprintf("jev_scorecard: refresh failed: %v", err)
			path := scorecard.CachePath(townRoot)
			if _, readErr := (scorecard.Cache{}).Read(path); errors.Is(readErr, scorecard.ErrCorruptScorecard) {
				line += fmt.Sprintf("; the cache %s is corrupt: delete it and rerun gt jev scorecard --refresh", path)
			}
			d.logger.Print(line)
		}
		// The attempt is spent whether it succeeded or not: recording it here
		// keeps a failing refresh to one line and one try per scheduled run
		// instead of one on every heartbeat.
		d.recordPatrolRun(&d.jevScorecardRun, "jev_scorecard", concluded)
	})
}

// runJevScorecardRefresh runs `gt jev scorecard --refresh` for the town, which
// scores every registered battery and writes the cache atomically
// (gt-hh07o2). A failure keeps the command's exit status and none of its
// output: the command names the batteries that failed on its streams, and a
// log line carries no captured output.
func (d *Daemon) runJevScorecardRefresh(ctx context.Context, townRoot string) error {
	if d.gtPath == "" {
		return errors.New("no gt binary resolved")
	}
	cmd := exec.CommandContext(ctx, d.gtPath, "jev", "scorecard", "--refresh") //nolint:gosec // G204: a fixed subcommand of the binary resolved at daemon init
	cmd.Dir = townRoot
	cmd.Env = daemonGTEnv(os.Environ())
	if _, _, err := d.runCmd(cmd); err != nil {
		return fmt.Errorf("gt jev scorecard --refresh: %w", err)
	}
	return nil
}

// jevScorecardThrottle holds the last time each line was said, so a line that
// would repeat every heartbeat is said once per window. Its zero value is
// ready to use, and the mutex keeps a caller that reads it from another
// goroutine safe.
type jevScorecardThrottle struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// allow reports whether the line key may be said at now, and remembers it.
// Entries past jevScorecardLogWindow are dropped, so the map holds only the
// lines it still suppresses.
func (t *jevScorecardThrottle) allow(key string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last == nil {
		t.last = make(map[string]time.Time)
	}
	if at, ok := t.last[key]; ok && now.Sub(at) < jevScorecardLogWindow {
		return false
	}
	for k, at := range t.last {
		if now.Sub(at) >= jevScorecardLogWindow {
			delete(t.last, k)
		}
	}
	t.last[key] = now.Round(0)
	return true
}
