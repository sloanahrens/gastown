package cmd

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/web"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	dashboardPort int
	dashboardBind string
	dashboardOpen bool
	dashboardLog  string
)

// dashboardLogMaxSize caps the dashboard log before rotatingLog moves it to
// <name>.old.
const dashboardLogMaxSize = 10 * 1024 * 1024

var dashboardCmd = &cobra.Command{
	Use:     "dashboard",
	GroupID: GroupDiag,
	Short:   "Start the convoy tracking web dashboard",
	Long: `Start a web server that displays the convoy tracking dashboard.

The dashboard shows real-time convoy status with:
- Convoy list with status indicators
- Progress tracking for each convoy
- Last activity indicator (green/yellow/red)
- Auto-refresh every 30 seconds via htmx

Runtime errors and warnings are written to the town's logs/dashboard.log
(append, rotated to .old when it grows) in addition to the terminal, so the
error stream survives the terminal pane and can be inspected after the fact.
Override the location with --log-file (GT_DASHBOARD_LOG for scripts); outside
a town, nothing is written unless one of those is given.

Example:
  gt dashboard                    # Start on default port 8080
  gt dashboard --port 3000        # Start on port 3000
  gt dashboard --bind 0.0.0.0     # Listen on all interfaces
  gt dashboard --open             # Start and open browser`,
	RunE: runDashboard,
}

func init() {
	dashboardCmd.Flags().IntVar(&dashboardPort, "port", 8080, "HTTP port to listen on")
	defaultBind := "127.0.0.1"
	if os.Getenv("IS_SANDBOX") != "" {
		defaultBind = "0.0.0.0"
	}
	dashboardCmd.Flags().StringVar(&dashboardBind, "bind", defaultBind, "Address to bind to (use 0.0.0.0 for all interfaces)")
	dashboardCmd.Flags().BoolVar(&dashboardOpen, "open", false, "Open browser automatically")
	dashboardCmd.Flags().StringVar(&dashboardLog, "log-file", os.Getenv("GT_DASHBOARD_LOG"),
		"Log file for runtime errors and warnings (default: logs/dashboard.log under the town root)")
	rootCmd.AddCommand(dashboardCmd)
}

