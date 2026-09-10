package daemon

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

const (
	defaultDoltRemotesInterval = 15 * time.Minute
	doltPushTimeout            = 60 * time.Second
)

// doltRemotesInterval returns the configured push interval, or the default (15m).
func doltRemotesInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.DoltRemotes != nil {
		if config.Patrols.DoltRemotes.Interval > 0 {
			return config.Patrols.DoltRemotes.Interval
		}
	}
	return defaultDoltRemotesInterval
}

// pushDoltRemotes commits and pushes each configured database to its remote.
// Non-fatal: errors are logged but don't stop the patrol.
func (d *Daemon) pushDoltRemotes() {
	if !d.isPatrolActive("dolt_remotes") {
		return
	}

	// Need dolt server to be configured for data dir
	if d.doltServer == nil || !d.doltServer.IsEnabled() {
		d.logger.Printf("dolt_remotes: dolt server not configured, skipping")
		return
	}

	dataDir := d.doltServer.config.DataDir
	if dataDir == "" {
		d.logger.Printf("dolt_remotes: no data dir configured, skipping")
		return
	}

	config := d.patrolConfig.Patrols.DoltRemotes
	remote := config.Remote
	branch := config.Branch
	if branch == "" {
		branch = "main"
	}

	// Get list of databases to push.
	// When a specific remote is configured, filter by it.
	// When no remote is configured, discover databases with any remote.
	databases := config.Databases
	if len(databases) == 0 {
		var err error
		if remote != "" {
			databases, err = d.discoverDatabasesWithRemotes(dataDir, remote)
		} else {
			databases, err = d.discoverDatabasesWithAnyRemote(dataDir)
		}
		if err != nil {
			d.logger.Printf("dolt_remotes: error discovering databases: %v", err)
			return
		}
	}

	if len(databases) == 0 {
		d.logger.Printf("dolt_remotes: no databases with remotes found")
		return
	}

	if remote != "" {
		d.logger.Printf("dolt_remotes: pushing %d database(s) to %s/%s", len(databases), remote, branch)
	} else {
		d.logger.Printf("dolt_remotes: pushing %d database(s) (auto-detected remotes)/%s", len(databases), branch)
	}

	pushed := 0
	for _, db := range databases {
		pushRemote := remote
		if pushRemote == "" {
			// Auto-detect the remote name for this database
			pushRemote = d.findDatabaseRemote(db)
			if pushRemote == "" {
				d.logger.Printf("dolt_remotes: %s: no remote found, skipping", db)
				continue
			}
		}
		if err := d.pushDatabase(db, pushRemote, branch); err != nil {
			d.logger.Printf("dolt_remotes: %s: push failed: %v", db, err)
		} else {
			pushed++
		}
	}

	d.logger.Printf("dolt_remotes: pushed %d/%d database(s)", pushed, len(databases))
}

// openDoltDB opens a connection to the running Dolt SQL server for the given
// database. All dolt_remotes SQL — reads and writes — goes through this live
// server connection rather than spawning a separate "dolt" CLI process against
// the on-disk data directory. Two processes touching the same Dolt data dir
// concurrently (the sql-server and a competing CLI invocation) is the exact
// unsafe-concurrent-writer hazard that keeps bd's own auto-push disabled by
// default; routing through the server that already owns the data dir avoids it.
func (d *Daemon) openDoltDB(dbName string) (*sql.DB, error) {
	dsn := fmt.Sprintf("root@tcp(%s:%d)/%s?parseTime=true&timeout=5s&readTimeout=30s&writeTimeout=30s",
		d.doltServerHost(), d.doltServerPort(), dbName)
	return sql.Open("mysql", dsn)
}

