package daemon

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/testdb"
)

const (
	defaultCompactorDogInterval = 24 * time.Hour
	// DefaultCompactorCommitThreshold is the commit count at which this patrol
	// advises. The daemon monitors only — it never rewrites history, and a
	// count is not a reason to: the line is a report line, not a compaction
	// trigger.
	//
	// Commit count is not the disk cost, which is why the size line below is a
	// separate alarm: this one keeps history-query latency visible and the town
	// sets it near 20000 (measurements:
	// docs/plans/2026-09-25-dolt-gc-maintenance-design.md, Problem).
	// scheduled_maintenance's gc is what actually reclaims disk.
	// Configurable via the "daemon" section of settings/config.json
	// (patrols.compactor_dog.threshold).
	DefaultCompactorCommitThreshold = 2000
	// compactorSizeThreshold is the on-disk size at which this patrol
	// escalates: 1 GiB, ahead of the 2 GB line past which flattening is the
	// offline operator procedure (docs/dolt-history-offline.md). Half the
	// trigger's value leaves a maintenance window of headroom at this town's
	// write rate, so the alert arrives while the cheap options are open.
	//
	// Escalation only: it decides when an operator is told, never what is
	// rewritten. A database can sit above it.
	compactorSizeThreshold = int64(1) << 30
	// compactorQueryTimeout is the timeout for individual SQL queries.
	compactorQueryTimeout = 30 * time.Second
)

// compactorDogInterval returns the configured interval, or the default (24h).
func compactorDogInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.CompactorDog != nil {
		if config.Patrols.CompactorDog.IntervalStr != "" {
			if d, err := time.ParseDuration(config.Patrols.CompactorDog.IntervalStr); err == nil && d > 0 {
				return d
			}
		}
	}
	return defaultCompactorDogInterval
}

// compactorDogTickInterval is how often the patrol loop asks whether
// compactor_dog is due — a check cadence, not a run cadence. The run interval
// (compactorDogInterval, 24h by default) is enforced against the last-run time
// in daemon/patrol_last_run.json, so a restart cannot postpone or starve a run
// (gt-ima2).
const compactorDogTickInterval = 15 * time.Minute

// compactorDogCycle runs one monitoring cycle and reports whether it
// completed cleanly: compactorDogCycleFn's answer when a test set one, else
// the real cycle. A cycle opens a SQL connection to every production database,
// so a test of the schedule must not need Dolt.
// Recording the last-run time stays outside the seam — the record is what
// the schedule is built on, and it must only happen for a cycle this return
// value calls clean (gt-4uxs): a cycle that found no databases or failed
// every inspection is not a completed run, and recording it as one would
// hide the failure behind a 24h wait before the next attempt.
func (d *Daemon) compactorDogCycle() bool {
	if d.compactorDogCycleFn != nil {
		return d.compactorDogCycleFn()
	}
	return d.runCompactorDog()
}

// compactorDogDecision is one due-ness evaluation.
type compactorDogDecision struct {
	// due is whether the patrol should run now.
	due bool
	// note explains the decision for the log. Never empty.
	note string
	// warn is set when the decision came from a broken last-run record rather
	// than from a comparison. A broken record runs the patrol *and* says so
	// (gt-ima2): silent skipping is the failure being fixed, and a run without
	// a word would hide a corrupt state file behind a patrol that looks merely
	// on schedule.
	warn string
}

// compactorDogDue decides whether the patrol should run, given the last-run
// state on disk. Delegates to the shared evaluatePatrolDue (gt-gxpwc), which
// now backs every ticker-driven patrol in this package.
func (d *Daemon) compactorDogDue(now time.Time, interval time.Duration) compactorDogDecision {
	dec := evaluatePatrolDue(d.config.TownRoot, "compactor_dog", d.lastCompactorDogRun, now, interval)
	return compactorDogDecision{due: dec.due, note: dec.note, warn: dec.warn}
}

