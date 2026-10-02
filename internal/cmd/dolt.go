package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	gtconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/doltpause"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/testdb"
	"github.com/steveyegge/gastown/internal/ui"
	"github.com/steveyegge/gastown/internal/workspace"
)

var doltCmd = &cobra.Command{
	Use:     "dolt",
	GroupID: GroupServices,
	Short:   "Manage the Dolt SQL server",
	RunE:    requireSubcommand,
	Long: `Manage the Dolt SQL server for Gas Town beads.

The Dolt server provides multi-client access to all rig databases,
avoiding the single-writer limitation of embedded Dolt mode.

Server configuration:
  - Port: 3307 (avoids conflict with MySQL on 3306)
  - User: root (default Dolt user, no password for localhost)
  - Data directory: .dolt-data/ (contains all rig databases)

Each rig (hq, gastown, beads) has its own database subdirectory.`,
}

var doltInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Initialize and repair Dolt workspace configuration",
	Long: `Verify and repair the Dolt workspace configuration.

This command scans all rig metadata.json files for Dolt server configuration
and ensures the referenced databases actually exist. It fixes the broken state
where metadata.json says backend=dolt but the database is missing from .dolt-data/.

For each broken workspace, it will:
  1. Check if local .beads/dolt/ data exists and migrate it
  2. Otherwise, create a fresh database in .dolt-data/

This is safe to run multiple times (idempotent). It will not modify workspaces
that are already healthy.`,
	RunE: runDoltInit,
}

var doltStartCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the Dolt server",
	Long: `Start the Dolt SQL server in the background.

The server will run until stopped with 'gt dolt stop'.`,
	RunE: runDoltStart,
}

var doltStopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the Dolt server",
	Long:  `Stop the running Dolt SQL server.`,
	RunE:  runDoltStop,
}

var doltRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the Dolt server (kills imposters)",
	Long: `Stop the Dolt SQL server, kill any imposter servers on the configured port,
and start the correct server from the configured data directory.

This is the nuclear option for recovering from a hijacked port — when another
process (e.g., bd's embedded Dolt server) has taken over the port with a
different data directory, serving empty/wrong databases.

Steps:
  1. Stop the tracked server (via PID file)
  2. Kill any other dolt sql-server on the configured port (imposters)
  3. Start the correct server from .dolt-data/`,
	RunE: runDoltRestart,
}

var doltKillImpostersCmd = &cobra.Command{
	Use:   "kill-imposters",
	Short: "Kill dolt servers hijacking this workspace's port",
	Long: `Find and kill any dolt sql-server that holds this workspace's configured
port but serves from a different data directory (an "imposter").

This is safe to run at any time. It only kills servers that are:
  1. Listening on the same port as this workspace's Dolt config
  2. Serving from a data directory OTHER than this workspace's .dolt-data/

It never kills the workspace's own legitimate Dolt server.

Examples:
  gt dolt kill-imposters          # Kill imposters on configured port
  gt dolt kill-imposters --dry-run # Preview without killing`,
	RunE: runDoltKillImposters,
}

var doltKillImpostersDry bool

var doltStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show Dolt server status",
	Long:  `Show the current status of the Dolt SQL server.`,
	RunE:  runDoltStatus,
}

var doltLogsCmd = &cobra.Command{
	Use:   "logs",
	Short: "View Dolt server logs",
	Long:  `View the Dolt server log file.`,
	RunE:  runDoltLogs,
}

var doltDumpCmd = &cobra.Command{
	Use:   "dump",
	Short: "Collect non-fatal Dolt server diagnostics",
	Long: `Collect a non-fatal Dolt diagnostic snapshot for incident response.

This command does not send SIGQUIT. Dolt 1.86.5 terminates sql-server after
SIGQUIT, so default diagnostics gather process metadata and recent logs only.`,
	RunE: runDoltDump,
}

var doltSQLCmd = &cobra.Command{
	Use:   "sql",
	Short: "Open Dolt SQL shell",
	Long: `Open an interactive SQL shell to the Dolt database.

Works in both embedded mode (no server) and server mode.
For multi-client access, start the server first with 'gt dolt start'.`,
	RunE: runDoltSQL,
}

var doltInitRigCmd = &cobra.Command{
	Use:   "init-rig <name>",
	Short: "Initialize a new rig database",
	Long: `Initialize a new rig database in the Dolt data directory.

Each rig (e.g., gastown, beads) gets its own database that will be
served by the Dolt server. The rig name becomes the database name
when connecting via MySQL protocol.

Example:
  gt dolt init-rig gastown
  gt dolt init-rig beads`,
	Args: cobra.ExactArgs(1),
	RunE: runDoltInitRig,
}

var doltListCmd = &cobra.Command{
	Use:   "list",
	Short: "List available rig databases",
	Long:  `List all rig databases in the Dolt data directory.`,
	RunE:  runDoltList,
}

var doltMigrateCmd = &cobra.Command{
	Use:   "migrate",
	Short: "Migrate existing dolt databases to centralized data directory",
	Long: `Migrate existing dolt databases from .beads/dolt/ locations to the
centralized .dolt-data/ directory structure.

This command will:
1. Detect existing dolt databases in .beads/dolt/ directories
2. Move them to .dolt-data/<rigname>/
3. Remove the old empty directories

Use --dry-run to preview what would be moved (source/target paths and sizes)
without making any changes.

After migration, start the server with 'gt dolt start'.`,
	RunE: runDoltMigrate,
}

var doltFixMetadataCmd = &cobra.Command{
	Use:   "fix-metadata",
	Short: "Update metadata.json in all rig .beads directories",
	Long: `Ensure all rig .beads/metadata.json files have correct Dolt server configuration.

This fixes the split-brain problem where bd falls back to local embedded databases
instead of connecting to the centralized Dolt server. It updates metadata.json with:
  - backend: "dolt"
  - dolt_mode: "server"
  - dolt_database: "<rigname>"

Safe to run multiple times (idempotent). Preserves any existing fields in metadata.json.`,
	RunE: runDoltFixMetadata,
}

var doltCleanupCmd = &cobra.Command{
	Use:   "cleanup",
	Short: "Remove orphaned databases from .dolt-data/",
	Long: `Detect and remove orphaned databases from the .dolt-data/ directory.

An orphaned database is one that exists in .dolt-data/ but is not referenced
by any rig's metadata.json. These are typically left over from partial setups,
renamed databases, or failed migrations.

Use --dry-run to preview what would be removed without making changes.

Guardrails (gt-61x, hardened by gt-2oy):
  - Databases referenced by an open hold bead (label "dolt-hold:<dbname>", or
    a blanket "dolt-hold") are never removed, even with --force. Close the
    hold bead to release the database.
  - If the holds query fails, every destructive run is refused (fail closed);
    only --dry-run continues without hold information.
  - Agent actors cannot use --force without recording authorization via
    --authorized-by <bead-id>. Unset GT_ROLE/BD_ACTOR off-terminal still
    counts as an agent. The bead must carry the "dolt-force-auth" label, be
    open, and not be created by the requesting agent itself; the forced
    removal is logged to it as a comment.

Examples:
  gt dolt cleanup             # Remove all orphaned databases
  gt dolt cleanup --dry-run   # Preview what would be removed
  gt dolt cleanup --force --authorized-by hq-xyz  # Agent with recorded authorization`,
	RunE: runDoltCleanup,
}

