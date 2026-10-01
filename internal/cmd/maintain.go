package cmd

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/workspace"
)

const (
	// defaultMaintainThreshold is the minimum commit count before flatten triggers.
	defaultMaintainThreshold = 100
	// maintainGCTimeout is the timeout for CALL dolt_gc() on a single database.
	maintainGCTimeout = 5 * time.Minute
	// maintainBackupTimeout is the timeout for dolt backup sync on a single database.
	maintainBackupTimeout = 2 * time.Minute
	// maintainQueryTimeout is the timeout for individual SQL queries during flatten.
	maintainQueryTimeout = 30 * time.Second
)

var (
	maintainForce         bool
	maintainDryRun        bool
	maintainThreshold     int
	maintainForceDiverged bool
)

var maintainCmd = &cobra.Command{
	Use:     "maintain",
	GroupID: GroupServices,
	Short:   "Run full Dolt maintenance (reap + flatten + gc)",
	Long: `Run the full Dolt maintenance pipeline in a single command.

All operations run via SQL on the running server — no downtime needed.

This encapsulates the maintenance procedure:
  1. Backup all databases (dolt backup sync)
  2. Reap closed wisps from each database
  3. Flatten databases over commit threshold
  4. Run dolt_gc() on each database

Each database is also probed for a configured backup remote. When a probe fails
the run stops before touching anything: a probe that did not answer is not
evidence that a database is unbacked, and flattening one without a backup
destroys the history the backup existed to keep.

Use --force for non-interactive mode (daemon/cron), or run interactively
to review the plan before proceeding.

This command flattens history. It does not check a Dolt remote: remote sync
was removed (ADR 0002). On a town that still has a Dolt remote registered
('gt doctor --check dolt-remote-leftovers' warns), remove it first with the
"Removing Dolt remotes" procedure in docs/design/dolt-storage.md, so no later
push can carry the rewritten history over the remote's.

For history-preserving reclamation, the
daemon's scheduled_maintenance patrol has a gc mode (gt config set
maintenance.mode gc): it runs CALL dolt_gc('--full') per database on a size
trigger, only while the town is quiet, and never flattens or pushes.

Examples:
  gt maintain                # Interactive (shows plan, asks confirmation)
  gt maintain --force        # Non-interactive (daemon/cron use)
  gt maintain --dry-run      # Preview what would happen
  gt maintain --threshold 50 # Custom commit threshold`,
	Args: cobra.NoArgs,
	RunE: runMaintain,
}

func init() {
	maintainCmd.Flags().BoolVar(&maintainForce, "force", false, "Non-interactive mode (skip confirmation)")
	maintainCmd.Flags().BoolVar(&maintainDryRun, "dry-run", false, "Preview without making changes")
	maintainCmd.Flags().IntVar(&maintainThreshold, "threshold", defaultMaintainThreshold, "Commit count threshold for flatten")
	// --force-diverged skipped the Dolt remote divergence pre-flight. The
	// remotes and the pre-flight are gone (ADR 0002); the flag still parses so
	// existing scripts keep working, and does nothing.
	maintainCmd.Flags().BoolVar(&maintainForceDiverged, "force-diverged", false, "No-op: Dolt remotes were removed (ADR 0002)")
	_ = maintainCmd.Flags().MarkDeprecated("force-diverged", "Dolt remotes were removed (ADR 0002); there is nothing to diverge from")
	_ = maintainCmd.Flags().MarkHidden("force-diverged")
	rootCmd.AddCommand(maintainCmd)
}

// maintainDBInfo holds per-database info for the maintenance plan.
type maintainDBInfo struct {
	name        string
	commitCount int
	// countKnown is false when maintainCountCommits failed. commitCount is then
	// meaningless — not a zero — and must not be read as one (gt-racu).
	countKnown bool
	countErr   error
	hasBackup  bool
	// backupKnown is false when maintainHasBackup failed. hasBackup is then
	// meaningless — not a "no" — and must not be read as one (gt-ij15).
	backupKnown bool
	backupErr   error
}

// needsFlatten reports whether the flatten phase should process this database.
//
// An unknown commit count must not be read as a legitimate zero: a failed
// measurement is not evidence that a database is quiet. Skipping on that basis
// is silent, because the plan would render a count failure and a genuinely idle
// database identically. An unknown count therefore flattens rather than skips.
func (db maintainDBInfo) needsFlatten(threshold int) bool {
	return !db.countKnown || db.commitCount >= threshold
}