// triggerCompactorDog runs a monitoring cycle when the patrol is due, on its
// own goroutine.
//
// Dispatched rather than run inline because a cycle queries every production
// database with a 30s timeout each; running that inside the select loop would
// stall the heartbeat and every tick behind it (gt-uvxy).
func (d *Daemon) triggerCompactorDog() {
	if !d.isPatrolActive("compactor_dog") {
		return
	}

	d.compactorDogMu.Lock()
	defer d.compactorDogMu.Unlock()

	if d.compactorDogRunning {
		d.logger.Printf("compactor_dog: previous cycle still running — skipping this check")
		return
	}

	dec := d.compactorDogDue(d.clk().Now(), compactorDogInterval(d.patrolConfig))

	// The first evaluation of each process is logged either way: "the patrol is
	// alive and not due" is what an operator needs after a restart, and it is
	// also the only evidence that a 24h gap is a schedule rather than a stall.
	firstCheck := !d.compactorDogChecked
	d.compactorDogChecked = true

	if !dec.due {
		if firstCheck {
			d.logger.Printf("compactor_dog: not due — %s", dec.note)
		}
		return
	}
	if dec.warn != "" {
		d.logger.Printf("compactor_dog: WARNING: %s — %s", dec.warn, dec.note)
	} else {
		d.logger.Printf("compactor_dog: due — %s", dec.note)
	}

	d.compactorDogRunning = true
	d.compactorDogCycles.Add(1)
	go func() {
		defer d.compactorDogCycles.Done()
		defer func() {
			d.compactorDogMu.Lock()
			d.compactorDogRunning = false
			d.compactorDogMu.Unlock()
		}()
		// The cycle's SQL phase shares the read side of doltMaintMu; a gc in
		// flight skips the cycle without recording it, so the next 15-minute
		// check runs it.
		release, ok := d.tryDoltTask("compactor_dog")
		if !ok {
			return
		}
		defer release()
		// The cycle opens a SQL connection to every production database, and
		// Run()'s startup catch-up reaches here before the first heartbeat — the
		// step that otherwise starts Dolt — so the bring-up belongs on this
		// goroutine, where it cannot stall the tick loop behind a restart and
		// its backoff (gt-ox6c).
		if err := d.ensureDoltServerUp(); err != nil {
			if errors.Is(err, errDoltBringUpSkipped) {
				return
			}
			d.logger.Printf("compactor_dog: Dolt server unavailable: %v", err)
		}
		if d.compactorDogCycle() {
			d.recordCompactorDogRun()
		} else {
			d.logger.Printf("compactor_dog: cycle failed — not recording last-run, next check will retry")
		}
	}()
}

// recordCompactorDogRun records the completion time of the cycle that just
// finished, in memory and on disk.
//
// The in-memory time is set even when the write fails: an unwritable daemon
// directory would otherwise leave the file stale, so every 15-minute check
// would see a due patrol and run a full cycle, opening a SQL connection to
// every production database each time. A restart then
// reads the missing record as "run the check", which is the safe direction for
// a monitor.
func (d *Daemon) recordCompactorDogRun() {
	at := d.clk().Now()

	d.compactorDogMu.Lock()
	d.lastCompactorDogRun = at
	d.compactorDogMu.Unlock()

	if err := savePatrolLastRun(d.config.TownRoot, "compactor_dog", at); err != nil {
		d.logger.Printf("compactor_dog: WARNING: cannot persist last-run time (%v) — "+
			"the next daemon start will run the check again", err)
	}
}

// compactorDogConfig returns the compactor_dog patrol config block, or nil.
func compactorDogConfig(config *DaemonPatrolConfig) *CompactorDogConfig {
	if config == nil || config.Patrols == nil {
		return nil
	}
	return config.Patrols.CompactorDog
}