var (
	doltLogLines            int
	doltLogFollow           bool
	doltMigrateDry          bool
	doltCleanupDry          bool
	doltCleanupForce        bool
	doltCleanupAuthorizedBy string
)

func init() {
	doltCmd.AddCommand(doltInitCmd)
	doltCmd.AddCommand(doltStartCmd)
	doltCmd.AddCommand(doltStopCmd)
	doltCmd.AddCommand(doltRestartCmd)
	doltCmd.AddCommand(doltKillImpostersCmd)
	doltCmd.AddCommand(doltStatusCmd)
	doltCmd.AddCommand(doltLogsCmd)
	doltCmd.AddCommand(doltDumpCmd)
	doltCmd.AddCommand(doltSQLCmd)
	doltCmd.AddCommand(doltInitRigCmd)
	doltCmd.AddCommand(doltListCmd)
	doltCmd.AddCommand(doltMigrateCmd)
	doltCmd.AddCommand(doltFixMetadataCmd)
	doltCmd.AddCommand(doltCleanupCmd)

	doltKillImpostersCmd.Flags().BoolVar(&doltKillImpostersDry, "dry-run", false, "Preview without killing")

	doltCleanupCmd.Flags().BoolVar(&doltCleanupDry, "dry-run", false, "Preview what would be removed without making changes")
	doltCleanupCmd.Flags().BoolVar(&doltCleanupForce, "force", false, "Remove databases even if they have user tables")
	doltCleanupCmd.Flags().StringVar(&doltCleanupAuthorizedBy, "authorized-by", "", "Bead ID recording the authorization for a forced cleanup (required for --force when run by an agent)")
	doltLogsCmd.Flags().IntVarP(&doltLogLines, "lines", "n", 50, "Number of lines to show")
	doltLogsCmd.Flags().BoolVarP(&doltLogFollow, "follow", "f", false, "Follow log output")

	doltMigrateCmd.Flags().BoolVar(&doltMigrateDry, "dry-run", false, "Preview what would be migrated without making changes")

	rootCmd.AddCommand(doltCmd)
}

func runDoltStart(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	config := doltserver.DefaultConfig(townRoot)
	if config.IsRemote() {
		return fmt.Errorf("Dolt server is remote (%s) — start/stop managed externally", config.HostPort())
	}

	// Check for databases before starting — user-facing guard for manual starts.
	// Internal callers (install, migrate) may legitimately start with an empty
	// data dir and create databases afterward via bd init.
	databases, _ := doltserver.ListDatabases(townRoot)
	if len(databases) == 0 {
		return fmt.Errorf("no databases found in %s\nInitialize with: gt dolt init-rig <name>", config.DataDir)
	}

	if err := doltserver.Start(townRoot); err != nil {
		return err
	}

	// Get state for display
	state, _ := doltserver.LoadState(townRoot)

	fmt.Printf("%s Dolt server started (PID %d, port %d)\n",
		style.Bold.Render("✓"), state.PID, config.Port)
	fmt.Printf("  Data dir: %s\n", state.DataDir)
	fmt.Printf("  Databases: %s\n", style.Dim.Render(strings.Join(state.Databases, ", ")))
	fmt.Printf("  Connection: %s\n", style.Dim.Render(doltserver.GetConnectionString(townRoot)))

	// Verify all filesystem databases are actually served by the SQL server.
	// Use retry since Start() only waits 500ms — DBs may still be loading.
	served, missing, verifyErr := doltserver.VerifyDatabasesWithRetry(townRoot, 5)
	if verifyErr != nil {
		fmt.Printf("  %s Could not verify databases: %v\n", style.Dim.Render("⚠"), verifyErr)
	} else if len(missing) > 0 {
		fmt.Printf("\n%s Some databases exist on disk but are NOT served:\n", style.Bold.Render("⚠"))
		for _, db := range missing {
			fmt.Printf("  - %s\n", db)
		}
		fmt.Printf("\n  Served: %v\n", served)
		fmt.Printf("  This usually means the database has a stale manifest.\n")
		fmt.Printf("  Try: %s\n", style.Dim.Render("cd ~/gt/.dolt-data/<db> && dolt fsck --repair"))
	} else {
		fmt.Printf("  %s All %d databases verified\n", style.Bold.Render("✓"), len(served))
	}

	return nil
}

func runDoltKillImposters(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	config := doltserver.DefaultConfig(townRoot)
	if config.IsRemote() {
		return fmt.Errorf("Dolt server is remote — imposter detection requires local server")
	}

	conflictPID, conflictDataDir := doltserver.CheckPortConflict(townRoot)
	if conflictPID == 0 {
		fmt.Printf("%s No imposters found on port %d\n", style.Bold.Render("✓"), config.Port)
		return nil
	}

	fmt.Printf("Found imposter dolt server:\n")
	fmt.Printf("  PID:      %d\n", conflictPID)
	fmt.Printf("  Data-dir: %s\n", conflictDataDir)
	fmt.Printf("  Expected: %s\n", config.DataDir)

	if doltKillImpostersDry {
		fmt.Printf("\n%s Dry-run — not killing\n", style.Warning.Render("~"))
		return nil
	}

	if err := doltserver.KillImposters(townRoot); err != nil {
		return fmt.Errorf("killing imposter: %w", err)
	}
	fmt.Printf("%s Imposter killed (PID %d)\n", style.Bold.Render("✓"), conflictPID)
	return nil
}

func runDoltStop(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	config := doltserver.DefaultConfig(townRoot)
	if config.IsRemote() {
		return fmt.Errorf("Dolt server is remote (%s) — start/stop managed externally", config.HostPort())
	}

	_, pid, _ := doltserver.IsRunning(townRoot)

	if err := doltserver.Stop(townRoot); err != nil {
		return err
	}

	fmt.Printf("%s Dolt server stopped (was PID %d)\n", style.Bold.Render("✓"), pid)
	return nil
}