// maintainPlanRow is one database's classification in the maintenance plan:
// whether it flattens and its backup state. It mirrors the decisions
// runMaintain's plan-display loop renders, split into a pure function of
// maintainDBInfo so the counting rules — an unknown commit count is counted
// once, only for a database that will actually flatten — have a test that
// does not require a live Dolt server (gt-aku6).
type maintainPlanRow struct {
	// willFlatten is true when the database is at or over threshold (or its
	// count is unknown).
	willFlatten bool
	// countUnknown is true only when willFlatten is also true and the commit
	// count measurement failed.
	countUnknown bool
	// hasBackup and backupUnknown mirror maintainDBInfo's own backup fields;
	// exactly one may be true, matching the mutually exclusive plan tags.
	hasBackup     bool
	backupUnknown bool
}

// maintainClassifyForPlan applies the plan's flatten/refusal/backup decisions
// to one database, exactly as runMaintain's plan-display loop does.
func maintainClassifyForPlan(db maintainDBInfo, threshold int) maintainPlanRow {
	var row maintainPlanRow
	if db.needsFlatten(threshold) {
		row.willFlatten = true
		row.countUnknown = !db.countKnown
	}
	switch {
	case !db.backupKnown:
		row.backupUnknown = true
	case db.hasBackup:
		row.hasBackup = true
	}
	return row
}

// countText renders the commit count for the plan line, keeping an unknown
// count distinguishable from a legitimate zero.
func (db maintainDBInfo) countText() string {
	if !db.countKnown {
		if db.countErr != nil {
			return fmt.Sprintf("commits unknown (%v)", db.countErr)
		}
		return "commits unknown"
	}
	return fmt.Sprintf("%d commits", db.commitCount)
}

// countLabel renders the bare commit count for the flatten result line
// ("608 → 3"). A failed measurement prints "unknown", never 0.
func (db maintainDBInfo) countLabel() string {
	if !db.countKnown {
		return "unknown"
	}
	return strconv.Itoa(db.commitCount)
}

// backupText renders the backup state for the plan line. A failed probe prints
// the failure, never the silence that reads as "no backup configured" (gt-ij15).
func (db maintainDBInfo) backupText() string {
	if db.backupKnown {
		return ""
	}
	return fmt.Sprintf("backup unknown (%v)", db.backupErr)
}

// maintainBackupRefusal returns the error that stops a run whose plan rests on
// a failed backup probe, or nil when every probe answered.
//
// The cost of the two readings of an unanswered probe is asymmetric: skipping a
// backed-up database's sync is invisible, while flattening an unbacked one
// destroys the history the backup existed to keep. An unanswered probe
// therefore refuses the run, naming each database whose probe failed (gt-ij15).
func maintainBackupRefusal(dbInfos []maintainDBInfo) error {
	var errs []error
	var names []string
	for _, db := range dbInfos {
		if db.backupKnown {
			continue
		}
		names = append(names, db.name)
		errs = append(errs, fmt.Errorf("%s: %w", db.name, db.backupErr))
	}
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("backup probe failed for %s — refusing to run maintenance: %w",
		strings.Join(names, ", "), errors.Join(errs...))
}