func runDashboard(cmd *cobra.Command, args []string) error {
	// Resolve the workspace before anything logs: the log file lives at the
	// town root, and the startup warnings below belong in it too (gt-9ed0).
	townRoot, wsErr := workspace.FindFromCwdOrError()

	// Mirror stdlib log output (all "dashboard:" errors and warnings) into
	// logs/dashboard.log so the error stream outlives the terminal pane.
	cleanupLog := installDashboardLog(townRoot)
	defer cleanupLog()

	// Check if we're in a workspace - if not, run in setup mode
	var handler http.Handler
	var err error
	webCfg := config.DefaultWebTimeoutsConfig()

	if wsErr != nil {
		// No workspace - run in setup mode
		handler, err = web.NewSetupMux()
		if err != nil {
			return fmt.Errorf("creating setup handler: %w", err)
		}
	} else {
		// In a workspace - run normal dashboard

		// Set BEADS_DOLT_PORT and GT_DOLT_PORT so bd/gt subprocesses connect
		// to the actual Dolt SQL server, not the dashboard's HTTP listen port.
		// Without this, inherited env vars could point bd at the wrong port.
		ensureDoltPortEnv(townRoot)

		fetcher, fetchErr := web.NewLiveConvoyFetcher()
		if fetchErr != nil {
			return fmt.Errorf("creating convoy fetcher: %w", fetchErr)
		}

		// Load web timeouts config (nil-safe: NewDashboardMux applies defaults)
		if ts, loadErr := config.LoadOrCreateTownSettings(config.TownSettingsPath(townRoot)); loadErr == nil {
			if ts.WebTimeouts != nil {
				webCfg = ts.WebTimeouts
			}
		} else {
			log.Printf("dashboard: loading town settings: %v (using defaults)", loadErr)
		}

		handler, err = web.NewDashboardMux(fetcher, webCfg)
		if err != nil {
			return fmt.Errorf("creating dashboard handler: %w", err)
		}
	}

	// Build the listen address and display URL
	listenAddr := fmt.Sprintf("%s:%d", dashboardBind, dashboardPort)
	displayHost := dashboardBind
	if displayHost == "0.0.0.0" {
		if hostname, err := os.Hostname(); err == nil {
			displayHost = hostname
		} else {
			displayHost = "localhost"
		}
	}
	url := fmt.Sprintf("http://%s:%d", displayHost, dashboardPort)

	// Open browser if requested
	if dashboardOpen {
		go openBrowser(url)
	}

	maxRunTimeout := config.ParseDurationOrDefault(webCfg.MaxRunTimeout, 120*time.Second)
	writeTimeout := maxRunTimeout + 15*time.Second
	if writeTimeout < 60*time.Second {
		writeTimeout = 60 * time.Second
	}

	// Start the server with timeouts
	// Only show the large banner if the terminal is wide enough (98 cols)
	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err == nil && width >= 98 {
		fmt.Print(`
 __       __  ________  __        ______    ______   __       __  ________
|  \  _  |  \|        \|  \      /      \  /      \ |  \     /  \|        \
| $$ / \ | $$| $$$$$$$$| $$     |  $$$$$$\|  $$$$$$\| $$\   /  $$| $$$$$$$$
| $$/  $\| $$| $$__    | $$     | $$   \$$| $$  | $$| $$$\ /  $$$| $$__
| $$  $$$\ $$| $$  \   | $$     | $$      | $$  | $$| $$$$\  $$$$| $$  \
| $$ $$\$$\$$| $$$$$   | $$     | $$   __ | $$  | $$| $$\$$ $$ $$| $$$$$
| $$$$  \$$$$| $$_____ | $$_____| $$__/  \| $$__/ $$| $$ \$$$| $$| $$_____
| $$$    \$$$| $$     \| $$     \\$$    $$ \$$    $$| $$  \$ | $$| $$     \
 \$$      \$$ \$$$$$$$$ \$$$$$$$$ \$$$$$$   \$$$$$$  \$$      \$$ \$$$$$$$$

 ________   ______          ______    ______    ______   ________   ______   __       __  __    __
|        \ /      \        /      \  /      \  /      \ |        \ /      \ |  \  _  |  \|  \  |  \
 \$$$$$$$$|  $$$$$$\      |  $$$$$$\|  $$$$$$\|  $$$$$$\ \$$$$$$$$|  $$$$$$\| $$ / \ | $$| $$\ | $$
   | $$   | $$  | $$      | $$ __\$$| $$__| $$| $$___\$$   | $$   | $$  | $$| $$/  $\| $$| $$$\| $$
   | $$   | $$  | $$      | $$|    \| $$    $$ \$$    \    | $$   | $$  | $$| $$  $$$\ $$| $$$$\ $$
   | $$   | $$  | $$      | $$ \$$$$| $$$$$$$$ _\$$$$$$\   | $$   | $$  | $$| $$ $$\$$\$$| $$\$$ $$
   | $$   | $$__/ $$      | $$__| $$| $$  | $$|  \__| $$   | $$   | $$__/ $$| $$$$  \$$$$| $$ \$$$$
   | $$    \$$    $$       \$$    $$| $$  | $$ \$$    $$   | $$    \$$    $$| $$$    \$$$| $$  \$$$
    \$$     \$$$$$$         \$$$$$$  \$$   \$$  \$$$$$$     \$$     \$$$$$$  \$$      \$$ \$$   \$$

`)
	} else {
		fmt.Print("\n  WELCOME TO GASTOWN\n\n")
	}
	fmt.Printf("  launching dashboard at %s  •  api: %s/api/  •  listening on %s  •  ctrl+c to stop\n", url, url, listenAddr)

	server := &http.Server{
		Addr:              listenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       120 * time.Second,
	}
	return server.ListenAndServe()
}