func runDoltRestart(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	config := doltserver.DefaultConfig(townRoot)
	if config.IsRemote() {
		return fmt.Errorf("Dolt server is remote (%s) — start/stop managed externally", config.HostPort())
	}

	// Step 1: Stop tracked server (if running)
	running, pid, _ := doltserver.IsRunning(townRoot)
	if running {
		fmt.Printf("Stopping Dolt server (PID %d)...\n", pid)
		if err := doltserver.Stop(townRoot); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: stop failed: %v (continuing with imposter kill)\n", err)
		} else {
			fmt.Printf("%s Stopped\n", style.Bold.Render("✓"))
		}
	}

	// Step 2: Kill any imposters on the port
	fmt.Println("Checking for imposter servers...")
	if err := doltserver.KillImposters(townRoot); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: imposter kill failed: %v\n", err)
	}

	// Brief pause to let port be released
	clockwork.NewRealClock().Sleep(500 * time.Millisecond)

	// Step 3: Check for databases before starting
	databases, _ := doltserver.ListDatabases(townRoot)
	if len(databases) == 0 {
		return fmt.Errorf("no databases found in %s\nInitialize with: gt dolt init-rig <name>", config.DataDir)
	}

	// Step 4: Start the correct server
	fmt.Println("Starting Dolt server...")
	if err := doltserver.Start(townRoot); err != nil {
		return fmt.Errorf("restart failed: %w", err)
	}

	// Display status (same as gt dolt start)
	state, _ := doltserver.LoadState(townRoot)

	fmt.Printf("%s Dolt server restarted (PID %d, port %d)\n",
		style.Bold.Render("✓"), state.PID, config.Port)
	fmt.Printf("  Data dir: %s\n", state.DataDir)
	fmt.Printf("  Databases: %s\n", style.Dim.Render(strings.Join(state.Databases, ", ")))
	fmt.Printf("  Connection: %s\n", style.Dim.Render(doltserver.GetConnectionString(townRoot)))

	// Verify databases
	served, missing, verifyErr := doltserver.VerifyDatabasesWithRetry(townRoot, 5)
	if verifyErr != nil {
		fmt.Printf("  %s Could not verify databases: %v\n", style.Dim.Render("⚠"), verifyErr)
	} else if len(missing) > 0 {
		fmt.Printf("\n%s Some databases exist on disk but are NOT served:\n", style.Bold.Render("⚠"))
		for _, db := range missing {
			fmt.Printf("  - %s\n", db)
		}
		fmt.Printf("\n  Served: %v\n", served)
	} else {
		fmt.Printf("  %s All %d databases verified\n", style.Bold.Render("✓"), len(served))
	}

	return nil
}

func runDoltStatus(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	running, pid, err := doltserver.IsRunning(townRoot)
	if err != nil {
		return fmt.Errorf("checking server status: %w", err)
	}

	config := doltserver.DefaultConfig(townRoot)

	if p := doltpause.Current(townRoot, time.Now()); p != nil {
		fmt.Printf("%s %s\n", style.Bold.Render("⏸"), p.Message())
	}

	if config.IsRemote() {
		if running {
			fmt.Printf("%s Dolt server is %s (remote: %s)\n",
				style.Bold.Render("●"),
				style.Bold.Render("reachable"),
				config.HostPort())
		} else {
			fmt.Printf("%s Dolt server is %s (remote: %s)\n",
				style.Dim.Render("○"),
				"not reachable",
				config.HostPort())
		}
		fmt.Printf("  Connection: %s\n", doltserver.GetConnectionString(townRoot))
		printBeadsRuntimeConfig(townRoot)
		if running {
			metrics := doltserver.GetHealthMetrics(townRoot)
			fmt.Printf("\n  %s\n", style.Bold.Render("Resource Metrics:"))
			fmt.Printf("    Query latency: %v\n", metrics.QueryLatency.Round(time.Millisecond))
			fmt.Printf("    Connections:   %d / %d (%.0f%%)\n",
				metrics.Connections, metrics.MaxConnections, metrics.ConnectionPct)
			if metrics.ReadOnly {
				fmt.Printf("\n  %s %s\n",
					style.Bold.Render("!!!"),
					style.Bold.Render("SERVER IS READ-ONLY — contact the remote server admin"))
			}
		}
		return nil
	}

	if running {
		fmt.Printf("%s Dolt server is %s (PID %d)\n",
			style.Bold.Render("●"),
			style.Bold.Render("running"),
			pid)

		// Query the live server for the authoritative database list. The
		// state file's databases[] is a snapshot taken at server start and
		// never updated, so DBs created/dropped since then would render
		// stale (gt-cs6).
		served, missing, verifyErr := doltserver.VerifyDatabases(townRoot)

		// Load state for more details
		state, err := doltserver.LoadState(townRoot)
		if err == nil && !state.StartedAt.IsZero() {
			fmt.Printf("  Started: %s\n", state.StartedAt.Format("2006-01-02 15:04:05"))
			fmt.Printf("  Port: %d\n", state.Port)
			fmt.Printf("  Data dir: %s\n", state.DataDir)
			databases, dbLabel := statusDatabases(served, verifyErr, state.Databases)
			if len(databases) > 0 {
				owners := doltserver.CollectDatabaseOwners(townRoot)
				fmt.Printf("  %s\n", dbLabel)
				for _, db := range databases {
					if owner, ok := owners[db]; ok {
						fmt.Printf("    - %-20s (%s)\n", db, owner)
					} else {
						fmt.Printf("    - %s\n", db)
					}
				}
			}
			fmt.Printf("  Connection: %s\n", doltserver.GetConnectionString(townRoot))
			printBeadsRuntimeConfig(townRoot)
		}

		// Resource metrics
		metrics := doltserver.GetHealthMetrics(townRoot)
		fmt.Printf("\n  %s\n", style.Bold.Render("Resource Metrics:"))
		fmt.Printf("    Query latency: %v\n", metrics.QueryLatency.Round(time.Millisecond))
		fmt.Printf("    Connections:   %d / %d (%.0f%%)\n",
			metrics.Connections, metrics.MaxConnections, metrics.ConnectionPct)
		fmt.Printf("    Disk usage:    %s\n", metrics.DiskUsageHuman)
		if metrics.ReadOnly {
			fmt.Printf("\n  %s %s\n",
				style.Bold.Render("!!!"),
				style.Bold.Render("SERVER IS READ-ONLY — run 'gt dolt restart'"))
		}

		// Verify all filesystem databases are actually served.
		if verifyErr != nil {
			fmt.Printf("\n  %s Database verification failed: %v\n", style.Bold.Render("!"), verifyErr)
		} else if len(missing) > 0 {
			fmt.Printf("\n  %s %s\n", style.Bold.Render("!!!"),
				style.Bold.Render("MISSING DATABASES — exist on disk but not served:"))
			for _, db := range missing {
				fmt.Printf("    - %s\n", db)
			}
			fmt.Printf("  Try: cd ~/gt/.dolt-data/<db> && dolt fsck --repair\n")
		}

		// Check for orphaned databases
		orphans, orphanErr := doltserver.FindOrphanedDatabases(townRoot)
		if orphanErr == nil && len(orphans) > 0 {
			fmt.Printf("\n  %s %d orphaned database(s) (not referenced by any rig):\n",
				style.Bold.Render("!"), len(orphans))
			for _, o := range orphans {
				fmt.Printf("    - %s (%s)\n", o.Name, formatBytes(o.SizeBytes))
			}
			fmt.Printf("  Clean up with: %s\n", style.Dim.Render("gt dolt cleanup"))
		}

		if len(metrics.Warnings) > 0 {
			fmt.Printf("\n  %s\n", style.Bold.Render("Warnings:"))
			for _, w := range metrics.Warnings {
				fmt.Printf("    %s %s\n", style.Bold.Render("!"), w)
			}
		}
	} else {
		fmt.Printf("%s Dolt server is %s\n",
			style.Dim.Render("○"),
			"not running")

		// List available databases
		databases, _ := doltserver.ListDatabases(townRoot)
		if len(databases) == 0 {
			fmt.Printf("\n%s No rig databases found in %s\n",
				style.Bold.Render("!"),
				config.DataDir)
			fmt.Printf("  Initialize with: %s\n", style.Dim.Render("gt dolt init-rig <name>"))
		} else {
			fmt.Printf("\nAvailable databases in %s:\n", config.DataDir)
			owners := doltserver.CollectDatabaseOwners(townRoot)
			for _, db := range databases {
				if owner, ok := owners[db]; ok {
					fmt.Printf("  - %-20s (%s)\n", db, owner)
				} else {
					fmt.Printf("  - %s\n", db)
				}
			}
			fmt.Printf("\nStart with: %s\n", style.Dim.Render("gt dolt start"))
		}
	}

	return nil
}

