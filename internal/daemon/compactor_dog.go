package daemon

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/reaper"
)

const (
	defaultCompactorDogInterval = 24 * time.Hour
	// defaultCompactorCommitThreshold is the commit count at which this patrol
	// escalates. The daemon monitors only — it never rewrites history, so this
	// is an escalation line, not a compaction trigger.
	//
	// 2000 is deliberately far above the agent-facing plugin's escalation table
	// (>500 escalate, >1000 hard escalate — see plugins/compactor-dog/plugin.md).
	// Escalating is not free: each d.escalate mints a bead, and beads are Dolt
	// commits. A threshold at or near the plugin's 500 line would let one busy
	// 24h cycle push the commit count back over the line with its own
	// escalations, re-triggering the patrol forever. The 2000 default is the
	// buffer against that loop. Configurable via daemon.json
	// (patrols.compactor_dog.threshold).
	//
	// Commit count is not the disk cost. With scheduled_maintenance mode gc
	// handling disk by size, this threshold only guards history-query
	// latency, and a town running gc mode sets it near 20000 (measurements:
	// docs/plans/2026-09-25-dolt-gc-maintenance-design.md, Problem).
	defaultCompactorCommitThreshold = 2000
	// compactorQueryTimeout is the timeout for individual SQL queries.
	compactorQueryTimeout = 30 * time.Second
)

// CompactorDogConfig holds configuration for the compactor_dog patrol.
type CompactorDogConfig struct {
	Enabled     bool   `json:"enabled"`
	IntervalStr string `json:"interval,omitempty"`
	// Threshold is the minimum commit count before this patrol escalates.
	// The daemon monitors and escalates — it does not compact. Defaults to
	// 2000; see defaultCompactorCommitThreshold for why that is well above the
	// plugin's 500/1000 escalation lines.
	Threshold int `json:"threshold,omitempty"`
	// Databases lists specific database names to check.
	// If empty, falls back to wisp_reaper config, then auto-discovery.
	Databases []string `json:"databases,omitempty"`

	// Deprecated: has no effect. The daemon patrol no longer compacts, so there
	// is no mode to select. Compaction is operator-only:
	// plugins/compactor-dog/run.sh --compact. The field is still parsed so that
	// a stale daemon.json value is reported rather than silently dropped.
	Mode string `json:"mode,omitempty"`
	// Deprecated: has no effect. The daemon patrol no longer compacts, so there
	// is no recent history to preserve. See Mode.
	KeepRecent int `json:"keep_recent,omitempty"`
}

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

// compactorDogNow is the patrol's clock, seamed so a test can walk a
// multi-restart timeline without sleeping through it.
var compactorDogNow = time.Now

// compactorDogCycleFn runs one monitoring cycle. Seamed like the package's
// other *Fn variables: a cycle opens a SQL connection to every production
// database and pours a dog molecule, so a test of the schedule must not need
// Dolt. Recording the last-run time stays outside the seam — the record is what
// the schedule is built on.
var compactorDogCycleFn = func(d *Daemon) { d.runCompactorDog() }

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

	dec := d.compactorDogDue(compactorDogNow(), compactorDogInterval(d.patrolConfig))

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
	go func() {
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
		compactorDogCycleFn(d)
		d.recordCompactorDogRun()
	}()
}