// ensureDoltPortEnv sets GT_DOLT_PORT, BEADS_DOLT_SERVER_PORT,
// BEADS_DOLT_PORT, and BEADS_DOLT_SERVER_HOST
// to the actual Dolt server connection info. This prevents bd subprocesses from
// inheriting stale or incorrect values from the environment.
// Uses the same resolver as AgentEnv and doltserver.DefaultConfig.
func ensureDoltPortEnv(townRoot string) {
	port := config.ResolveDoltPort(townRoot)
	if port <= 0 {
		port = doltserver.DefaultPort
	}
	portStr := strconv.Itoa(port)
	os.Setenv("GT_DOLT_PORT", portStr)
	os.Setenv("BEADS_DOLT_SERVER_PORT", portStr)
	os.Setenv("BEADS_DOLT_PORT", portStr)

	if host := config.ResolveDoltHost(townRoot); host != "" {
		os.Setenv("GT_DOLT_HOST", host)
		os.Setenv("BEADS_DOLT_SERVER_HOST", host)
	} else {
		os.Unsetenv("BEADS_DOLT_SERVER_HOST")
	}
}

// rotatingLog appends to the log file, rotating it to <name>.old once it grows
// past max. Rotation is best-effort: the rename-then-reopen is not atomic, so
// lines written in that window can be lost, and after a failed rename the log
// stops rotating and keeps appending to the oversized file (gt-9ed0).
type rotatingLog struct {
	mu           sync.Mutex
	path         string
	max          int64
	f            *os.File
	rotateFailed bool
}

// open (re)opens the log file in append mode, creating it and its directory
// if needed.
func (r *rotatingLog) open() error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(r.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if r.f != nil {
		r.f.Close()
	}
	r.f = f
	return nil
}

// rotate renames the live file to <name>.old and reopens a fresh one. The
// caller holds r.mu. On a failed rename it reopens the existing file so
// appends continue where they left off, and reports the failure.
func (r *rotatingLog) rotate() error {
	r.f.Close()
	r.f = nil
	renameErr := os.Rename(r.path, r.path+".old")
	if err := r.open(); err != nil {
		return err
	}
	return renameErr
}

func (r *rotatingLog) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	if r.max > 0 && !r.rotateFailed {
		if fi, err := r.f.Stat(); err == nil && fi.Size()+int64(len(p)) > r.max {
			if err := r.rotate(); err != nil {
				// Retrying a rename that keeps failing would cost a
				// stat/close/rename/reopen on every later write, all under
				// r.mu, so accept the oversized file instead.
				r.rotateFailed = true
			}
		}
	}
	return r.f.Write(p)
}

func (r *rotatingLog) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	err := r.f.Close()
	r.f = nil
	return err
}

// installDashboardLog redirects the stdlib log package to write to the
// dashboard log file (append, with rotation) in addition to stderr, so
// runtime errors and warnings land in a durable file, not just the terminal.
// The file is <townRoot>/logs/dashboard.log, or the --log-file path when one
// is set. Outside a town there is no directory for the dashboard to own, so
// setup mode logs only when --log-file is given: creating ./logs/ in whatever
// directory the user ran from is not ours to do (gt-9ed0). A file that cannot
// be opened degrades the dashboard to stderr-only logging.
//
// The returned cleanup func restores stderr-only logging; the caller defers it.
func installDashboardLog(townRoot string) func() {
	logPath := dashboardLog
	if logPath == "" {
		if townRoot == "" {
			return func() {}
		}
		logPath = filepath.Join(townRoot, "logs", "dashboard.log")
	}

	rl := &rotatingLog{path: logPath, max: dashboardLogMaxSize}
	if err := rl.open(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: dashboard log file %s unavailable (%v); logging to stderr only\n", logPath, err)
		return func() {}
	}

	prev := log.Writer()
	log.SetOutput(io.MultiWriter(os.Stderr, rl))
	return func() {
		log.SetOutput(prev)
		_ = rl.Close()
	}
}

// openBrowser opens the specified URL in the default browser.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	default:
		return
	}
	_ = cmd.Start()
}