// statusDatabases picks the database list to display in `gt dolt status` and
// a label annotating its source. The live SHOW DATABASES result is
// authoritative — including when it's empty; the state-file snapshot (taken
// at server start, never refreshed) is only a fallback when the live query
// failed.
func statusDatabases(served []string, verifyErr error, cached []string) ([]string, string) {
	if verifyErr != nil {
		return cached, "Databases (cached at server start; live query failed):"
	}
	return served, "Databases (live):"
}

func currentBeadsRuntimeConfig() (doltserver.BeadsRuntimeConfig, bool) {
	cwd, err := os.Getwd()
	if err != nil {
		return doltserver.BeadsRuntimeConfig{}, false
	}
	return doltserver.ReadBeadsRuntimeConfig(beads.ResolveBeadsDir(cwd))
}

func printBeadsRuntimeConfig(townRoot string) {
	cfg, ok := currentBeadsRuntimeConfig()
	if !ok {
		return
	}
	parts := []string{"server metadata"}
	if cfg.Database != "" {
		parts = append(parts, "database "+cfg.Database)
	}
	if cfg.Host != "" && cfg.Port > 0 {
		parts = append(parts, netJoinHostPort(cfg.Host, cfg.Port))
	}
	if cfg.Source != "" {
		parts = append(parts, "from "+cfg.Source)
	}
	fmt.Printf("  Beads client: %s\n", strings.Join(parts, ", "))
	if hint := beadsScopeHint(cfg.Database, townRoot); hint != "" {
		fmt.Print(hint)
	}
}

func beadsScopeHint(database, townRoot string) string {
	if database != "hq" {
		return ""
	}

	return fmt.Sprintf("    Gas Town town beads use database hq. Use `bd -C %s <cmd>` for hq-* beads; do not use `bd --global`, which targets Beads' beads_global database.\n", gtconfig.ShellQuote(townRoot))
}

func netJoinHostPort(host string, port int) string {
	return host + ":" + strconv.Itoa(port)
}

func runDoltLogs(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	config := doltserver.DefaultConfig(townRoot)

	if _, err := os.Stat(config.LogFile); os.IsNotExist(err) {
		return fmt.Errorf("no log file found at %s", config.LogFile)
	}

	if doltLogFollow {
		// Use tail -f for following
		tailCmd := exec.Command("tail", "-f", config.LogFile)
		tailCmd.Stdout = os.Stdout
		tailCmd.Stderr = os.Stderr
		return tailCmd.Run()
	}

	// Use tail -n for last N lines
	tailCmd := exec.Command("tail", "-n", strconv.Itoa(doltLogLines), config.LogFile)
	tailCmd.Stdout = os.Stdout
	tailCmd.Stderr = os.Stderr
	return tailCmd.Run()
}

func runDoltDump(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	running, pid, err := doltserver.IsRunning(townRoot)
	if err != nil {
		return fmt.Errorf("checking server status: %w", err)
	}
	if !running {
		return fmt.Errorf("Dolt server is not running — nothing to dump")
	}

	config := doltserver.DefaultConfig(townRoot)

	fmt.Printf("Dolt diagnostic snapshot (non-fatal)\n")
	fmt.Printf("  Live PID:   %d\n", pid)
	fmt.Printf("  Port:       %d\n", config.Port)
	fmt.Printf("  Data dir:   %s\n", config.DataDir)
	fmt.Printf("  Log file:   %s\n", config.LogFile)
	fmt.Printf("  Connection: %s\n", doltserver.GetConnectionString(townRoot))

	if info, err := doltserver.ReadSQLServerInfo(townRoot); err == nil {
		fmt.Printf("  SQL metadata: %s\n", info.Path)
		fmt.Printf("    PID:       %d\n", info.PID)
		fmt.Printf("    Port:      %d\n", info.Port)
		if info.ServerID != "" {
			fmt.Printf("    Server ID: %s\n", info.ServerID)
		}
	} else {
		fmt.Printf("  SQL metadata: unavailable (%v)\n", err)
	}

	if state, err := doltserver.LoadState(townRoot); err == nil && state.PID > 0 {
		fmt.Printf("  Daemon state: %s\n", doltserver.StateFile(townRoot))
		fmt.Printf("    PID:       %d", state.PID)
		if state.PID != pid {
			fmt.Printf(" (stale; live PID is %d)", pid)
		}
		fmt.Println()
		if !state.StartedAt.IsZero() {
			fmt.Printf("    Started:   %s\n", state.StartedAt.Format("2006-01-02 15:04:05"))
		}
		if state.DataDir != "" {
			fmt.Printf("    Data dir:  %s\n", state.DataDir)
		}
	}

	fmt.Printf("\nRecent Dolt log lines:\n")
	tailCmd := exec.Command("tail", "-n", "200", config.LogFile)
	tailCmd.Stdout = os.Stdout
	tailCmd.Stderr = os.Stderr
	if err := tailCmd.Run(); err != nil {
		fmt.Printf("  (unable to read recent logs: %v)\n", err)
	}

	fmt.Printf("\nNo signal was sent. Do not use kill -QUIT for routine diagnostics unless the Dolt version has been verified not to terminate on SIGQUIT.\n")

	return nil
}

func runDoltSQL(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	config := doltserver.DefaultConfig(townRoot)

	// Check if server is running - if so, connect via Dolt SQL client
	running, _, _ := doltserver.IsRunning(townRoot)
	if running {
		// Connect to running server using dolt sql client
		// Using --no-tls since server doesn't have TLS configured
		host := config.Host
		if host == "" {
			host = "127.0.0.1"
		}
		sqlArgs := []string{
			"--host", host,
			"--port", strconv.Itoa(config.Port),
			"--user", config.User,
			"--no-tls",
			"sql",
		}
		sqlCmd := exec.Command("dolt", sqlArgs...)
		// GH#2537: Set cmd.Dir to prevent stray .doltcfg/privileges.db in CWD.
		sqlCmd.Dir = config.DataDir
		if config.Password != "" {
			sqlCmd.Env = append(os.Environ(), "DOLT_CLI_PASSWORD="+config.Password)
		}
		sqlCmd.Stdin = os.Stdin
		sqlCmd.Stdout = os.Stdout
		sqlCmd.Stderr = os.Stderr
		return sqlCmd.Run()
	}

	// Server not running - list databases and pick first one for embedded mode
	databases, err := doltserver.ListDatabases(townRoot)
	if err != nil {
		return fmt.Errorf("listing databases: %w", err)
	}

	if len(databases) == 0 {
		return fmt.Errorf("no databases found in %s\nInitialize with: gt dolt init-rig <name>", config.DataDir)
	}

	// Use first database for embedded SQL shell
	dbDir := doltserver.RigDatabaseDir(townRoot, databases[0])
	fmt.Printf("Using database: %s (start server with 'gt dolt start' for multi-database access)\n\n", databases[0])

	sqlCmd := exec.Command("dolt", "sql")
	sqlCmd.Dir = dbDir
	sqlCmd.Stdin = os.Stdin
	sqlCmd.Stdout = os.Stdout
	sqlCmd.Stderr = os.Stderr

	return sqlCmd.Run()
}