func runMaintain(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	config := doltserver.DefaultConfig(townRoot)
	if config.IsRemote() {
		return fmt.Errorf("maintain requires local Dolt server (remote: %s)", config.HostPort())
	}

	// Verify server is running (needed for reap + flatten phases).
	running, _, err := doltserver.IsRunning(townRoot)
	if err != nil || !running {
		return fmt.Errorf("Dolt server not running — start with 'gt dolt start'")
	}

	// Phase 0: Build and display maintenance plan.
	fmt.Printf("%s Building maintenance plan...\n", style.Bold.Render("●"))

	databases, err := doltserver.ListDatabases(townRoot)
	if err != nil {
		return fmt.Errorf("listing databases: %w", err)
	}
	if len(databases) == 0 {
		fmt.Printf("%s No databases found — nothing to maintain\n", style.Dim.Render("○"))
		return nil
	}

	dbInfos := make([]maintainDBInfo, 0, len(databases))
	for _, dbName := range databases {
		info := maintainDBInfo{name: dbName}
		count, err := maintainCountCommits(config, dbName)
		if err != nil {
			// Keep the error instead of discarding it: a bare zero would read
			// as a quiet database and silently skip it (gt-racu).
			info.countErr = err
		} else {
			info.commitCount = count
			info.countKnown = true
		}
		hasBackup, err := maintainHasBackup(config.DataDir, dbName)
		if err != nil {
			// Keep the error instead of discarding it: a bare false would read
			// as "no backup configured" and silently skip the database's
			// backup while the run still reported success (gt-ij15).
			info.backupErr = err
		} else {
			info.hasBackup = hasBackup
			info.backupKnown = true
		}
		dbInfos = append(dbInfos, info)
	}

	// Display plan.
	flattenCount := 0
	backupCount := 0
	unknownCount := 0
	unknownBackupCount := 0
	fmt.Printf("\n%s Maintenance plan:\n", style.Bold.Render("●"))
	for _, db := range dbInfos {
		row := maintainClassifyForPlan(db, maintainThreshold)
		tags := ""
		if row.willFlatten {
			reason := ""
			if row.countUnknown {
				// unknownCount feeds the "flattened, not skipped" line below
				// (gt-racu).
				reason = " (count unknown)"
				unknownCount++
			}
			tags += fmt.Sprintf(" %s", style.Warning.Render("→ flatten"+reason))
			flattenCount++
		}
		switch {
		case row.backupUnknown:
			// Neither a backed-up database nor an unbacked one: the probe did
			// not answer, and the line says so rather than leaving the gap that
			// reads as "no backup configured" (gt-ij15).
			tags += fmt.Sprintf(" %s", style.Warning.Render("→ "+db.backupText()))
			unknownBackupCount++
		case row.hasBackup:
			tags += fmt.Sprintf(" %s", style.Dim.Render("[backup]"))
			backupCount++
		}
		fmt.Printf("  %s: %s%s\n", db.name, db.countText(), tags)
	}
	fmt.Printf("\n  Databases: %d\n", len(dbInfos))
	fmt.Printf("  Will backup: %d\n", backupCount)
	fmt.Printf("  Will flatten: %d (threshold: %d commits)\n", flattenCount, maintainThreshold)
	fmt.Printf("  Will gc: %d\n", len(dbInfos))
	if unknownCount > 0 {
		fmt.Printf("  Unknown commit counts: %d (flattened, not skipped)\n", unknownCount)
	}
	if unknownBackupCount > 0 {
		fmt.Printf("  Backup state unknown: %d (probe failed — see reasons above)\n", unknownBackupCount)
	}

	// Stop before every phase that touches a database, preview included: an
	// operator asking for a plan of a broken environment needs the failure, not
	// a plan that reads as success.
	if err := maintainBackupRefusal(dbInfos); err != nil {
		return err
	}

	if maintainDryRun {
		fmt.Printf("\n%s Dry run complete — no changes made\n", style.Dim.Render("ℹ"))
		return nil
	}

	// Interactive confirmation.
	if !maintainForce {
		fmt.Printf("\nProceed? [y/N] ")
		reader := bufio.NewReader(os.Stdin)
		answer, _ := reader.ReadString('\n')
		answer = strings.TrimSpace(strings.ToLower(answer))
		if answer != "y" && answer != "yes" {
			fmt.Println("Aborted.")
			return nil
		}
	}

	start := time.Now()

	// No need to park rigs or stop the server — all operations (flatten, gc)
	// are safe on a running server per Tim Sehn (2026-02-28).

	// Phase 2: Backup.
	if backupCount > 0 {
		fmt.Printf("\n%s Backing up databases...\n", style.Bold.Render("●"))
		for _, db := range dbInfos {
			if !db.hasBackup {
				continue
			}
			backupName := db.name + "-backup"
			if err := maintainBackupSync(config.DataDir, db.name, backupName); err != nil {
				fmt.Printf("  %s %s: backup failed: %v\n", style.Warning.Render("!"), db.name, err)
			} else {
				fmt.Printf("  %s %s backed up\n", style.Bold.Render("✓"), db.name)
			}
		}
	}

	// Phase 3: Reap (server up).
	fmt.Printf("\n%s Reaping closed wisps...\n", style.Bold.Render("●"))
	totalReaped := 0
	for _, db := range dbInfos {
		purged, err := doltserver.PurgeClosedEphemerals(townRoot, db.name, false)
		if err != nil {
			fmt.Printf("  %s %s: reap failed: %v\n", style.Warning.Render("!"), db.name, err)
		} else if purged > 0 {
			fmt.Printf("  %s %s: reaped %d wisps\n", style.Bold.Render("✓"), db.name, purged)
			totalReaped += purged
		} else {
			fmt.Printf("  %s %s: nothing to reap\n", style.Dim.Render("○"), db.name)
		}
	}

	// Phase 4: Flatten (server up).
	totalFlattened := 0
	if flattenCount > 0 {
		fmt.Printf("\n%s Flattening databases...\n", style.Bold.Render("●"))
		for _, db := range dbInfos {
			if !db.needsFlatten(maintainThreshold) {
				continue
			}
			if err := maintainFlattenDB(config, db.name); err != nil {
				fmt.Printf("  %s %s: flatten failed: %v\n", style.Bold.Render("✗"), db.name, err)
			} else {
				post := maintainDBInfo{name: db.name}
				if postCount, err := maintainCountCommits(config, db.name); err != nil {
					post.countErr = err
				} else {
					post.commitCount = postCount
					post.countKnown = true
				}
				fmt.Printf("  %s %s: %s → %s commits\n",
					style.Bold.Render("✓"), db.name, db.countLabel(), post.countLabel())
				totalFlattened++
			}
		}
	}

	// Phase 5: GC (safe on running server — no downtime needed).
	gcCount := 0
	fmt.Printf("\n%s Running GC (via SQL on running server)...\n", style.Bold.Render("●"))
	for _, db := range dbInfos {
		gcStart := time.Now()
		if err := maintainGCDatabase(config, db.name); err != nil {
			fmt.Printf("  %s %s: gc failed: %v\n", style.Warning.Render("!"), db.name, err)
		} else {
			fmt.Printf("  %s %s: gc completed (%v)\n",
				style.Bold.Render("✓"), db.name, time.Since(gcStart).Round(time.Millisecond))
			gcCount++
		}
	}

	elapsed := time.Since(start)
	fmt.Printf("\n%s Maintenance complete (%v)\n", style.Success.Render("✓"), elapsed.Round(time.Second))
	fmt.Printf("  Wisps reaped: %d\n", totalReaped)
	fmt.Printf("  Databases flattened: %d\n", totalFlattened)
	fmt.Printf("  Databases gc'd: %d\n", gcCount)

	return nil
}