// recordCompactorDogRun records the completion time of the cycle that just
// finished, in memory and on disk.
//
// The in-memory time is set even when the write fails: an unwritable daemon
// directory would otherwise leave the file stale, so every 15-minute check
// would see a due patrol and run a full cycle — and each cycle pours a
// molecule, making that a bead leak rather than just noise. A restart then
// reads the missing record as "run the check", which is the safe direction for
// a monitor.
func (d *Daemon) recordCompactorDogRun() {
	at := compactorDogNow()

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
// defaultCompactorCommitThreshold (2000). This is the commit count at which the
// patrol escalates; it does not trigger compaction, because the daemon patrol
// never compacts.
func compactorDogThreshold(config *DaemonPatrolConfig) int {
	if cd := compactorDogConfig(config); cd != nil {
		if cd.Threshold > 0 {
			return cd.Threshold
		}
	}
	return defaultCompactorCommitThreshold
}

// runCompactorDog checks each production database's commit count and escalates
// for every database at or above the threshold. It never rewrites history.
//
// Reconciliation with the agent-facing plugin (plugins/compactor-dog/plugin.md):
// both paths now monitor and escalate, and the operator-only destructive path
// lives in exactly one place — plugins/compactor-dog/run.sh --compact. The two
// thresholds differ because the two monitors run on different cadences and have
// different per-run costs; the daemon's 2000 versus the plugin's 500/1000 is
// explained on defaultCompactorCommitThreshold.
//
// ZFC Exemption: This dog executes imperatively in Go rather than via agent-driven
// formula execution. The mol-dog-compactor formula is used for observability
// tracking only (pourDogMolecule + closeStep/failStep). Agent execution is
// impractical because the patrol needs database/sql connections and a stable
// per-run step ledger that outlives any single agent session. See
// mol-dog-compactor.formula.toml for full rationale.
func (d *Daemon) runCompactorDog() {
	if !d.isPatrolActive("compactor_dog") {
		return
	}

	threshold := compactorDogThreshold(d.patrolConfig)
	d.logger.Printf("compactor_dog: starting monitoring cycle (threshold=%d)", threshold)

	warnDeprecatedCompactorConfig(d, compactorDogConfig(d.patrolConfig))

	mol := d.pourDogMolecule(constants.MolDogCompactor, nil)
	defer mol.close()

	databases := d.compactorDatabases()
	if len(databases) == 0 {
		d.logger.Printf("compactor_dog: no databases to check")
		mol.failStep("inspect", "no databases found")
		return
	}

	// Step "inspect": count commits per database. The step is closed exactly
	// once, after the loop that performs the inspection — counting errors are
	// inspection errors, so they are attributed here rather than to a later
	// step.
	type candidate struct {
		name    string
		commits int
	}
	var candidates []candidate
	belowThreshold := 0
	errors := 0

	for _, dbName := range databases {
		commitCount, err := d.compactorCountCommits(dbName)
		if err != nil {
			d.logger.Printf("compactor_dog: %s: error counting commits: %v", dbName, err)
			errors++
			continue
		}

		if commitCount >= threshold {
			candidates = append(candidates, candidate{name: dbName, commits: commitCount})
		} else {
			d.logger.Printf("compactor_dog: %s: %d commits (below threshold %d), OK",
				dbName, commitCount, threshold)
			belowThreshold++
		}
	}

	if errors > 0 {
		mol.failStep("inspect", fmt.Sprintf("%d databases had errors", errors))
	} else {
		mol.closeStep("inspect")
	}

	// Step "monitor": escalate. This step does not fail — escalation is the
	// happy path for a database over threshold, not an error condition.
	for _, c := range candidates {
		d.logger.Printf("compactor_dog: %s: %d commits (threshold %d) — ESCALATING",
			c.name, c.commits, threshold)
		d.escalateAlert("compactor_dog:"+c.name, "compactor_dog", fmt.Sprintf(
			"Commit threshold exceeded for %s: %d commits (threshold %d). "+
				"Compaction is operator-only: run plugins/compactor-dog/run.sh --compact. "+
				"See plugins/compactor-dog/plugin.md for the escalation policy.",
			c.name, c.commits, threshold))
	}
	mol.closeStep("monitor")

	d.logger.Printf("compactor_dog: cycle complete — above_threshold=%d below_threshold=%d errors=%d",
		len(candidates), belowThreshold, errors)
	mol.closeStep("report")
}

// warnDeprecatedCompactorConfig logs a warning when daemon.json still carries
// compactor_dog config keys that the patrol no longer honors. Removing the
// fields outright would make a stale config silently inert; keeping them parsed
// means an operator who set mode or keep_recent is told the setting is dead
// instead of quietly getting different behavior than the file implies.
func warnDeprecatedCompactorConfig(d *Daemon, cd *CompactorDogConfig) {
	if cd == nil || (cd.Mode == "" && cd.KeepRecent == 0) {
		return
	}
	d.logger.Printf("compactor_dog: WARNING: deprecated config ignored — "+
		"patrols.compactor_dog.mode=%q keep_recent=%d have no effect because the "+
		"daemon patrol no longer compacts; compaction is operator-only via "+
		"plugins/compactor-dog/run.sh --compact",
		cd.Mode, cd.KeepRecent)
}

// compactorDatabases returns the list of databases to check.
// Checks its own config first, falls back to wisp_reaper config, then auto-discovery.
func (d *Daemon) compactorDatabases() []string {
	if d.patrolConfig != nil && d.patrolConfig.Patrols != nil {
		if cd := d.patrolConfig.Patrols.CompactorDog; cd != nil {
			if len(cd.Databases) > 0 {
				return cd.Databases
			}
		}
		if d.patrolConfig.Patrols.WispReaper != nil {
			if dbs := d.patrolConfig.Patrols.WispReaper.Databases; len(dbs) > 0 {
				return dbs
			}
		}
	}
	return reaper.DefaultDatabases
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
	dsn := fmt.Sprintf("root@tcp(%s:%d)/%s?parseTime=true&timeout=5s&readTimeout=30s&writeTimeout=30s",
		"127.0.0.1", d.doltServerPort(), dbName)
	return sql.Open("mysql", dsn)
}