func runDoltInitRig(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	rigName := args[0]

	serverWasRunning, created, err := doltserver.InitRig(townRoot, rigName)
	if err != nil {
		return err
	}

	config := doltserver.DefaultConfig(townRoot)
	rigDir := doltserver.RigDatabaseDir(townRoot, rigName)

	if !created {
		fmt.Printf("%s Rig database %q already exists (no-op)\n", style.Bold.Render("✓"), rigName)
		fmt.Printf("  Location: %s\n", rigDir)
		return nil
	}

	fmt.Printf("%s Initialized rig database %q\n", style.Bold.Render("✓"), rigName)
	fmt.Printf("  Location: %s\n", rigDir)
	fmt.Printf("  Data dir: %s\n", config.DataDir)

	if serverWasRunning {
		fmt.Printf("  Server: %s\n", style.Bold.Render("database registered with running server"))
	} else {
		fmt.Printf("\nStart server with: %s\n", style.Dim.Render("gt dolt start"))
	}

	return nil
}

func runDoltInit(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Find workspaces with broken Dolt configuration
	broken, verifyWarning := doltserver.FindBrokenWorkspaces(townRoot)
	if verifyWarning != "" {
		fmt.Printf("  %s %s\n\n", style.Bold.Render("⚠"), verifyWarning)
	}

	// Check for orphaned databases regardless of broken workspaces
	orphans, orphanErr := doltserver.FindOrphanedDatabases(townRoot)

	if len(broken) == 0 {
		// Also check if there are any databases at all
		databases, _ := doltserver.ListDatabases(townRoot)
		if len(databases) == 0 {
			fmt.Println("No Dolt databases found and no workspaces configured for Dolt.")
			fmt.Printf("\nInitialize a rig database with: %s\n", style.Dim.Render("gt dolt init-rig <name>"))
		} else {
			fmt.Printf("%s All workspaces healthy (%d database(s) verified)\n",
				style.Bold.Render("✓"), len(databases))
		}

		// Report orphans even when workspaces are healthy
		if orphanErr == nil && len(orphans) > 0 {
			fmt.Printf("\n%s %d orphaned database(s) in .dolt-data/ (not referenced by any rig):\n",
				style.Bold.Render("!"), len(orphans))
			for _, o := range orphans {
				fmt.Printf("  - %s (%s)\n", o.Name, formatBytes(o.SizeBytes))
			}
			fmt.Printf("\nClean up with: %s\n", style.Dim.Render("gt dolt cleanup"))
		}

		return nil
	}

	fmt.Printf("Found %d workspace(s) with broken Dolt configuration:\n\n", len(broken))

	repaired := 0
	for _, ws := range broken {
		if ws.NotServed {
			fmt.Printf("  %s %s: database %q exists on disk but is not served by the running Dolt server\n",
				style.Bold.Render("!"), ws.RigName, ws.ConfiguredDB)
			fmt.Printf("    Try restarting the server: %s\n", style.Dim.Render("gt dolt restart"))
			continue
		}
		fmt.Printf("  %s %s: metadata.json → database %q (missing from .dolt-data/)\n",
			style.Bold.Render("!"), ws.RigName, ws.ConfiguredDB)
		if ws.HasLocalData {
			fmt.Printf("    Local data found at %s\n", style.Dim.Render(ws.LocalDataPath))
		}

		action, err := doltserver.RepairWorkspace(townRoot, ws)
		if err != nil {
			fmt.Printf("    %s Repair failed: %v\n", style.Bold.Render("✗"), err)
			continue
		}

		fmt.Printf("    %s Repaired: %s\n", style.Bold.Render("✓"), action)
		repaired++
	}

	if repaired > 0 {
		fmt.Printf("\n%s Repaired %d/%d workspace(s)\n", style.Bold.Render("✓"), repaired, len(broken))
	}

	// Report orphans after repairs
	if orphanErr == nil && len(orphans) > 0 {
		fmt.Printf("\n%s %d orphaned database(s) in .dolt-data/ (not referenced by any rig):\n",
			style.Bold.Render("!"), len(orphans))
		for _, o := range orphans {
			fmt.Printf("  - %s (%s)\n", o.Name, formatBytes(o.SizeBytes))
		}
		fmt.Printf("\nClean up with: %s\n", style.Dim.Render("gt dolt cleanup"))
	}

	return nil
}

