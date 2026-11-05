package cmd

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beadsql"
	gtconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/doltpause"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/ui"
	"github.com/steveyegge/gastown/internal/util"
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
	doltCmd.AddCommand(doltLatencyCmd)
	doltCmd.AddCommand(doltTopCmd)
	doltCmd.AddCommand(newDoltCompareCmd(doltCompareRealDeps()))

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
	container, err := config.ExternalContainerSetting()
	if err != nil {
		return err
	}
	if container != "" {
		// The container serves its own databases, so the host filesystem guard
		// below says nothing about this town, and the endpoint may well be a
		// remote docker host: gt still starts the container it named (gt-xeb1x).
		if err := doltserver.Start(townRoot); err != nil {
			return err
		}
		fmt.Printf("%s Dolt container %s is running (port %d)\n",
			style.Bold.Render("✓"), container, config.Port)
		fmt.Printf("  Connection: %s\n", style.Dim.Render(doltserver.GetConnectionString(townRoot)))
		return nil
	}
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
	// Imposter detection names whatever holds the port and signals it. In
	// container mode that holder is the container's own port forward, not an
	// imposter, and gt sends no signal in this mode at all (gt-xeb1x).
	if container, err := config.ExternalContainerSetting(); err != nil {
		return err
	} else if container != "" {
		return fmt.Errorf("dolt.external_container = %q: a container's port is held by its own docker forward, and gt never signals a process it did not start — nothing to kill", container)
	}
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
	container, err := config.ExternalContainerSetting()
	if err != nil {
		return err
	}
	if container != "" {
		if err := doltserver.Stop(townRoot); err != nil {
			return err
		}
		fmt.Printf("%s Dolt container %s stopped\n", style.Bold.Render("✓"), container)
		return nil
	}
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
	container, err := config.ExternalContainerSetting()
	if err != nil {
		return err
	}
	if container != "" {
		// Container mode has no pid to stop, no imposter to kill and no host
		// data dir to inspect: the restart is docker stop then docker start
		// through internal/doltserver (gt-xeb1x).
		if err := doltserver.Restart(townRoot); err != nil {
			return fmt.Errorf("restart failed: %w", err)
		}
		fmt.Printf("%s Dolt container %s restarted (port %d)\n",
			style.Bold.Render("✓"), container, config.Port)
		fmt.Printf("  Connection: %s\n", style.Dim.Render(doltserver.GetConnectionString(townRoot)))
		return nil
	}
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

// doltStatusReads is where `gt dolt status` reads the town and the server: the
// town root, whether the server answers, its metrics, the databases it serves,
// the archive reader the database lines count through, and the stray-server
// warning. Production passes the real calls; a test passes fakes, so the report
// runs with no live Dolt, no Docker container and no host process scan
// (gt-w1ekf).
type doltStatusReads struct {
	townRoot           func() (string, error)
	isRunning          func(townRoot string) (bool, int, error)
	healthMetrics      func(townRoot string) *doltserver.HealthMetrics
	verifyDatabases    func(townRoot string) (served, missing []string, err error)
	servedDatabases    func(config *doltserver.Config) ([]string, error)
	archiveReader      func(townRoot string, config *doltserver.Config, databases []string) archiveBehindReader
	beadsRuntimeConfig func(w io.Writer, townRoot string)
	strayWarning       func(townRoot string)
}

// liveDoltStatusReads is the status command's production reads.
func liveDoltStatusReads() doltStatusReads {
	return doltStatusReads{
		townRoot:           workspace.FindFromCwdOrError,
		isRunning:          doltserver.IsRunning,
		healthMetrics:      doltserver.GetHealthMetrics,
		verifyDatabases:    doltserver.VerifyDatabases,
		servedDatabases:    servedDatabasesOverSQL,
		archiveReader:      newStatusArchiveReader,
		beadsRuntimeConfig: printBeadsRuntimeConfig,
		strayWarning:       printStrayWorkspaceServerWarning,
	}
}

func runDoltStatus(cmd *cobra.Command, args []string) error {
	return runDoltStatusWith(os.Stdout, liveDoltStatusReads())
}