// maintainCountCommits returns the number of Dolt commits in a database.
func maintainCountCommits(config *doltserver.Config, dbName string) (int, error) {
	db, err := maintainOpenDB(config, dbName)
	if err != nil {
		return 0, err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), maintainQueryTimeout)
	defer cancel()

	var count int
	query := fmt.Sprintf("SELECT COUNT(*) FROM `%s`.dolt_log", dbName)
	if err := db.QueryRowContext(ctx, query).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// maintainHasBackup reports whether a database has a <name>-backup remote
// configured. A non-nil error means the probe itself failed, which is not the
// absence of a backup: `dolt backup` exits 0 with an empty list when none are
// configured, so a non-zero exit is a real fault (gt-ij15).
func maintainHasBackup(dataDir, dbName string) (bool, error) {
	return maintainHasBackupWith(runDoltBackupList, dataDir, dbName)
}

// runDoltBackupList runs `dolt backup` in dbDir and returns its stdout and stderr.
func runDoltBackupList(dbDir string) (stdout, stderr []byte, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "dolt", "backup")
	cmd.Dir = dbDir
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	out, err := cmd.Output()
	return out, errBuf.Bytes(), err
}

// maintainHasBackupWith is maintainHasBackup with the dolt invocation
// injected, so unit tests answer it without a dolt binary.
func maintainHasBackupWith(listBackups func(dbDir string) (stdout, stderr []byte, err error), dataDir, dbName string) (bool, error) {
	dbDir := filepath.Join(dataDir, dbName)

	// stderr is used for the error path only; the backup list is read from
	// stdout so a diagnostic line cannot be mistaken for a configured backup.
	output, stderr, err := listBackups(dbDir)
	if err != nil {
		if detail := strings.TrimSpace(string(stderr)); detail != "" {
			return false, fmt.Errorf("dolt backup in %s: %w: %s", dbDir, err, detail)
		}
		return false, fmt.Errorf("dolt backup in %s: %w", dbDir, err)
	}

	backupName := dbName + "-backup"
	for _, line := range strings.Split(string(output), "\n") {
		if strings.TrimSpace(line) == backupName {
			return true, nil
		}
	}
	return false, nil
}