func runDoltCleanup(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// GUARDRAIL (gt-61x): agents cannot force-remove databases without recorded
	// authorization. A reaper dog once force-deleted DBs that a hold bead had
	// explicitly parked for a human/mayor decision — the only trace was an
	// ephemeral nudge. Forcing agents through --authorized-by leaves a durable
	// audit trail on the authorizing bead.
	if doltCleanupForce && !doltCleanupDry {
		// Identity is corroborated, not just read from env: unset GT_ROLE/BD_ACTOR
		// off-terminal is treated as an agent, so agents cannot pose as human
		// operators by unsetting their env (gt-2oy).
		if actor, isAgent := resolveDestructiveActor(agentActor(os.Getenv), ui.IsTerminal()); isAgent {
			if err := checkAgentForceAuthorization(actor, doltCleanupAuthorizedBy); err != nil {
				return err
			}
			issue, err := beads.New(townRoot).Show(doltCleanupAuthorizedBy)
			if err != nil {
				return fmt.Errorf("--authorized-by bead %q not found: %w", doltCleanupAuthorizedBy, err)
			}
			// The bead must be a genuine authorization record, not just any bead (gt-2oy).
			if err := doltserver.ValidateForceAuthorization(issue, actor); err != nil {
				return err
			}
		}
	}

	orphans, err := doltserver.FindOrphanedDatabases(townRoot)
	if err != nil {
		return fmt.Errorf("finding orphaned databases: %w", err)
	}

	if len(orphans) == 0 {
		fmt.Printf("%s No orphaned databases found in .dolt-data/\n", style.Bold.Render("✓"))
		return nil
	}

	// GUARDRAIL (gt-61x): databases referenced by an open hold bead are excluded
	// from cleanup regardless of --force. Close the hold bead to release them.
	holds, holdsErr := doltserver.FindDatabaseHolds(townRoot)
	// Fail closed for ALL destructive runs, not just --force: if we can't see
	// the holds, we can't prove any removal is safe (gt-2oy). Dry runs may
	// continue — they remove nothing.
	if err := holdsGateError(holdsErr, doltCleanupDry); err != nil {
		return err
	}
	if holdsErr != nil {
		fmt.Printf("%s Could not query database holds: %v\n", style.Bold.Render("!"), holdsErr)
		fmt.Printf("  Dry run continues without hold information.\n\n")
	}

	var removable []doltserver.OrphanedDatabase
	held := 0
	fmt.Printf("Found %d orphaned database(s) in .dolt-data/:\n\n", len(orphans))
	for _, o := range orphans {
		if hold := doltserver.HoldFor(holds, o.Name); hold != nil {
			held++
			fmt.Printf("  %s %s (%s) — HELD by %s: %s\n", style.Bold.Render("⛔"), o.Name,
				formatBytes(o.SizeBytes), hold.BeadID, hold.Title)
			fmt.Printf("    %s\n", style.Dim.Render("excluded from cleanup; close the hold bead to release"))
			continue
		}
		removable = append(removable, o)
		fmt.Printf("  %s %s (%s)\n", style.Bold.Render("!"), o.Name, formatBytes(o.SizeBytes))
		fmt.Printf("    %s\n", style.Dim.Render(o.Path))
	}

	if doltCleanupDry {
		fmt.Println("\nDry run: no changes made.")
		return nil
	}

	if len(removable) == 0 {
		fmt.Printf("\n%s All %d orphaned database(s) are held — nothing to remove.\n",
			style.Bold.Render("!"), held)
		return nil
	}
	orphans = removable

	// BALK: If orphans are a large fraction of all databases, something is likely
	// wrong with the orphan detection (e.g., metadata files not found). Refuse to
	// proceed without --force to prevent accidentally dropping production databases. (gt-xvh)
	allDBs, _ := doltserver.ListDatabases(townRoot)
	if len(allDBs) > 0 && !doltCleanupForce {
		orphanRatio := float64(len(orphans)) / float64(len(allDBs))
		if orphanRatio > 0.5 && len(orphans) > 3 {
			fmt.Printf("\n%s %d of %d databases (%.0f%%) flagged as orphans — this is suspicious.\n",
				style.Bold.Render("!"), len(orphans), len(allDBs), orphanRatio*100)
			fmt.Printf("  This usually means metadata.json files are missing or incorrect,\n")
			fmt.Printf("  not that the databases are actually orphaned.\n\n")
			fmt.Printf("  To proceed anyway: gt dolt cleanup --force\n")
			fmt.Printf("  To diagnose: gt dolt list   (check owner column for mismatches)\n")
			return fmt.Errorf("refusing to clean %d/%d databases without --force (safety check, gt-xvh)", len(orphans), len(allDBs))
		}
	}

	// BALK: If there are too many orphans, SQL-based cleanup will take hours
	// because each DROP DATABASE is a separate query against an overloaded server.
	// Force the user to stop the server and clean the filesystem directly.
	// (Clown Show #18: 245 orphans at 27s latency = ~2 hour cleanup)
	const maxSQLCleanup = 50
	if len(orphans) > maxSQLCleanup {
		fmt.Printf("\n%s Too many orphans (%d) for SQL-based cleanup (max %d).\n",
			style.Bold.Render("!"), len(orphans), maxSQLCleanup)
		fmt.Printf("  The server is likely overloaded. SQL cleanup would take hours.\n\n")
		fmt.Printf("  Instead, stop the server and clean the filesystem:\n\n")
		fmt.Printf("    gt dolt stop\n")
		fmt.Printf("    cd %s/.dolt-data && rm -rf %s\n", townRoot, testDatabaseGlobs())
		fmt.Printf("    gt dolt start\n\n")
		fmt.Printf("  This is safe — orphan databases have no production data.\n")
		return fmt.Errorf("too many orphans (%d) for SQL cleanup — see instructions above", len(orphans))
	}

	// AUDIT (gt-87a): write-ahead intent record before the first DROP. A crash
	// or kill mid-loop must still leave a durable record of who intended to
	// remove what. Recording failure aborts the cleanup (fail closed).
	var auditor *cleanupAuditor
	if doltCleanupForce {
		auditor = newCleanupAuditor(townRoot, cleanupActorLabel(), doltCleanupAuthorizedBy)
		names := make([]string, len(orphans))
		for i, o := range orphans {
			names[i] = o.Name
		}
		if err := auditor.recordIntent(names); err != nil {
			return err
		}
	}

	fmt.Println()
	removed := 0
	var removedNames []string
	var failedNames []string
	for i, o := range orphans {
		if err := doltserver.RemoveDatabase(townRoot, o.Name, doltCleanupForce); err != nil {
			// If DROP caused read-only, stop immediately and recover (gt-r1cyd)
			if doltserver.IsReadOnlyError(err.Error()) {
				fmt.Printf("  %s DROP put server into read-only mode — attempting recovery...\n", style.Bold.Render("!"))
				if recoverErr := doltserver.RecoverReadOnly(townRoot); recoverErr != nil {
					fmt.Printf("  %s Recovery failed: %v\n", style.Bold.Render("✗"), recoverErr)
					fmt.Printf("  Run: gt dolt stop && gt dolt start\n")
				} else {
					fmt.Printf("  %s Server recovered from read-only state\n", style.Bold.Render("✓"))
				}
				// Stopping here leaves this orphan and every orphan after it in
				// place; they are failures, not omissions (gt-tfsp6).
				failedNames = append(failedNames, o.Name)
				for _, rest := range orphans[i+1:] {
					failedNames = append(failedNames, rest.Name)
				}
				break
			}
			fmt.Printf("  %s Failed to remove %s: %v\n", style.Bold.Render("✗"), o.Name, err)
			failedNames = append(failedNames, o.Name)
			continue
		}
		fmt.Printf("  %s Removed %s\n", style.Bold.Render("✓"), o.Name)
		removed++
		removedNames = append(removedNames, o.Name)

		// Health check after each DROP to catch read-only early (gt-r1cyd)
		probe := doltserver.CheckReadOnly(townRoot)
		if probe.IsUnknown() {
			fmt.Printf("  %s Read-only probe could not run: %v\n", style.Bold.Render("!"), probe.Err())
		}
		if probe.IsFail() {
			fmt.Printf("  %s Server went read-only after DROP — attempting recovery...\n", style.Bold.Render("!"))
			if recoverErr := doltserver.RecoverReadOnly(townRoot); recoverErr != nil {
				fmt.Printf("  %s Recovery failed: %v\n", style.Bold.Render("✗"), recoverErr)
				fmt.Printf("  Run: gt dolt stop && gt dolt start\n")
				for _, rest := range orphans[i+1:] {
					failedNames = append(failedNames, rest.Name)
				}
				break
			}
			fmt.Printf("  %s Server recovered — continuing cleanup\n", style.Bold.Render("✓"))
		}
	}

	// A pass that leaves orphans behind must not exit 0: scripts and agents
	// gate on the exit code, so "Removed 0/1" reading as success hides the
	// failure (gt-tfsp6).
	cleanupErr := cleanupExitError(failedNames, len(orphans))
	glyph := style.Bold.Render("✓")
	if cleanupErr != nil {
		glyph = style.Bold.Render("✗")
	}
	fmt.Printf("\n%s Removed %d/%d orphaned database(s)\n", glyph, removed, len(orphans))

	// Record the forced removal on the authorizing bead (audit trail, gt-61x).
	// Runs even when removed == 0 (a failed attempt is audit-worthy) and fails
	// loud: a recording failure is a non-zero exit, not a warning (gt-87a).
	if auditor != nil {
		if err := auditor.recordCompletion(removed, len(orphans), removedNames); err != nil {
			fmt.Printf("%s %v\n", style.Bold.Render("!"), err)
			return err
		}
		if doltCleanupAuthorizedBy != "" {
			fmt.Printf("Recorded on %s\n", style.Dim.Render(doltCleanupAuthorizedBy))
		}
	}

	return cleanupErr
}