// compactorDogThreshold returns the configured commit threshold, or
// DefaultCompactorCommitThreshold (2000). This is the commit count at which the
// patrol advises; it triggers neither compaction nor an escalation, because
// the daemon patrol never compacts.
func compactorDogThreshold(config *DaemonPatrolConfig) int {
	if cd := compactorDogConfig(config); cd != nil {
		if cd.Threshold > 0 {
			return cd.Threshold
		}
	}
	return DefaultCompactorCommitThreshold
}

// compactorCondition names one measured property of one database. The pair
// (database, condition) is an alarm's stable identity.
type compactorCondition string

const (
	compactorCommits compactorCondition = "commits"
	compactorSize    compactorCondition = "size"
)

// Withdrawal reasons. `gt escalate clear` records the reason on the bead it
// closes, and a count that is over its line reads differently from a
// measurement that came back under one.
const (
	// compactorUnderReason closes an alarm whose measurement is back below
	// its line.
	compactorUnderReason = "compactor_dog: back below threshold"
	// compactorAdvisoryReason closes a commit-count escalation the patrol no
	// longer raises: the count is an advisory, and this is how the escalation
	// an older build opened is retired (gt-3mu6q).
	compactorAdvisoryReason = "commit count is advisory"
)

// compactorAdvisory is one database's commit count over its line. The count is
// reported — logged, and recorded for the status line — because it is not a
// reason to rewrite history: flatten is an offline operator procedure with a
// single trigger, a database past 2 GB (docs/dolt-history-offline.md).
type compactorAdvisory struct {
	db        string
	commits   int
	threshold int
}

// note renders the advisory as the cycle logs it.
func (a compactorAdvisory) note() string {
	return fmt.Sprintf("%s has %d commits, over the %d line — advisory only, no escalation; "+
		"nothing rewrites history routinely and flatten is an offline operator procedure "+
		"past 2 GB (docs/dolt-history-offline.md)", a.db, a.commits, a.threshold)
}

// compactorAlertKey is one database-condition's alert key: what `gt escalate`
// records a repeat of the condition onto, and what `gt escalate clear` closes.
// It is per database and per condition so that one database back under its
// size line withdraws its size alarm without touching its commit alarm, or
// anyone else's (gt-vwry).
func compactorAlertKey(db string, condition compactorCondition) string {
	return fmt.Sprintf("compactor_dog:%s:%s", db, condition)
}

// compactorAlarm is one alarm a cycle raises or withdraws. message is set on a
// raise; a withdrawal carries its own reason instead, because a commit count
// that has become advisory and a size line back under its threshold are
// different news about the same database.
type compactorAlarm struct {
	key       string
	db        string
	condition compactorCondition
	message   string
	// reason is the withdrawal's reason, set on an alarm to clear.
	reason string
}

// compactorProbes are the measurements behind one monitoring cycle: which
// databases exist, each one's commit count, and its size on disk. They are
// functions so every branch is reachable without a Dolt server or a real data
// directory (doctorProbes' shape).
//
// size is nil when sizes are not readable from this host at all — the server's
// data directory is not here. The cycle then checks commits alone, which it
// can, over SQL.
type compactorProbes struct {
	// databases lists the databases to check.
	databases func() ([]string, error)
	// commits counts one database's dolt_log commits.
	commits func(db string) (int, error)
	// size measures one database's on-disk store, Dolt's journal excluded.
	size func(db string) (int64, error)
}

// compactorReport is everything one monitoring cycle measured and decided.
type compactorReport struct {
	// names are the databases checked, in discovery order.
	names []string
	// readings is one line per database: what was measured against what line.
	readings []string
	// unread is one line per measurement that could not be taken. An
	// unreadable database is reported, never skipped in silence: a monitor
	// whose silence could mean either "fine" or "could not look" is the
	// failure this patrol exists to prevent.
	unread []string
	// advisories is one entry per database whose commit count is over its
	// line: reported, never escalated (gt-3mu6q).
	advisories []compactorAdvisory
	// raised and cleared are the alarms to file and to withdraw.
	raised  []compactorAlarm
	cleared []compactorAlarm
	// discoveryErr is set when the database list itself could not be read.
	discoveryErr error
	// sizeUnchecked is true when no size was read at all — no data directory
	// on this host. The cycle then has no size alarms at all, which is not the
	// same report as every database being small.
	sizeUnchecked bool
}

