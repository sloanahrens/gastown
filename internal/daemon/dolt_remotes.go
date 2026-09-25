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

	// doltRemotesReadTimeout is the MySQL driver's socket-level read/write
	// timeout for dolt_remotes connections. It must exceed doltPushTimeout so
	// the request context's deadline fires first (a clean cancellation)
	// rather than the driver's own readTimeout aborting a large, legitimately
	// slow DOLT_PUSH mid-flight with a raw i/o timeout.
	doltRemotesReadTimeout = doltPushTimeout + 30*time.Second

	// shutdownDoltPushBudget bounds how long daemon shutdown waits for
	// pushDoltRemotesBounded before moving on. pushDoltRemotes pushes
	// databases one at a time, each with its own doltPushTimeout (60s) per
	// add/commit/push step, so it has no bound of its own — an unreachable
	// remote could otherwise make shutdown (and so a restart's "new binary is
	// in force" wait, see waitForRestart) open-ended (gt-oqbw).
	shutdownDoltPushBudget = 20 * time.Second

	// otelShutdownBudget bounds Shutdown's OTel flush (daemon.go). Part of
	// ShutdownBudget below.
	otelShutdownBudget = 5 * time.Second
)

// ShutdownBudget is the real wall-clock ceiling on Daemon.shutdown(): the sum
// of its three bounded steps in the order they run — pushDoltRemotesBounded,
// then the Dolt SQL server's own graceful-stop wait (doltServerStopBudget,
// dolt.go) before it SIGKILLs, then the OTel flush. Everything else in
// shutdown (stopping the curator, convoy manager, KRC pruner) is in-process
// and returns immediately; these three are the only steps that wait on
// something external.
//
// This is the actual value a daemon restart must plan around — not an
// estimate — because both restart paths bound the OLD daemon's lifetime by a
// mechanism outside shutdown() itself: `gt daemon restart`'s launchd
// supervisor (internal/cmd/daemon_supervisor.go) sets the job's ExitTimeOut
// to this same budget, so a SIGTERM'd daemon that hasn't exited by then is
// SIGKILLed regardless of which step it is on; the hand path's StopDaemon
// (daemon.go) uses a much shorter ShutdownNotifyDelay (500ms) before it does
// the same. Either way, ShutdownBudget is the longest the old process can
// legitimately take, and it is what waitForRestart's poll budget is derived
// from.
const ShutdownBudget = shutdownDoltPushBudget + doltServerStopBudget + otelShutdownBudget

// doltRemotesInterval returns the configured push interval, or the default (15m).
func doltRemotesInterval(config *DaemonPatrolConfig) time.Duration {
	if config != nil && config.Patrols != nil && config.Patrols.DoltRemotes != nil {
		if config.Patrols.DoltRemotes.Interval > 0 {
			return config.Patrols.DoltRemotes.Interval
		}
	}
	return defaultDoltRemotesInterval
}

// runBounded runs fn in a goroutine and waits at most budget for it to
// finish. If it doesn't, runBounded returns anyway — calling onTimeout first
// — while fn keeps running in the background; it is never canceled, only
// abandoned. Used where a callee has no context/cancellation support of its
// own but the caller cannot afford to block on it indefinitely.
func runBounded(budget time.Duration, fn func(), onTimeout func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(budget):
		onTimeout()
	}
}

// pushDoltRemotesBounded runs pushDoltRemotes with a hard wall-clock ceiling
// so shutdown can never block on it indefinitely. The push it abandons on
// timeout keeps running in the background against a Dolt server the caller
// is about to stop — best case it finishes anyway, worst case it fails and
// logs, and either is preferable to a shutdown (and thus a restart, see
// waitForRestart) that never returns.
func (d *Daemon) pushDoltRemotesBounded() {
	runBounded(shutdownDoltPushBudget, d.pushDoltRemotes, func() {
		d.logger.Printf("Warning: dolt_remotes push still running after %s at shutdown; continuing shutdown without waiting for it", shutdownDoltPushBudget)
	})
}

// pushDoltRemotes commits and pushes each configured database to its remote.
// Non-fatal: errors are logged but don't stop the patrol.
func (d *Daemon) pushDoltRemotes() {
	if !d.isPatrolActive("dolt_remotes") {
		return
	}
	release, ok := d.tryDoltTask("dolt_remotes")
	if !ok {
		return
	}
	defer release()

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
	dsn := fmt.Sprintf("root@tcp(%s:%d)/%s?parseTime=true&timeout=5s&readTimeout=%s&writeTimeout=%s",
		d.doltServerHost(), d.doltServerPort(), dbName, doltRemotesReadTimeout, doltRemotesReadTimeout)
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

	// Each step gets its own doltPushTimeout budget. A single shared context
	// across add/commit/push would let a slow add or commit eat into the
	// budget DOLT_PUSH actually needs for large data — silently shrinking it
	// well below doltPushTimeout.
	addCtx, addCancel := context.WithTimeout(context.Background(), doltPushTimeout)
	defer addCancel()

	// Step 1: Stage any unstaged changes (non-fatal)
	if _, err := conn.ExecContext(addCtx, "CALL DOLT_ADD('-A')"); err != nil {
		// Ignore - may have nothing to stage
		d.logger.Printf("dolt_remotes: %s: add (non-fatal): %v", db, err)
	}

	commitCtx, commitCancel := context.WithTimeout(context.Background(), doltPushTimeout)
	defer commitCancel()

	// Step 2: Commit staged changes only if dolt_status shows pending work.
	// Skipping DOLT_COMMIT when nothing is staged avoids "nothing to commit"
	// warnings in dolt.log, which were causing log bloat at ~3/sec (gt-zb8).
	staged, err := d.hasStagedChanges(commitCtx, conn)
	if err != nil {
		// Fail open: if we can't check, attempt the commit and let it fail naturally.
		staged = true
	}
	if staged {
		if _, err := conn.ExecContext(commitCtx,
			"CALL DOLT_COMMIT('-m', 'daemon: auto-commit pending changes', '--author', 'Gas Town Daemon <daemon@gastown.local>')",
		); err != nil {
			d.logger.Printf("dolt_remotes: %s: commit (non-fatal): %v", db, err)
		}
	}

	pushCtx, pushCancel := context.WithTimeout(context.Background(), doltPushTimeout)
	defer pushCancel()

	// Step 3: Push to remote — gets its own fresh doltPushTimeout budget.
	if _, err := conn.ExecContext(pushCtx, "CALL DOLT_PUSH(?, ?)", remote, branch); err != nil {
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