// runDoltStatusWith runs the status report, writing it to w. A test passes a
// bytes.Buffer and stub reads, so it never swaps the process stdout (gt-w1ekf).
func runDoltStatusWith(w io.Writer, reads doltStatusReads) error {
	townRoot, err := reads.townRoot()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	config := doltserver.DefaultConfig(townRoot)
	// An unusable dolt.external_container is named here rather than read as a
	// host server, and before the status probe, which would report the same
	// setting error as "checking server status" (gt-xeb1x).
	container, err := config.ExternalContainerSetting()
	if err != nil {
		return err
	}

	running, pid, err := reads.isRunning(townRoot)
	if err != nil {
		// dolt.external_container was resolved above and host and remote modes
		// report no error, so this is the container's failed SQL ping: report
		// it as the status it is, with the failure named, rather than failing
		// the command (gt-0v0bg).
		fmt.Fprintf(w, "%s Dolt server is %s (container %s)\n",
			style.Dim.Render("○"), "not running", container)
		fmt.Fprintf(w, "  Ping: %s\n", style.Dim.Render(err.Error()))
		fmt.Fprintf(w, "  Connection: %s\n", doltserver.GetConnectionString(townRoot))
		reads.beadsRuntimeConfig(w, townRoot)
		printArchiveCheckUnavailable(w, archiveUnavailableContainerPing)
		return nil
	}

	if p := doltpause.Current(townRoot, time.Now()); p != nil {
		fmt.Fprintf(w, "%s %s\n", style.Bold.Render("⏸"), p.Message())
	}

	// A container's Dolt has no pid here and no state file to read: the SQL
	// ping IsRunning made is the status, and the metrics come from the server
	// over SQL (gt-xeb1x).
	if container != "" {
		if running {
			fmt.Fprintf(w, "%s Dolt server is %s (container %s)\n",
				style.Bold.Render("●"), style.Bold.Render("running"), container)
			printResourceMetrics(w, reads.healthMetrics(townRoot))

			// The host branch reads the served databases through
			// doltserver.VerifyDatabases, which runs the dolt CLI on the host;
			// in container mode the container owns that CLI, so the list is
			// read over the same SQL connection the ping used (gt-w1ekf).
			databases, listErr := reads.servedDatabases(config)
			if listErr != nil {
				fmt.Fprintf(w, "  %s Listing the databases the container serves failed: %v\n",
					style.Bold.Render("!"), listErr)
			}
			printDatabaseArchiveBlock(w, townRoot,
				reads.archiveReader(townRoot, config, databases), databases,
				"Databases (served by the container):")
		} else {
			fmt.Fprintf(w, "%s Dolt server is %s (container %s)\n",
				style.Dim.Render("○"), "not running", container)
			printArchiveCheckUnavailable(w, archiveUnavailableContainerStopped)
		}
		fmt.Fprintf(w, "  Connection: %s\n", doltserver.GetConnectionString(townRoot))
		reads.beadsRuntimeConfig(w, townRoot)
		return nil
	}

	if config.IsRemote() {
		if running {
			fmt.Fprintf(w, "%s Dolt server is %s (remote: %s)\n",
				style.Bold.Render("●"),
				style.Bold.Render("reachable"),
				config.HostPort())
		} else {
			fmt.Fprintf(w, "%s Dolt server is %s (remote: %s)\n",
				style.Dim.Render("○"),
				"not reachable",
				config.HostPort())
		}
		fmt.Fprintf(w, "  Connection: %s\n", doltserver.GetConnectionString(townRoot))
		reads.beadsRuntimeConfig(w, townRoot)
		if running {
			metrics := reads.healthMetrics(townRoot)
			fmt.Fprintf(w, "\n  %s\n", style.Bold.Render("Resource Metrics:"))
			fmt.Fprintf(w, "    Query latency: %v\n", metrics.QueryLatency.Round(time.Millisecond))
			fmt.Fprintf(w, "    Connections:   %d / %d (%.0f%%)\n",
				metrics.Connections, metrics.MaxConnections, metrics.ConnectionPct)
			if metrics.ReadOnly {
				fmt.Fprintf(w, "\n  %s %s\n",
					style.Bold.Render("!!!"),
					style.Bold.Render("SERVER IS READ-ONLY — contact the remote server admin"))
			}
		}
		printArchiveCheckUnavailable(w, archiveUnavailableNoDatabases)
		reads.strayWarning(townRoot)
		return nil
	}

	if running {
		fmt.Fprintf(w, "%s Dolt server is %s (PID %d)\n",
			style.Bold.Render("●"),
			style.Bold.Render("running"),
			pid)

		// Query the live server for the authoritative database list. The
		// state file's databases[] is a snapshot taken at server start and
		// never updated, so DBs created/dropped since then would render
		// stale (gt-cs6).
		served, missing, verifyErr := reads.verifyDatabases(townRoot)

		// Load state for more details
		state, err := doltserver.LoadState(townRoot)
		if err == nil && !state.StartedAt.IsZero() {
			fmt.Fprintf(w, "  Started: %s\n", state.StartedAt.Format("2006-01-02 15:04:05"))
			fmt.Fprintf(w, "  Port: %d\n", state.Port)
			fmt.Fprintf(w, "  Data dir: %s\n", state.DataDir)
			databases, dbLabel := statusDatabases(served, verifyErr, state.Databases)
			printDatabaseArchiveBlock(w, townRoot,
				reads.archiveReader(townRoot, config, databases), databases, dbLabel)
			fmt.Fprintf(w, "  Connection: %s\n", doltserver.GetConnectionString(townRoot))
			reads.beadsRuntimeConfig(w, townRoot)
		} else {
			// No state, so no database line can be built: the flatten
			// runbook's archive check must not read that as clean (gt-w1ekf).
			printArchiveCheckUnavailable(w, archiveUnavailableNoDatabases)
		}

		// Resource metrics
		metrics := reads.healthMetrics(townRoot)
		printResourceMetrics(w, metrics)

		// Verify all filesystem databases are actually served.
		if verifyErr != nil {
			fmt.Fprintf(w, "\n  %s Database verification failed: %v\n", style.Bold.Render("!"), verifyErr)
		} else if len(missing) > 0 {
			fmt.Fprintf(w, "\n  %s %s\n", style.Bold.Render("!!!"),
				style.Bold.Render("MISSING DATABASES — exist on disk but not served:"))
			for _, db := range missing {
				fmt.Fprintf(w, "    - %s\n", db)
			}
			fmt.Fprintf(w, "  Try: cd ~/gt/.dolt-data/<db> && dolt fsck --repair\n")
		}

		// Check for orphaned databases
		orphans, orphanErr := doltserver.FindOrphanedDatabases(townRoot)
		if orphanErr == nil && len(orphans) > 0 {
			fmt.Fprintf(w, "\n  %s %d orphaned database(s) (not referenced by any rig):\n",
				style.Bold.Render("!"), len(orphans))
			for _, o := range orphans {
				fmt.Fprintf(w, "    - %s (%s)\n", o.Name, formatBytes(o.SizeBytes))
			}
			fmt.Fprintf(w, "  Clean up with: %s\n", style.Dim.Render("gt dolt cleanup"))
		}

		if len(metrics.Warnings) > 0 {
			fmt.Fprintf(w, "\n  %s\n", style.Bold.Render("Warnings:"))
			for _, warning := range metrics.Warnings {
				fmt.Fprintf(w, "    %s %s\n", style.Bold.Render("!"), warning)
			}
		}
	} else {
		fmt.Fprintf(w, "%s Dolt server is %s\n",
			style.Dim.Render("○"),
			"not running")

		// List available databases
		databases, _ := doltserver.ListDatabases(townRoot)
		if len(databases) == 0 {
			fmt.Fprintf(w, "\n%s No rig databases found in %s\n",
				style.Bold.Render("!"),
				config.DataDir)
			fmt.Fprintf(w, "  Initialize with: %s\n", style.Dim.Render("gt dolt init-rig <name>"))
		} else {
			fmt.Fprintf(w, "\nAvailable databases in %s:\n", config.DataDir)
			owners := doltserver.CollectDatabaseOwners(townRoot)
			for _, db := range databases {
				if owner, ok := owners[db]; ok {
					fmt.Fprintf(w, "  - %-20s (%s)\n", db, owner)
				} else {
					fmt.Fprintf(w, "  - %s\n", db)
				}
			}
			fmt.Fprintf(w, "\nStart with: %s\n", style.Dim.Render("gt dolt start"))
		}
		printArchiveCheckUnavailable(w, archiveUnavailableHostStopped)
	}

	reads.strayWarning(townRoot)
	return nil
}