// pushDatabase commits pending changes and pushes a single database to its remote.
func (d *Daemon) pushDatabase(db, remote, branch string) error {
	// Safety: refuse to push anything that looks like a test database.
	// This is the last line of defense against pushing pollution to GitHub.
	for _, prefix := range []string{"test", "beads_t", "beads_pt", "doctest_"} {
		if strings.HasPrefix(db, prefix) {
			return fmt.Errorf("REFUSED: %q looks like a test database (prefix %q)", db, prefix)
		}
	}

	conn, err := d.openDoltDB(db)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), doltPushTimeout)
	defer cancel()

	// Step 1: Stage any unstaged changes (non-fatal)
	if _, err := conn.ExecContext(ctx, "CALL DOLT_ADD('-A')"); err != nil {
		// Ignore - may have nothing to stage
		d.logger.Printf("dolt_remotes: %s: add (non-fatal): %v", db, err)
	}

	// Step 2: Commit staged changes only if dolt_status shows pending work.
	// Skipping DOLT_COMMIT when nothing is staged avoids "nothing to commit"
	// warnings in dolt.log, which were causing log bloat at ~3/sec (gt-zb8).
	staged, err := d.hasStagedChanges(ctx, conn)
	if err != nil {
		// Fail open: if we can't check, attempt the commit and let it fail naturally.
		staged = true
	}
	if staged {
		if _, err := conn.ExecContext(ctx,
			"CALL DOLT_COMMIT('-m', 'daemon: auto-commit pending changes', '--author', 'Gas Town Daemon <daemon@gastown.local>')",
		); err != nil {
			d.logger.Printf("dolt_remotes: %s: commit (non-fatal): %v", db, err)
		}
	}

	// Step 3: Push to remote
	if _, err := conn.ExecContext(ctx, "CALL DOLT_PUSH(?, ?)", remote, branch); err != nil {
		return fmt.Errorf("push failed: %w", err)
	}

	d.logger.Printf("dolt_remotes: %s: pushed to %s/%s", db, remote, branch)
	return nil
}

// hasStagedChanges returns true if the database has staged changes in dolt_status.
func (d *Daemon) hasStagedChanges(ctx context.Context, conn *sql.DB) (bool, error) {
	var count int
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM dolt_status WHERE staged = 1").Scan(&count); err != nil {
		return false, err
	}
	return count != 0, nil
}

// discoverDatabasesWithRemotes lists databases in the data directory
// that have the specified remote configured.
func (d *Daemon) discoverDatabasesWithRemotes(dataDir, remote string) ([]string, error) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil, fmt.Errorf("reading data dir: %w", err)
	}

	var databases []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		// Skip hidden directories
		if strings.HasPrefix(name, ".") {
			continue
		}
		// Check if this directory is a Dolt database (has .dolt subdirectory)
		doltDir := filepath.Join(dataDir, name, ".dolt")
		if _, err := os.Stat(doltDir); os.IsNotExist(err) {
			continue
		}
		// Check if it has the specified remote
		if d.databaseHasRemote(name, remote) {
			databases = append(databases, name)
		}
	}

	return databases, nil
}

// databaseHasRemote checks if a database has the specified remote configured.
func (d *Daemon) databaseHasRemote(db, remote string) bool {
	conn, err := d.openDoltDB(db)
	if err != nil {
		return false
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), doltCmdTimeout)
	defer cancel()

	var name string
	if err := conn.QueryRowContext(ctx, "SELECT name FROM dolt_remotes WHERE name = ?", remote).Scan(&name); err != nil {
		return false
	}
	return true
}

// databaseHasAnyRemote checks if a database has any remote configured.
func (d *Daemon) databaseHasAnyRemote(db string) bool {
	conn, err := d.openDoltDB(db)
	if err != nil {
		return false
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), doltCmdTimeout)
	defer cancel()

	var name string
	if err := conn.QueryRowContext(ctx, "SELECT name FROM dolt_remotes LIMIT 1").Scan(&name); err != nil {
		return false
	}
	return true
}

// findDatabaseRemote returns the name of the first remote configured for a database.
// Returns empty string if no remote is found.
func (d *Daemon) findDatabaseRemote(db string) string {
	conn, err := d.openDoltDB(db)
	if err != nil {
		return ""
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), doltCmdTimeout)
	defer cancel()

	var name string
	if err := conn.QueryRowContext(ctx, "SELECT name FROM dolt_remotes LIMIT 1").Scan(&name); err != nil {
		return ""
	}
	return name
}

// discoverDatabasesWithAnyRemote lists databases that have any remote configured.
func (d *Daemon) discoverDatabasesWithAnyRemote(dataDir string) ([]string, error) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil, fmt.Errorf("reading data dir: %w", err)
	}

	var databases []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		doltDir := filepath.Join(dataDir, name, ".dolt")
		if _, err := os.Stat(doltDir); os.IsNotExist(err) {
			continue
		}
		if d.databaseHasAnyRemote(name) {
			databases = append(databases, name)
		}
	}

	return databases, nil
}