// maintainBackupSync runs dolt backup sync for a single database.
func maintainBackupSync(dataDir, dbName, backupName string) error {
	ctx, cancel := context.WithTimeout(context.Background(), maintainBackupTimeout)
	defer cancel()

	dbDir := filepath.Join(dataDir, dbName)
	cmd := exec.CommandContext(ctx, "dolt", "backup", "sync", backupName)
	cmd.Dir = dbDir

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// maintainOpenDB opens a connection to the Dolt server for a database.
func maintainOpenDB(config *doltserver.Config, dbName string) (*sql.DB, error) {
	// wa-d6f: socket-first DSN (TCP fallback) to avoid TIME_WAIT churn from
	// short-lived gt maintain invocations.
	dsn := buildDoltDSNFromConfig(config, dbName, dsnOpts{
		ParseTime:    true,
		Timeout:      "5s",
		ReadTimeout:  "30s",
		WriteTimeout: "30s",
	})
	return sql.Open("mysql", dsn)
}

// maintainFlattenDB flattens a database's commit history to a single commit.
// Uses direct SQL on the running server — no branches, no downtime.
// Per Tim Sehn (2026-02-28): DOLT_RESET --soft + DOLT_COMMIT is safe on a
// running server. Concurrent writes during flatten are safe.
func maintainFlattenDB(config *doltserver.Config, dbName string) error {
	db, err := maintainOpenDB(config, dbName)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), maintainQueryTimeout)
	defer cancel()

	// Verify connection.
	var dummy int
	if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&dummy); err != nil {
		return fmt.Errorf("connection check: %w", err)
	}

	// Pre-flight: record row counts for integrity verification.
	preCounts, err := flattenGetRowCounts(db, dbName)
	if err != nil {
		return fmt.Errorf("pre-flight row counts: %w", err)
	}

	// Find root commit.
	var rootHash string
	if err := db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT commit_hash FROM `%s`.dolt_log ORDER BY date ASC LIMIT 1", dbName),
	).Scan(&rootHash); err != nil {
		return fmt.Errorf("find root commit: %w", err)
	}

	// USE database for session-scoped operations.
	if _, err := db.ExecContext(ctx, fmt.Sprintf("USE `%s`", dbName)); err != nil {
		return fmt.Errorf("use database: %w", err)
	}

	// Soft-reset to root on main — all data remains staged.
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_RESET('--soft', '%s')", rootHash)); err != nil {
		return fmt.Errorf("soft reset: %w", err)
	}

	// Commit flattened data.
	commitMsg := fmt.Sprintf("maintain: flatten %s history", dbName)
	if _, err := db.ExecContext(ctx, fmt.Sprintf("CALL DOLT_COMMIT('-Am', '%s')", commitMsg)); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	// Verify integrity: row counts must match pre-flight.
	postCounts, err := flattenGetRowCounts(db, dbName)
	if err != nil {
		return fmt.Errorf("post-flatten row counts: %w", err)
	}
	for table, preCount := range preCounts {
		postCount, ok := postCounts[table]
		if !ok {
			return fmt.Errorf("integrity: table %q missing after flatten", table)
		}
		if preCount != postCount {
			return fmt.Errorf("integrity: %q pre=%d post=%d", table, preCount, postCount)
		}
	}

	return nil
}

// maintainGCDatabase runs dolt gc via SQL on the running server.
// Safe on a running server — no downtime needed (Tim Sehn, 2026-02-28).
func maintainGCDatabase(config *doltserver.Config, dbName string) error {
	db, err := maintainOpenDB(config, dbName)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), maintainGCTimeout)
	defer cancel()

	if _, err := db.ExecContext(ctx, "CALL dolt_gc()"); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("timeout after %v", maintainGCTimeout)
		}
		return fmt.Errorf("dolt_gc: %w", err)
	}
	return nil
}

// flattenGetRowCounts returns table -> row count for all user tables.
func flattenGetRowCounts(db *sql.DB, dbName string) (map[string]int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	query := fmt.Sprintf("SELECT table_name FROM information_schema.tables WHERE table_schema = '%s' AND table_name NOT LIKE 'dolt_%%'", dbName)
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, name)
	}

	counts := make(map[string]int, len(tables))
	for _, table := range tables {
		var count int
		if err := db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM `%s`.`%s`", dbName, table)).Scan(&count); err != nil {
			return nil, fmt.Errorf("count %s: %w", table, err)
		}
		counts[table] = count
	}
	return counts, nil
}