// printResourceMetrics writes the Resource Metrics block of `gt dolt status`
// for a running local server: latency, connections, disk usage and the
// server's physical memory footprint.
func printResourceMetrics(w io.Writer, metrics *doltserver.HealthMetrics) {
	fmt.Fprintf(w, "\n  %s\n", style.Bold.Render("Resource Metrics:"))
	fmt.Fprintf(w, "    Query latency: %v\n", metrics.QueryLatency.Round(time.Millisecond))
	fmt.Fprintf(w, "    Connections:   %d / %d (%.0f%%)\n",
		metrics.Connections, metrics.MaxConnections, metrics.ConnectionPct)
	fmt.Fprintf(w, "    Disk usage:    %s\n", metrics.DiskUsageHuman)
	fmt.Fprintf(w, "    Memory:        %s\n", memoryFootprintLine(metrics))
	if metrics.ReadOnly {
		fmt.Fprintf(w, "\n  %s %s\n",
			style.Bold.Render("!!!"),
			style.Bold.Render("SERVER IS READ-ONLY — run 'gt dolt restart'"))
	}
}

// memoryFootprintLine renders the server's physical memory footprint for the
// status output, beside the disk usage. A zero footprint reads as
// unavailable: the platform has no vmmap, the server has no PID, or vmmap
// could not read the process (gt-v1zyv).
func memoryFootprintLine(metrics *doltserver.HealthMetrics) string {
	if metrics == nil || metrics.FootprintBytes <= 0 {
		return "unavailable"
	}
	if metrics.FootprintPeakBytes > 0 {
		return fmt.Sprintf("%s (peak %s)",
			formatBytes(metrics.FootprintBytes), formatBytes(metrics.FootprintPeakBytes))
	}
	return formatBytes(metrics.FootprintBytes)
}

// printStrayWorkspaceServerWarning names any running Dolt server a beads
// workspace started for itself on a port other than the town's, and is silent
// when there is none.
//
// Such a server serves that workspace alone, and the port file beside its
// config sends later bd calls there, where they fail with "database not found"
// while the town's server is healthy (gt-4js4f). This prints, and never stops a
// server.
func printStrayWorkspaceServerWarning(townRoot string) {
	servers, err := util.FindDoltServers()
	if err != nil {
		return // the status printed above stands; a scan failure is not a finding
	}

	townPort := doltserver.DefaultConfig(townRoot).Port
	strays := strayWorkspaceServerLabels(servers, townPort)
	if len(strays) == 0 {
		return
	}

	townPortText := "none"
	if townPort > 0 {
		townPortText = strconv.Itoa(townPort)
	}
	fmt.Printf("\n  %s %d stray Dolt server(s) started by a beads workspace: %s — not the town's (port %s); stop a stray with 'kill <pid>'\n",
		style.Bold.Render("!"), len(strays), strings.Join(strays, ", "), townPortText)
}