// compactorDogFindings inspects every database against both lines and decides
// each alarm: a size over its line to raise, a commit count over its line to
// advise on, and a measurement back under a line to withdraw. Every
// measurement comes from the probes and the thresholds are parameters, so a
// cycle over any number of databases is testable with temp directories and
// fake counters.
//
// It only decides. Filing an escalation is a `gt` subprocess that can take a
// minute under load, so the caller does that.
func compactorDogFindings(p compactorProbes, commitThreshold int, sizeThreshold int64) compactorReport {
	var r compactorReport
	if p.size == nil {
		r.sizeUnchecked = true
	}

	databases, err := p.databases()
	if err != nil {
		r.discoveryErr = err
		return r
	}

	// A measurement under its line withdraws that alarm. A withdraw for a
	// condition that was never raised is a no-op — `gt escalate clear` treats
	// an unknown key as success — so a database that merely stopped tripping a
	// line this cycle is not remembered here.
	for _, db := range databases {
		r.names = append(r.names, db)

		reading := db + ":"

		commits, err := p.commits(db)
		switch {
		case err != nil:
			r.unread = append(r.unread, fmt.Sprintf("%s: counting commits: %v", db, err))
			reading += " commits unread"
		case commits >= commitThreshold:
			// Advisory, not alarm: the count is not a reason to rewrite
			// history, so an escalation for it would page an operator with no
			// action behind it. The withdrawal is issued anyway — it is what
			// closes an escalation an older build opened for this key
			// (gt-3mu6q).
			r.advisories = append(r.advisories, compactorAdvisory{db: db, commits: commits, threshold: commitThreshold})
			r.cleared = append(r.cleared, compactorAlarm{
				key: compactorAlertKey(db, compactorCommits), db: db, condition: compactorCommits,
				reason: compactorAdvisoryReason,
			})
			reading += fmt.Sprintf(" %d commits OVER line %d (advisory)", commits, commitThreshold)
		default:
			r.cleared = append(r.cleared, compactorAlarm{
				key: compactorAlertKey(db, compactorCommits), db: db, condition: compactorCommits,
				reason: compactorUnderReason,
			})
			reading += fmt.Sprintf(" %d commits ok (line %d)", commits, commitThreshold)
		}

		if p.size != nil {
			size, err := p.size(db)
			switch {
			case err != nil:
				r.unread = append(r.unread, fmt.Sprintf("%s: measuring size: %v", db, err))
				reading += "; size unread"
			case size >= sizeThreshold:
				r.raised = append(r.raised, compactorAlarm{
					key: compactorAlertKey(db, compactorSize), db: db, condition: compactorSize,
					message: fmt.Sprintf("Size threshold exceeded for %s: %s on disk (threshold %s). "+
						"Flattening is the offline operator procedure once a database passes 2 GB "+
						"(docs/dolt-history-offline.md); this is the alert with a gigabyte of room "+
						"before it. Nothing rewrites history routinely.",
						db, formatBytes(size), formatBytes(sizeThreshold)),
				})
				reading += fmt.Sprintf("; %s OVER line %s", formatBytes(size), formatBytes(sizeThreshold))
			default:
				r.cleared = append(r.cleared, compactorAlarm{
					key: compactorAlertKey(db, compactorSize), db: db, condition: compactorSize,
					reason: compactorUnderReason,
				})
				reading += fmt.Sprintf("; %s ok (line %s)", formatBytes(size), formatBytes(sizeThreshold))
			}
		}

		r.readings = append(r.readings, reading)
	}
	return r
}

