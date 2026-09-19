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
		d.escalate("compactor_dog", fmt.Sprintf(
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