// strayWorkspaceServerLabels names each running Dolt server that belongs to a
// beads workspace rather than the town: one "PID <pid> on port <port>" per
// server whose --config lies under a .beads directory, except one pointed at
// the town's own port (which is what the town wants).
func strayWorkspaceServerLabels(servers []util.DoltServerProcess, townPort int) []string {
	var labels []string
	for _, s := range servers {
		if _, _, ok := util.BeadsDirFromConfigPath(s.ConfigPath); !ok {
			continue // the town's own server runs from .dolt-data, not a .beads dir
		}
		// A workspace pointed at the town's own port is what the town wants;
		// an unreadable port (0) is not the town's, so it is named too.
		if townPort > 0 && s.Port == townPort {
			continue
		}
		port := "unknown"
		if s.Port > 0 {
			port = strconv.Itoa(s.Port)
		}
		labels = append(labels, fmt.Sprintf("PID %d on port %s", s.PID, port))
	}
	return labels
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

// databaseStatusLine renders one database's line in `gt dolt status`, with the
// history archive note when it has one.
func databaseStatusLine(db, owner, note string) string {
	if note != "" {
		note = "; " + note
	}
	if owner != "" {
		return fmt.Sprintf("    - %-20s (%s)%s", db, owner, note)
	}
	return fmt.Sprintf("    - %s%s", db, note)
}

// The reasons `gt dolt status` names when it can produce no database line at
// all. Step 2 of the flatten runbook stops on that line, so an empty result has
// to say why it is empty: silence would pass the pre-flatten check without
// checking anything (gt-w1ekf).
const (
	archiveUnavailableContainerPing    = "container ping failed"
	archiveUnavailableContainerStopped = "container not running"
	archiveUnavailableHostStopped      = "host server not running"
	archiveUnavailableNoDatabases      = "no databases listed"
)

// archiveCheckUnavailableLine is the status line that tells the flatten
// runbook this status proved nothing about the archive.
func archiveCheckUnavailableLine(reason string) string {
	return "ARCHIVE CHECK UNAVAILABLE: " + reason
}

// printArchiveCheckUnavailable writes the line naming why the status could not
// list any database.
func printArchiveCheckUnavailable(w io.Writer, reason string) {
	fmt.Fprintln(w, archiveCheckUnavailableLine(reason))
}

// printDatabaseArchiveBlock writes the status's database lines: the list's
// source label, then one databaseStatusLine per database carrying its archive
// note. A list with no database prints the unavailable line instead, so a
// status that listed none can never read as a clean pre-flatten check
// (gt-w1ekf).
func printDatabaseArchiveBlock(w io.Writer, townRoot string, reader archiveBehindReader, databases []string, dbLabel string) {
	if len(databases) == 0 {
		printArchiveCheckUnavailable(w, archiveUnavailableNoDatabases)
		return
	}
	owners := doltserver.CollectDatabaseOwners(townRoot)
	notes, noteErr := archiveNotes(townRoot, reader, databases)
	if noteErr != nil {
		fmt.Fprintf(w, "%s", archiveUnreadableLine(noteErr))
	}
	fmt.Fprintf(w, "  %s\n", dbLabel)
	for _, db := range databases {
		fmt.Fprintln(w, databaseStatusLine(db, owners[db], notes[db]))
	}
}

// servedDatabasesOverSQL lists the databases the Dolt server serves, reading
// SHOW DATABASES over the MySQL driver. The container branch of `gt dolt
// status` uses it because the host branch's doltserver.VerifyDatabases runs the
// dolt CLI on the host, and a container town's Dolt CLI lives in the container.
// It reuses readDatabaseNames, the same SQL listing `gt dolt compare` uses, so
// the Dolt/MySQL internal catalogs stay out of the list.
func servedDatabasesOverSQL(config *doltserver.Config) ([]string, error) {
	return servedDatabasesOverSQLWith(config, nil)
}

// servedDatabasesOverSQLWith is servedDatabasesOverSQL with the pool opener a
// test injects; a nil opener opens the MySQL driver.
func servedDatabasesOverSQLWith(config *doltserver.Config, openDB func(dsn string) (*sql.DB, error)) ([]string, error) {
	// TCP only: a container answers on the town's published port, and a unix
	// socket on the host at that port belongs to another server (gt-w1ekf).
	dsn := formatDoltDSN(config.UserDSN(), "tcp", config.HostPort(), "", dsnOpts{
		Timeout:     "5s",
		ReadTimeout: "10s",
	})
	open := openDB
	if open == nil {
		open = func(dsn string) (*sql.DB, error) { return sql.Open("mysql", dsn) }
	}
	db, err := open(dsn)
	if err != nil {
		return nil, fmt.Errorf("opening the SQL connection: %w", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("connecting to the Dolt server: %w", err)
	}
	defer conn.Close()

	names, err := readDatabaseNames(ctx, conn)
	if err != nil {
		return nil, fmt.Errorf("listing the served databases: %w", err)
	}
	return names, nil
}

// newStatusArchiveReader builds the reader the status's archive notes count
// through. A container's Dolt answers on its published TCP port, so the reader
// is told not to fall back to a unix socket on the host, where another server
// may hold the same port (gt-w1ekf).
func newStatusArchiveReader(townRoot string, config *doltserver.Config, databases []string) archiveBehindReader {
	return doltArchiveReader{
		config:         config,
		tcpOnly:        config.ExternalContainer != "",
		archivedHashes: archivedHashesLoader(townRoot, databases),
	}
}

// archiveNotes returns the history archive notes for every listed database that
// has a watermark: "archived through <date>, N commits behind" for its commits
// and "events archived through <created_at>, N events behind" for its events.
// Counts are read through reader. An unreadable watermark file is an error so
// the caller can say so; a database whose count cannot be read carries the
// error instead of a zero that reads as caught up.
func archiveNotes(townRoot string, reader archiveBehindReader, databases []string) (map[string]string, error) {
	wms, err := daemon.LoadArchiveWatermarks(townRoot)
	if err != nil {
		// No database can be read as caught up when the watermarks are
		// unreadable, and a line that simply went missing would let the
		// pre-flatten check pass over it: give every database the state itself
		// as its note, and hand the error back for the header line.
		notes := make(map[string]string, len(databases))
		for _, db := range databases {
			notes[db] = fmt.Sprintf("archive state unreadable: %v", err)
		}
		return notes, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return archiveBehindNotes(ctx, reader, wms, databases), nil
}

// archivedHashesLoader returns the loader the pre-flatten check reads a
// database's archived commit hashes through. The directory is the daemon
// config's maintenance.archive_dir; a config that cannot be read, or a setting
// left unset, is an error rather than an empty set, because a check that
// cannot see the archive cannot call any commit safe (gt-7nxj4).
//
// A status call builds one loader, and the archive is read once: the first ask
// lists the directory and opens each listed database's files, and every later
// ask is served from that reading. One status call therefore reads each
// database's archived-hash set once, however many notes consult it (gt-3w3hk).
func archivedHashesLoader(townRoot string, databases []string) func(db string) (map[string]struct{}, error) {
	dir, dirErr := archiveDirForStatus(townRoot)
	var (
		loaded  bool
		sets    map[string]map[string]struct{}
		loadErr error
	)
	return func(db string) (map[string]struct{}, error) {
		if dirErr != nil {
			return nil, dirErr
		}
		if dir == "" {
			return nil, fmt.Errorf("%s is unset: the archive cannot say which commits it holds", gtconfig.MaintenanceArchiveDirKey)
		}
		if !loaded {
			sets, loadErr = daemon.LoadArchivedCommitHashesFor(dir, databases)
			loaded = true
		}
		if loadErr != nil {
			return nil, loadErr
		}
		set, ok := sets[db]
		if !ok {
			return nil, fmt.Errorf("database %q was not listed for the archive check", db)
		}
		return set, nil
	}
}

// archiveDirForStatus reads maintenance.archive_dir from the daemon config
// once, for every database's check in one status call.
func archiveDirForStatus(townRoot string) (string, error) {
	patrol, err := daemon.ReadPatrolConfig(townRoot)
	if err != nil {
		return "", fmt.Errorf("reading the daemon config: %w", err)
	}
	if patrol == nil || patrol.Patrols == nil {
		return "", nil
	}
	return patrol.Patrols.ScheduledMaintenance.ParseArchiveDir()
}

// archiveUnreadableLine is the header `gt dolt status` prints when the
// watermarks cannot be read. Each database's own line then carries the same
// error, so no database reads as if the archive covered it.
func archiveUnreadableLine(err error) string {
	return fmt.Sprintf("  %s Archive watermarks unreadable: %v\n", style.Bold.Render("!"), err)
}

// archiveBehindReader counts the rows newer than a database's archive
// watermarks.
type archiveBehindReader interface {
	commitsBehind(ctx context.Context, db string, wm daemon.ArchiveWatermark) (int64, error)
	eventsBehind(ctx context.Context, db string, wm daemon.EventsArchiveWatermark) (int64, error)
}

// archiveBehindNotes renders the archive notes for every listed database. One
// with no watermark still gets a line, saying it is not archived yet and how
// far behind it is: omitting it would let the pre-flatten check ("every
// database reads 0 commits behind") pass over a database the archive has never
// covered (gt-7nxj4).
func archiveBehindNotes(ctx context.Context, reader archiveBehindReader, wms daemon.ArchiveWatermarks, databases []string) map[string]string {
	notes := make(map[string]string, len(databases))
	for _, db := range databases {
		notes[db] = strings.Join([]string{
			commitArchiveNote(ctx, reader, db, wms.Commits[db]),
			eventsArchiveNote(ctx, reader, db, wms.Events[db]),
		}, "; ")
	}
	return notes
}

// commitArchiveNote renders one database's commit archive note.
func commitArchiveNote(ctx context.Context, reader archiveBehindReader, db string, wm daemon.ArchiveWatermark) string {
	behind, err := reader.commitsBehind(ctx, db, wm)
	if err != nil {
		return fmt.Sprintf("archive behind count unavailable: %v", err)
	}
	if wm.Date.IsZero() {
		return fmt.Sprintf("not archived yet, %s", commitsBehindText(behind))
	}
	return fmt.Sprintf("archived through %s, %s", wm.Date.UTC().Format("2006-01-02"), commitsBehindText(behind))
}

// eventsArchiveNote renders one database's events archive note. The count
// covers the archive's lookback window and no more, so the note names the
// window's start: a row older than it is the archive's accepted limit, and the
// line must not read as if the whole table were archived.
func eventsArchiveNote(ctx context.Context, reader archiveBehindReader, db string, wm daemon.EventsArchiveWatermark) string {
	behind, err := reader.eventsBehind(ctx, db, wm)
	if err != nil {
		return fmt.Sprintf("events behind count unavailable: %v", err)
	}
	if wm.CreatedAt == "" {
		if behind < 0 {
			return "events not archived yet, at least one event behind (count bounded: existence check)"
		}
		return fmt.Sprintf("events not archived yet, %s", eventsBehindText(behind))
	}
	since, err := wm.Since()
	if err != nil {
		return fmt.Sprintf("events behind count unavailable: %v", err)
	}
	return fmt.Sprintf("events archived through %s, %s since %s", wm.CreatedAt, eventsBehindText(behind), since)
}

// eventsBehindUncounted is eventsBehind's answer when there is no watermark:
// the table holds at least one row, but the reader did not count them. A full
// COUNT(*) over an events table the archive has never covered is what the
// status must not run, so that path is an existence check and the note says so.
const eventsBehindUncounted int64 = -1

// commitsBehindText renders how many commits an archive has not written out.
func commitsBehindText(n int64) string {
	if n == 1 {
		return "1 commit behind"
	}
	return fmt.Sprintf("%d commits behind", n)
}

// eventsBehindText renders how many events rows an archive has not written out.
func eventsBehindText(n int64) string {
	if n == 1 {
		return "1 event behind"
	}
	return fmt.Sprintf("%d events behind", n)
}

// doltArchiveReader counts archive lag against the live server.
type doltArchiveReader struct {
	config *doltserver.Config
	// tcpOnly makes every count connect over TCP rather than a unix socket.
	// Container mode sets it: the container answers on the town's published
	// port, and a host socket at that port belongs to another server (gt-w1ekf).
	tcpOnly bool
	// openDB opens the pool a count runs on; nil opens with the MySQL driver.
	// A test injects a fake driver so the failure paths are reachable without
	// a server.
	openDB func(dsn string) (*sql.DB, error)
	// archivedHashes returns the commit hashes db's archive files hold. A nil
	// loader cannot prove any commit is archived, so every commit counts as
	// behind rather than a zero that reads as safe.
	archivedHashes func(db string) (map[string]struct{}, error)
}

// open opens the read-only pool one count runs on.
func (r doltArchiveReader) open(dsn string) (*sql.DB, error) {
	if r.openDB != nil {
		return r.openDB(dsn)
	}
	return sql.Open("mysql", dsn)
}

// dsn renders the connection string one count runs on. tcpOnly selects the
// town's published TCP port without probing a host unix socket, which in
// container mode would be a different server's (gt-w1ekf).
func (r doltArchiveReader) dsn(db string) string {
	opts := dsnOpts{
		Timeout:      "5s",
		ReadTimeout:  "10s",
		WriteTimeout: "10s",
	}
	if r.tcpOnly {
		return formatDoltDSN(r.config.UserDSN(), "tcp", r.config.HostPort(), db, opts)
	}
	return buildDoltDSNFromConfig(r.config, db, opts)
}

// commitsBehind counts the hashes in db's dolt_log that no archive file holds:
// the pre-flatten check. It compares every log hash with the set the archive
// keeps for the database, so a commit dated before the watermark's cursor is
// counted too — a date cursor cannot see a backdated or out-of-order commit,
// and reading one as caught up before a flatten loses it (gt-7nxj4). Zero
// means every commit in the log is archived; an unreadable set is an error,
// never a zero.
func (r doltArchiveReader) commitsBehind(ctx context.Context, db string, wm daemon.ArchiveWatermark) (int64, error) {
	if !dbNameRe.MatchString(db) {
		return 0, fmt.Errorf("database name %q is not a plain identifier — refusing to query it", db)
	}
	if err := wm.Validate(); err != nil {
		// A watermark the archive itself would refuse cannot read as safe
		// either; counting it as caught up would be the lie this guards.
		return 0, err
	}
	if r.archivedHashes == nil {
		return 0, fmt.Errorf("database %q has no archive to check its commits against", db)
	}
	archived, err := r.archivedHashes(db)
	if err != nil {
		return 0, fmt.Errorf("database %q: reading its archived commit hashes: %w", db, err)
	}
	dsn := r.dsn(db)
	conn, err := r.open(dsn)
	if err != nil {
		return 0, fmt.Errorf("database %q not readable: %w", db, err)
	}
	defer conn.Close()

	rows, err := conn.QueryContext(ctx, "SELECT commit_hash FROM dolt_log")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var missing int64
	for rows.Next() {
		var hash string
		if err := rows.Scan(&hash); err != nil {
			return 0, err
		}
		if _, ok := archived[hash]; !ok {
			missing++
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return missing, nil
}

// eventsBehind counts db's events rows the archive has not written out, over
// the same lookback window the archive reads: rows with created_at at or
// after the watermark's date minus the window, minus the ids the watermark
// already holds. A watermark the query cannot compare is refused rather than
// matched against no row.
//
// With no watermark the reader does not count the table: it asks whether the
// table holds any row and returns eventsBehindUncounted when it does, because a
// full COUNT(*) over an events table the archive has never covered is exactly
// the read the status must not run. An empty table is zero.
//
// The count is the archive's own window, not the whole table: a row older
// than the window is the archive's documented accepted limit (eventsArchiveLookback),
// so the status says the count is windowed rather than calling the table
// complete.
func (r doltArchiveReader) eventsBehind(ctx context.Context, db string, wm daemon.EventsArchiveWatermark) (int64, error) {
	if err := wm.Validate(); err != nil {
		return 0, err
	}
	query, err := eventsBehindQuery(db, wm)
	if err != nil {
		return 0, err
	}
	if !dbNameRe.MatchString(db) {
		return 0, fmt.Errorf("database name %q is not a plain identifier — refusing to query it", db)
	}
	dsn := r.dsn(db)
	conn, err := r.open(dsn)
	if err != nil {
		return 0, fmt.Errorf("database %q not readable: %w", db, err)
	}
	defer conn.Close()

	if wm.CreatedAt == "" {
		var probe int
		err := conn.QueryRowContext(ctx, query).Scan(&probe)
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil // no row at all: nothing is behind
		}
		if err != nil {
			return 0, err
		}
		return eventsBehindUncounted, nil
	}

	rows, err := conn.QueryContext(ctx, query)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	return daemon.UnarchivedEvents(rows, wm.ArchivedIDs())
}

// eventsBehindQuery renders the count's SELECT: an existence check with no
// watermark (whether the table holds any row, never a COUNT(*)), or the lookback
// window's rows when there is one. Both statements are declared in beadsql and
// named here so an engine test can run the exact text the status sends.
func eventsBehindQuery(db string, wm daemon.EventsArchiveWatermark) (string, error) {
	if wm.CreatedAt == "" {
		return beadsql.TableProbe("events").Inline()
	}
	since, err := wm.Since()
	if err != nil {
		return "", err
	}
	return beadsql.BackupEvents(db, since).Inline()
}

func currentBeadsRuntimeConfig() (doltserver.BeadsRuntimeConfig, bool) {
	cwd, err := os.Getwd()
	if err != nil {
		return doltserver.BeadsRuntimeConfig{}, false
	}
	return doltserver.ReadBeadsRuntimeConfig(beads.ResolveBeadsDir(cwd))
}

func printBeadsRuntimeConfig(w io.Writer, townRoot string) {
	cfg, ok := currentBeadsRuntimeConfig()
	if !ok {
		return
	}
	fmt.Fprintf(w, "  Beads client: %s\n", beadsClientSummary(cfg))
	if hint := beadsScopeHint(cfg.Database, townRoot); hint != "" {
		fmt.Fprint(w, hint)
	}
}

// beadsClientSummary is the "Beads client" status line. The endpoint is
// joined with net.JoinHostPort so an IPv6 host prints bracketed ([::1]:3307).
func beadsClientSummary(cfg doltserver.BeadsRuntimeConfig) string {
	parts := []string{"server metadata"}
	if cfg.Database != "" {
		parts = append(parts, "database "+cfg.Database)
	}
	if cfg.Host != "" && cfg.Port > 0 {
		parts = append(parts, net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	}
	if cfg.Source != "" {
		parts = append(parts, "from "+cfg.Source)
	}
	return strings.Join(parts, ", ")
}

func beadsScopeHint(database, townRoot string) string {
	if database != "hq" {
		return ""
	}

	return fmt.Sprintf("    Gas Town town beads use database hq. Use `bd -C %s <cmd>` for hq-* beads; do not use `bd --global`, which targets Beads' beads_global database.\n", gtconfig.ShellQuote(townRoot))
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

	return doltInitRig(os.Stdout, townRoot, args[0], doltserver.InitRig)
}

// doltInitRig initializes rigName's database in townRoot and writes the report
// to out, so a test drives the command without moving the process's directory.
// initRig is doltserver.InitRig in the command; a test replaces it because a
// town whose Dolt is on another host would otherwise reach the network.
func doltInitRig(out io.Writer, townRoot, rigName string, initRig func(townRoot, rigName string) (doltserver.InitRigResult, error)) error {
	result, err := initRig(townRoot, rigName)
	if err != nil {
		return err
	}

	config := doltserver.DefaultConfig(townRoot)

	if result.SkipReason != "" {
		// The rig keeps its beads in the tracker, so it has no database here and
		// no location to report: say why instead of "already exists"
		// (gt-p85pw.102).
		fmt.Fprintln(out, result.SkipReason)
		return nil
	}

	// A town whose Dolt is on another host keeps its rig databases there, so
	// the location is that server's host:port. The path under .dolt-data names
	// a directory this town never had (gt-pbp1k). Remote is the path InitRig
	// took, not a second reading of the config, so the report describes the
	// database this run touched (gt-l5s2m).
	remote := result.Remote
	location := doltserver.RigDatabaseDir(townRoot, rigName)
	if remote {
		location = config.HostPort()
	}

	if !result.Created {
		fmt.Fprintf(out, "%s Rig database %q already exists (no-op)\n", style.Bold.Render("✓"), rigName)
		fmt.Fprintf(out, "  Location: %s\n", location)
		return nil
	}

	fmt.Fprintf(out, "%s Initialized rig database %q\n", style.Bold.Render("✓"), rigName)
	fmt.Fprintf(out, "  Location: %s\n", location)
	if remote {
		fmt.Fprintf(out, "  Data dir: %s\n", style.Dim.Render("none (databases live on the remote Dolt)"))
	} else {
		fmt.Fprintf(out, "  Data dir: %s\n", config.DataDir)
	}

	if result.ServerWasRunning {
		fmt.Fprintf(out, "  Server: %s\n", style.Bold.Render("database registered with running server"))
	} else {
		fmt.Fprintf(out, "\nStart server with: %s\n", style.Dim.Render("gt dolt start"))
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
	// explicitly parked for a human decision — the only trace was an
	// ephemeral nudge. Forcing agents through --authorized-by leaves a durable
	// audit trail on the authorizing bead.
	if doltCleanupForce && !doltCleanupDry {
		if err := authorizeCleanupForce(townRoot, agentActor(os.Getenv), ui.IsTerminal(), doltCleanupAuthorizedBy); err != nil {
			return err
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
		fmt.Print(tooManyOrphansHint())
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
// The agent-by-default actor is doltserver.UnidentifiedActor, which every
// authorization guard refuses (gt-kz9k0).
func resolveDestructiveActor(envActor string, stdoutIsTTY bool) (actor string, isAgent bool) {
	if envActor != "" {
		return envActor, true
	}
	if stdoutIsTTY {
		return "", false
	}
	return string(doltserver.UnidentifiedActor), true
}

// authorizeCleanupForce enforces the force guardrails for a destructive
// cleanup: an agent needs a genuine authorization bead (gt-61x, gt-2oy), and an
// unidentified actor can never supply one (gt-kz9k0).
//
// The actor and bead are parameters rather than reads of process state so each
// refusal is testable without mutating the environment.
func authorizeCleanupForce(townRoot, envActor string, stdoutIsTTY bool, authorizedBy string) error {
	actor, isAgent := resolveDestructiveActor(envActor, stdoutIsTTY)
	if !isAgent {
		return nil
	}
	// Refuse before the bead lookup: an unidentified process can create the very
	// bead it names, so no bead can authorize it (gt-kz9k0).
	if doltserver.IsUnidentifiedActor(actor) {
		return doltserver.ErrUnidentifiedActor
	}
	if err := checkAgentForceAuthorization(actor, authorizedBy); err != nil {
		return err
	}
	issue, err := beads.New(townRoot).Show(authorizedBy)
	if err != nil {
		return fmt.Errorf("--authorized-by bead %q not found: %w", authorizedBy, err)
	}
	// The bead must be a genuine authorization record, not just any bead (gt-2oy).
	return doltserver.ValidateForceAuthorization(issue, actor)
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
  1. Get explicit approval from the overseer (escalate if needed)
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

// migrateRunningGuard refuses a live migration while the daemon or the Dolt
// server is up. A dry run makes no changes, so it is never refused.
func migrateRunningGuard(dryRun, daemonRunning, serverRunning bool) error {
	if dryRun {
		return nil
	}
	if daemonRunning {
		return fmt.Errorf("Gas Town daemon is running. Stop it first with: gt daemon stop\n\nThe daemon spawns bd processes that can race with migration.\nStop the daemon, run migration, then restart it.")
	}
	if serverRunning {
		return fmt.Errorf("Dolt server is running. Stop it first with: gt dolt stop")
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

	// Check if Dolt server is running - must stop first
	running, _, _ := doltserver.IsRunning(townRoot)

	// A dry run changes nothing, so it previews while the town is up.
	if err := migrateRunningGuard(doltMigrateDry, daemonRunning, running); err != nil {
		return err
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
		if daemonRunning || running {
			fmt.Println("Note: the daemon or Dolt server is running; stop them before a real migration.")
		}
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

// tooManyOrphansHint is the guidance printed when there are too many orphans
// for SQL cleanup. It suggests no blanket rm -rf: a test-database glob also
// matches databases an open dolt-hold bead parks, and only the hold-aware
// gt dolt cleanup listing says which are safe to remove.
func tooManyOrphansHint() string {
	return "  The server is likely overloaded. SQL cleanup would take hours.\n\n" +
		"  Instead, list the orphans while the server is still up (the hold lookup\n" +
		"  needs it), then stop the server and remove them from the filesystem:\n\n" +
		"    gt dolt cleanup --dry-run    # lists each orphan and marks the ones HELD by a hold bead\n" +
		"    gt dolt stop\n" +
		"    (delete each orphan directory that listing did not mark HELD, by its full name)\n" +
		"    gt dolt start\n\n" +
		"  Never remove databases in bulk by glob: a glob also matches held databases.\n"
}