// runCompactorDog checks every production database's commit count and on-disk
// size, escalates for each database at or above the size line, and advises on
// each one at or above the commit line. It never rewrites history.
//
// Rewriting history is not a routine path at all: it is an offline operator
// procedure (docs/dolt-history-offline.md) with a single trigger, a database
// past 2 GB. The size alarm is the advance warning of that line; the commit
// count is not a second trigger, only a reading (gt-3mu6q).
//
// Returns whether the cycle completed cleanly — false when there was nothing
// to inspect or something could not be inspected, so the caller knows not to
// record this as a completed run (gt-4uxs).
func (d *Daemon) runCompactorDog() bool {
	return d.runCompactorDogWith(d.compactorDogProbes())
}

// runCompactorDogWith runs one monitoring cycle over the measurements in p.
// Split out from runCompactorDog so a test drives the whole cycle, alarms
// included, from probes it controls — offline, with no Dolt server.
func (d *Daemon) runCompactorDogWith(p compactorProbes) bool {
	if !d.isPatrolActive("compactor_dog") {
		return true
	}

	commitThreshold := compactorDogThreshold(d.patrolConfig)
	d.logger.Printf("compactor_dog: starting monitoring cycle (commit_threshold=%d size_threshold=%s)",
		commitThreshold, formatBytes(compactorSizeThreshold))

	warnDeprecatedCompactorConfig(d, compactorDogConfig(d.patrolConfig))

	cycle := d.startDogCycle("compactor_dog")
	defer cycle.close()

	rep := compactorDogFindings(p, commitThreshold, compactorSizeThreshold)

	// Step "inspect": read every database. A cycle that could not look at
	// something is not a clean cycle — it must not be recorded as a completed
	// run, or the failure waits out the full interval instead of retrying on
	// the next 15-minute check (gt-4uxs).
	switch {
	case rep.discoveryErr != nil:
		d.logger.Printf("compactor_dog: cannot discover databases: %v", rep.discoveryErr)
		cycle.failStep("inspect", fmt.Sprintf("database discovery: %v", rep.discoveryErr))
		return false
	case len(rep.names) == 0:
		d.logger.Printf("compactor_dog: no databases to check")
		cycle.failStep("inspect", "no databases found")
		return false
	}
	// The readings first, then what could not be read: the UNREAD lines are
	// the ones a reader is looking for, and a database missing from the
	// readings above is what sent them there.
	for _, line := range rep.readings {
		d.logger.Printf("compactor_dog: %s", line)
	}
	for _, line := range rep.unread {
		d.logger.Printf("compactor_dog: UNREAD %s", line)
	}
	if len(rep.unread) > 0 {
		cycle.failStep("inspect", fmt.Sprintf("%d measurement(s) unreadable", len(rep.unread)))
	} else {
		cycle.closeStep("inspect")
	}

	// The advisories are recorded for gt status to show on the Services line
	// (gt-3mu6q). A failed write leaves the previous reading in place, which
	// the line then reports as what it is — the last cycle's measurement.
	if err := saveCompactorAdvisories(d.config.TownRoot, d.clk().Now(), rep.advisories); err != nil {
		d.logger.Printf("compactor_dog: WARNING: cannot record commit advisories (%v) — "+
			"gt status will show the previous cycle's reading", err)
	}

	// Step "monitor": file, advise and withdraw. None is a failure — an alarm
	// over a line is this patrol working, and a withdrawal is it working
	// again.
	for _, a := range rep.advisories {
		d.logger.Printf("compactor_dog: ADVISORY %s", a.note())
	}
	for _, a := range rep.raised {
		d.logger.Printf("compactor_dog: %s: %s — ESCALATING", a.db, a.condition)
		d.escalateAlert(a.key, "compactor_dog", a.message)
	}
	d.clearWithdrawn(rep.cleared)
	cycle.closeStep("monitor")

	sizes := ""
	if rep.sizeUnchecked {
		// Says so on the cycle's own line: "no database is large" and "no size
		// was read" are different reports, and only one of them is news.
		sizes = " sizes=unchecked"
	}
	d.logger.Printf("compactor_dog: cycle complete — checked=%d raised=%d advisories=%d cleared=%d unread=%d%s",
		len(rep.names), len(rep.raised), len(rep.advisories), len(rep.cleared), len(rep.unread), sizes)
	cycle.closeStep("report")

	return len(rep.unread) == 0
}