// cleanupExitError is the cleanup pass's exit status: non-nil when any targeted
// orphan was left in place, naming them. Callers gate on the exit code, so a
// run that removed 0 of 1 orphans exiting 0 reads as success (gt-tfsp6).
func cleanupExitError(failed []string, targeted int) error {
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("failed to remove %d of %d orphaned database(s): %s",
		len(failed), targeted, strings.Join(failed, ", "))
}

// resolveDestructiveActor decides who is running a destructive cleanup.
// Env identity (GT_ROLE/BD_ACTOR) alone is spoofable: an agent can run
// 'env -u GT_ROLE -u BD_ACTOR gt dolt cleanup --force' to pose as a human
// operator (gt-2oy). So unset identity only counts as human when stdout is an
// interactive terminal; off-terminal, unset identity is agent-by-default.
func resolveDestructiveActor(envActor string, stdoutIsTTY bool) (actor string, isAgent bool) {
	if envActor != "" {
		return envActor, true
	}
	if stdoutIsTTY {
		return "", false
	}
	return "unidentified agent (GT_ROLE/BD_ACTOR unset, not an interactive terminal)", true
}

// holdsGateError decides whether cleanup may continue after a failed holds
// query. Destructive runs fail closed: if we cannot see the holds, we cannot
// prove any removal is safe (gt-2oy). Only a dry run may continue.
func holdsGateError(holdsErr error, dryRun bool) error {
	if holdsErr == nil || dryRun {
		return nil
	}
	return fmt.Errorf("cannot verify database holds (refusing destructive cleanup; retry when beads is reachable, or use --dry-run to inspect): %w", holdsErr)
}

// agentActor returns the agent identity when this process runs as a Gas Town
// agent (GT_ROLE or BD_ACTOR set), or "" for a human at a plain terminal.
func agentActor(getenv func(string) string) string {
	if role := getenv("GT_ROLE"); role != "" {
		return role
	}
	if actor := getenv("BD_ACTOR"); actor != "" {
		return actor
	}
	return ""
}

// cleanupActorLabel names the actor for audit comments.
func cleanupActorLabel() string {
	if actor := agentActor(os.Getenv); actor != "" {
		return actor
	}
	return "human operator"
}

// cleanupAuditor records the audit trail for forced cleanups (gt-87a).
// The authorizing bead carries the durable trail: an INTENT comment before the
// first DROP and a completion comment after, so a crash mid-cleanup cannot
// leave destroyed databases with no record. The town event log additionally
// covers every forced run, including human operators with no --authorized-by
// bead.
type cleanupAuditor struct {
	actor      string
	beadID     string // authorizing bead ("" = none; event log only)
	addComment func(id, text string) error
	logAudit   func(eventType string, payload map[string]interface{})
}

func newCleanupAuditor(townRoot, actor, beadID string) *cleanupAuditor {
	return &cleanupAuditor{
		actor:      actor,
		beadID:     beadID,
		addComment: beads.New(townRoot).AddComment,
		logAudit: func(eventType string, payload map[string]interface{}) {
			// Event log is best-effort by design; the bead comment is the
			// fail-closed record.
			_ = events.LogAuditTo(townRoot, eventType, actor, payload)
		},
	}
}

// recordIntent writes the write-ahead audit record before any database is
// dropped. If the bead comment cannot be written, the cleanup must not
// proceed: a forced destructive run may not outrun its audit trail (gt-87a).
func (a *cleanupAuditor) recordIntent(names []string) error {
	a.logAudit(events.TypeDoltCleanupIntent, map[string]interface{}{
		"authorized_by": a.beadID,
		"databases":     strings.Join(names, ","),
	})
	if a.beadID == "" {
		return nil
	}
	comment := fmt.Sprintf("gt dolt cleanup --force INTENT by %s: removing %d database(s): %s",
		a.actor, len(names), strings.Join(names, ", "))
	if err := a.addComment(a.beadID, comment); err != nil {
		return fmt.Errorf("cannot write intent record to %s — refusing destructive cleanup; the audit trail must precede removal (gt-87a): %w", a.beadID, err)
	}
	return nil
}

// recordCompletion writes the post-removal audit record. It runs even when
// nothing was removed — a failed forced attempt is still audit-worthy. A
// failure to record on the bead is returned so the caller exits non-zero
// instead of completing a forced destructive run with no durable trail.
func (a *cleanupAuditor) recordCompletion(removed, attempted int, names []string) error {
	a.logAudit(events.TypeDoltCleanupDone, map[string]interface{}{
		"authorized_by": a.beadID,
		"removed":       removed,
		"attempted":     attempted,
		"databases":     strings.Join(names, ","),
	})
	if a.beadID == "" {
		return nil
	}
	detail := "none"
	if len(names) > 0 {
		detail = strings.Join(names, ", ")
	}
	comment := fmt.Sprintf("gt dolt cleanup --force by %s: removed %d/%d database(s): %s",
		a.actor, removed, attempted, detail)
	if err := a.addComment(a.beadID, comment); err != nil {
		return fmt.Errorf("removed %d database(s) but failed to record completion on %s — audit trail incomplete (gt-87a): %w", removed, a.beadID, err)
	}
	return nil
}

// checkAgentForceAuthorization enforces the gt-61x guardrail: an agent actor
// may only run a forced cleanup when it records authorization via a bead ID.
func checkAgentForceAuthorization(actor, authorizedBy string) error {
	if authorizedBy != "" {
		return nil
	}
	return fmt.Errorf(`agent actor %q may not run 'gt dolt cleanup --force' without recorded authorization (gt-61x)

Destructive database removal by agents requires an authorization bead:
  1. Get explicit approval from the mayor/overseer (escalate if needed)
  2. Reference the bead that records the decision:
       gt dolt cleanup --force --authorized-by <bead-id>

The removal will be logged as a comment on that bead`, actor)
}