// compactorAlarmKeys collects the alert keys of the alarms to withdraw.
func compactorAlarmKeys(alarms []compactorAlarm) []string {
	keys := make([]string, 0, len(alarms))
	for _, a := range alarms {
		keys = append(keys, a.key)
	}
	return keys
}

// clearWithdrawn withdraws the alarms a cycle decided to withdraw, one `gt
// escalate clear` per reason: the reason is recorded on the bead it closes, so
// a commit count that is now advisory must not be closed as if it had dropped
// below its line (gt-3mu6q).
func (d *Daemon) clearWithdrawn(alarms []compactorAlarm) {
	var reasons []string
	keys := make(map[string][]string, len(alarms))
	for _, a := range alarms {
		if _, seen := keys[a.reason]; !seen {
			reasons = append(reasons, a.reason)
		}
		keys[a.reason] = append(keys[a.reason], a.key)
	}
	for _, reason := range reasons {
		d.clearAlerts(reason, keys[reason]...)
	}
}

// compactorDogProbes wires compactorProbes to the live town.
func (d *Daemon) compactorDogProbes() compactorProbes {
	external, why := d.maintenance().gcExternal(d)
	if external {
		d.logger.Printf("compactor_dog: sizes unchecked — %s", why)
	}
	dataDir := d.maintenanceDataDir()
	measure := d.maintenance().measure

	return compactorProbesFor(external,
		compactorDatabaseSources{local: d.compactorDatabases, server: d.compactorServerDatabases},
		d.compactorCountCommits, func(db string) (int64, error) {
			m, err := measure(dataDir, db)
			return m.total, err
		})
}

// compactorDatabaseSources are the two places a cycle's database list can come
// from. They are named at the probe-building call site because picking the
// wrong one is silent: the cycle reports an empty or failing inspection either
// way (gt-7yejc).
type compactorDatabaseSources struct {
	// local walks the server's data directory, which must be on this host.
	local func() ([]string, error)
	// server asks the running server over SQL.
	server func() ([]string, error)
}

// compactorProbesFor is compactorDogProbes' choice of measurements, kept apart
// from the town so a test drives both branches with no server and no data
// directory.
//
// externalServer means the server's storage is not on this host: there is no
// size to read here, and the database list must come from the server as well,
// because the data directory under this town root is not the one serving (and
// usually does not exist). Walking it fails the whole cycle and leaves the
// commit alarm unevaluated on every tick.
func compactorProbesFor(externalServer bool, sources compactorDatabaseSources, commits func(db string) (int, error), size func(db string) (int64, error)) compactorProbes {
	if externalServer {
		return compactorProbes{databases: sources.server, commits: commits}
	}
	return compactorProbes{databases: sources.local, commits: commits, size: size}
}

// warnDeprecatedCompactorConfig logs a warning when the daemon patrol config
// (the "daemon" section of settings/config.json) still carries compactor_dog
// config keys that the patrol no longer honors. Removing the fields outright
// would make a stale config silently inert; keeping them parsed means an
// operator who set mode or keep_recent is told the setting is dead instead of
// quietly getting different behavior than the file implies.
func warnDeprecatedCompactorConfig(d *Daemon, cd *CompactorDogConfig) {
	if cd == nil || (cd.Mode == "" && cd.KeepRecent == 0) {
		return
	}
	d.logger.Printf("compactor_dog: WARNING: deprecated config ignored — "+
		"patrols.compactor_dog.mode=%q keep_recent=%d have no effect because the "+
		"daemon patrol no longer compacts; compaction is operator-only via "+
		"the offline procedure in docs/dolt-history-offline.md",
		cd.Mode, cd.KeepRecent)
}

// compactorDatabases lists the databases to check: patrols.compactor_dog's own
// list when an operator set one, else every database the maintenance code
// discovers under the server's data directory on this host.
//
// Discovery, not a fixed list: a database the list does not name is never
// checked, which is how a 31,335-commit database went unreported (gt-7yejc).
// It is the same walk gc does, so both patrols hold the same database set.
func (d *Daemon) compactorDatabases() ([]string, error) {
	if configured := compactorConfiguredDatabases(d.patrolConfig); len(configured) > 0 {
		return configured, nil
	}

	databases, err := d.maintenance().gcDatabases(d.maintenanceDataDir())
	if err != nil {
		return nil, err
	}
	return compactorRealDatabases(databases), nil
}

// compactorConfiguredDatabases returns the database list an operator set for
// this patrol, or nil when the config names none. It is used as given: an
// explicit list is an instruction, and filtering it would leave a database the
// operator asked for unchecked without saying so.
func compactorConfiguredDatabases(config *DaemonPatrolConfig) []string {
	if config != nil && config.Patrols != nil {
		if cd := config.Patrols.CompactorDog; cd != nil && len(cd.Databases) > 0 {
			return cd.Databases
		}
	}
	return nil
}

// compactorServerDatabases lists the databases the Dolt server itself reports,
// over SQL. It is the source when the server's data directory is not on this
// host; a remote server answers here where a directory walk cannot.
func (d *Daemon) compactorServerDatabases() ([]string, error) {
	if configured := compactorConfiguredDatabases(d.patrolConfig); len(configured) > 0 {
		return configured, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), compactorQueryTimeout)
	defer cancel()

	// No default database: SHOW DATABASES is catalog-wide, and naming one would
	// fail on a server where that database has been dropped.
	db, err := d.compactorOpenDB("")
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return nil, fmt.Errorf("show databases: %w", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("show databases: %w", err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("show databases: %w", err)
	}
	return compactorRealDatabases(names), nil
}

// compactorRealDatabases drops the names no alarm should carry: the server's
// own catalogs, and the scratch databases test cruft leaves behind
// (internal/testdb). An alarm about either is noise an operator learns to
// ignore, and noise is how a real alarm gets missed.
func compactorRealDatabases(databases []string) []string {
	real := make([]string, 0, len(databases))
	for _, db := range databases {
		if doltserver.IsSystemDatabase(db) || testdb.IsTestDatabaseName(db) {
			continue
		}
		real = append(real, db)
	}
	return real
}

// compactorCountCommits counts the number of commits in the database's dolt_log.
func (d *Daemon) compactorCountCommits(dbName string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), compactorQueryTimeout)
	defer cancel()

	db, err := d.compactorOpenDB(dbName)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	var count int
	query := fmt.Sprintf("SELECT COUNT(*) FROM `%s`.dolt_log", dbName)
	if err := db.QueryRowContext(ctx, query).Scan(&count); err != nil {
		return 0, fmt.Errorf("count dolt_log: %w", err)
	}
	return count, nil
}

// compactorOpenDB opens a connection to the Dolt server for the given database.
func (d *Daemon) compactorOpenDB(dbName string) (*sql.DB, error) {
	return sql.Open("mysql", d.compactorDSN(dbName))
}

// compactorDSN is the DSN compactorOpenDB connects with.
func (d *Daemon) compactorDSN(dbName string) string {
	return fmt.Sprintf("%s@tcp(%s)/%s?parseTime=true&timeout=5s&readTimeout=30s&writeTimeout=30s",
		d.doltUserInfo(), net.JoinHostPort(d.doltServerHost(), strconv.Itoa(d.doltServerPort())), dbName)
}