func runDoltList(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	config := doltserver.DefaultConfig(townRoot)
	databases, err := doltserver.ListDatabases(townRoot)
	if err != nil {
		return fmt.Errorf("listing databases: %w", err)
	}

	if len(databases) == 0 {
		fmt.Printf("No rig databases found in %s\n", config.DataDir)
		fmt.Printf("\nInitialize with: %s\n", style.Dim.Render("gt dolt init-rig <name>"))
		return nil
	}

	owners := doltserver.CollectDatabaseOwners(townRoot)
	fmt.Printf("Rig databases in %s:\n\n", config.DataDir)
	for _, db := range databases {
		dbDir := doltserver.RigDatabaseDir(townRoot, db)
		if owner, ok := owners[db]; ok {
			fmt.Printf("  %s (%s)\n    %s\n", style.Bold.Render(db), owner, style.Dim.Render(dbDir))
		} else {
			fmt.Printf("  %s (orphan)\n    %s\n", style.Bold.Render(db), style.Dim.Render(dbDir))
		}
	}

	return nil
}

func runDoltMigrate(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	config := doltserver.DefaultConfig(townRoot)
	if config.IsRemote() {
		return fmt.Errorf("Dolt server is remote (%s) — migration requires local server access", config.HostPort())
	}

	// Check if daemon is running - must stop first to avoid race conditions.
	// The daemon spawns many bd processes via gt status heartbeats. If these
	// run concurrently with migration, race conditions occur between old
	// old and new backends.
	daemonRunning, _, _ := daemon.IsRunning(townRoot)
	if daemonRunning {
		return fmt.Errorf("Gas Town daemon is running. Stop it first with: gt daemon stop\n\nThe daemon spawns bd processes that can race with migration.\nStop the daemon, run migration, then restart it.")
	}

	// Check if Dolt server is running - must stop first
	running, _, _ := doltserver.IsRunning(townRoot)
	if running {
		return fmt.Errorf("Dolt server is running. Stop it first with: gt dolt stop")
	}

	// Find databases to migrate
	migrations := doltserver.FindMigratableDatabases(townRoot)
	if len(migrations) == 0 {
		fmt.Println("No databases found to migrate.")
		return nil
	}

	fmt.Printf("Found %d database(s) to migrate:\n\n", len(migrations))
	for _, m := range migrations {
		sizeStr := dirSizeHuman(m.SourcePath)
		fmt.Printf("  %s (%s)\n", m.SourcePath, sizeStr)
		fmt.Printf("    → %s\n\n", m.TargetPath)
	}

	if doltMigrateDry {
		fmt.Println("Dry run: no changes made.")
		return nil
	}

	// Perform migrations
	for _, m := range migrations {
		fmt.Printf("Migrating %s...\n", m.RigName)
		if err := doltserver.MigrateRigFromBeads(townRoot, m.RigName, m.SourcePath); err != nil {
			return fmt.Errorf("migrating %s: %w", m.RigName, err)
		}
		fmt.Printf("  %s Migrated to %s\n", style.Bold.Render("✓"), m.TargetPath)
	}

	// Update metadata.json for all migrated rigs
	updated, metaErrs := doltserver.EnsureAllMetadata(townRoot)
	if len(updated) > 0 {
		fmt.Printf("\nUpdated metadata.json for: %s\n", strings.Join(updated, ", "))
	}
	for _, err := range metaErrs {
		fmt.Printf("  %s metadata.json update failed: %v\n", style.Dim.Render("⚠"), err)
	}

	fmt.Printf("\n%s Migration complete.\n", style.Bold.Render("✓"))

	// Auto-start the Dolt server to prevent split-brain risk.
	// If bd commands are run before the server starts, they may silently create
	// isolated local databases instead of connecting to the centralized server.
	fmt.Printf("\nStarting Dolt server to prevent split-brain risk...\n")
	if err := doltserver.Start(townRoot); err != nil {
		fmt.Printf("\n%s Could not auto-start Dolt server: %v\n", style.Bold.Render("⚠"), err)
		fmt.Printf("\n%s WARNING: Do NOT run bd commands until the server is started!\n", style.Bold.Render("⚠"))
		fmt.Printf("  Running bd before 'gt dolt start' risks split-brain: bd may create an\n")
		fmt.Printf("  isolated local database instead of connecting to the centralized server.\n")
		fmt.Printf("\n  Start manually with: %s\n", style.Dim.Render("gt dolt start"))
	} else {
		state, _ := doltserver.LoadState(townRoot)
		fmt.Printf("%s Dolt server started (PID %d)\n", style.Bold.Render("✓"), state.PID)

		// Verify the server is actually serving all databases that exist on disk.
		// Dolt silently skips databases with stale manifests after migration,
		// so filesystem discovery and SQL discovery can diverge.
		// Use retry since the server may still be loading databases after Start().
		served, missing, verifyErr := doltserver.VerifyDatabasesWithRetry(townRoot, 5)
		if verifyErr != nil {
			fmt.Printf("  %s Could not verify databases: %v\n", style.Dim.Render("⚠"), verifyErr)
			fmt.Printf("  Migration may be incomplete. Verify manually with: %s\n", style.Dim.Render("gt dolt status"))
			return fmt.Errorf("database verification failed after migration: %w", verifyErr)
		} else if len(missing) > 0 {
			fmt.Printf("\n%s Some databases exist on disk but are NOT served by Dolt:\n", style.Bold.Render("⚠"))
			for _, db := range missing {
				fmt.Printf("  - %s\n", db)
			}
			fmt.Printf("\n  Served databases: %v\n", served)
			fmt.Printf("\n  This usually means the database has a stale manifest from migration.\n")
			fmt.Printf("  To fix, try:\n")
			fmt.Printf("    1. Stop the server:  %s\n", style.Dim.Render("gt dolt stop"))
			fmt.Printf("    2. Repair the DB:    %s\n", style.Dim.Render("cd ~/gt/.dolt-data/<db> && dolt fsck --repair"))
			fmt.Printf("    3. Restart:           %s\n", style.Dim.Render("gt dolt start"))
			return fmt.Errorf("migration incomplete: %d database(s) exist on disk but are not served: %v", len(missing), missing)
		} else {
			fmt.Printf("  %s All %d databases verified as served\n", style.Bold.Render("✓"), len(served))
		}
	}

	return nil
}

// dirSizeHuman returns a human-readable size string for a directory tree.
func dirSizeHuman(path string) string {
	var total int64
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip errors
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return formatBytes(total)
}

func runDoltFixMetadata(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	updated, errs := doltserver.EnsureAllMetadata(townRoot)

	if len(updated) > 0 {
		fmt.Printf("%s Updated metadata.json for %d rig(s):\n", style.Bold.Render("✓"), len(updated))
		for _, name := range updated {
			fmt.Printf("  - %s\n", name)
		}
	}

	if len(errs) > 0 {
		fmt.Println()
		for _, err := range errs {
			fmt.Printf("  %s %v\n", style.Dim.Render("⚠"), err)
		}
	}

	if len(updated) == 0 && len(errs) == 0 {
		fmt.Println("No rig databases found. Nothing to update.")
	}

	return nil
}

// testDatabaseGlobs is a shell glob per test-database prefix, for the manual
// cleanup hint.
func testDatabaseGlobs() string {
	globs := testdb.Prefixes()
	for i, p := range globs {
		globs[i] = p + "*"
	}
	return strings.Join(globs, " ")
}
