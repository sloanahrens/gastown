package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/crew"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/formula"
	"github.com/steveyegge/gastown/internal/mayor"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

// agentStartResult holds the result of starting an agent.
type agentStartResult struct {
	name   string // Display name like "Mayor"
	ok     bool   // Whether start succeeded
	detail string // Status detail (session name or error)
}

// UpOutput represents the JSON output of the up command.
type UpOutput struct {
	Success  bool            `json:"success"`
	Services []ServiceStatus `json:"services"`
	Summary  UpSummary       `json:"summary"`
}

// ServiceStatus represents the status of a single service.
type ServiceStatus struct {
	Name   string `json:"name"`
	Type   string `json:"type"` // dolt, daemon, mayor, crew, polecat
	Rig    string `json:"rig,omitempty"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// UpSummary provides counts for the up command output.
type UpSummary struct {
	Total   int `json:"total"`
	Started int `json:"started"`
	Failed  int `json:"failed"`
}

func buildUpSummary(services []ServiceStatus) UpSummary {
	started := 0
	failed := 0
	for _, svc := range services {
		if svc.OK {
			started++
		} else {
			failed++
		}
	}
	return UpSummary{
		Total:   len(services),
		Started: started,
		Failed:  failed,
	}
}

func emitUpJSON(w io.Writer, services []ServiceStatus) error {
	summary := buildUpSummary(services)
	output := UpOutput{
		Success:  summary.Failed == 0,
		Services: services,
		Summary:  summary,
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(output); err != nil {
		return err
	}
	if summary.Failed > 0 {
		return NewSilentExit(1)
	}
	return nil
}

// maxConcurrentAgentStarts limits parallel agent startups to avoid resource
// exhaustion. Each agent start spawns a tmux session and runs gt prime, so
// more than ~10 concurrent starts can saturate CPU and cause timeouts.
const maxConcurrentAgentStarts = 10

var upCmd = &cobra.Command{
	Use:     "up",
	GroupID: GroupServices,
	Short:   "Bring up all Gas Town services",
	Long: `Start all Gas Town long-lived services.

This is the idempotent "boot" command for Gas Town. It ensures all
infrastructure agents are running:

  • Dolt       - Shared SQL database server for beads
  • Daemon     - Go background process that pokes agents
  • Mayor      - Global work coordinator

Polecats are NOT started by this command - they are transient workers
spawned on demand by the Mayor. The daemon's patrol_scan tick restarts
one whose session died while it held work.

Use --restore to also start:
  • Crew       - Per rig settings (settings/config.json crew.startup)
  • Polecats   - Those with pinned beads (work attached)

Running 'gt up' multiple times is safe - it only starts services that
aren't already running.`,
	Args: cobra.NoArgs,
	RunE: runUp,
}

var (
	upQuiet   bool
	upRestore bool
	upJSON    bool
)

func init() {
	upCmd.Flags().BoolVarP(&upQuiet, "quiet", "q", false, "Only show errors (ignored with --json)")
	upCmd.Flags().BoolVar(&upRestore, "restore", false, "Also restore crew (from settings) and polecats (from hooks)")
	upCmd.Flags().BoolVar(&upJSON, "json", false, "Output as JSON")
	rootCmd.AddCommand(upCmd)
}

// syncUpFormulas writes the formulas this binary ships into the town formulas
// dir. The binary is canonical (gt-y3pgh.6): gt up syncs as gt install does, so
// an upgraded binary's formulas reach disk and a hand-edited copy is replaced.
// Files gt does not own are left on disk for gt doctor to report.
func syncUpFormulas(townRoot string, out, errOut io.Writer) {
	count, err := formula.ProvisionFormulas(townRoot)
	if err != nil {
		fmt.Fprintf(errOut, "Warning: could not sync formulas: %v\n", err)
		return
	}
	if count > 0 {
		fmt.Fprintf(out, "%s Synced %d formulas from this binary\n", style.SuccessPrefix, count)
	}
}

func runUp(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Ensure lifecycle defaults are configured. On first run this creates
	// mayor/daemon.json with sensible defaults for the six-stage Dolt lifecycle.
	// On subsequent runs it fills in any newly added patrols without touching
	// existing config. Errors are non-fatal — the town can run without lifecycle
	// automation, it just won't have automated maintenance.
	if err := daemon.EnsureLifecycleConfigFile(townRoot); err != nil {
		// A daemon.json that does not parse is never rewritten and the town
		// does not start from defaults (gt-fcxe9.10).
		if errors.Is(err, config.ErrUnparseable) {
			return err
		}
		fmt.Fprintf(os.Stderr, "Warning: could not configure lifecycle defaults: %v\n", err)
	}

	// Load daemon.json env vars so services (Dolt, etc.) use the right config.
	// The daemon does this too, but gt up starts services before the daemon.
	if patrolCfg := daemon.LoadPatrolConfig(townRoot); patrolCfg != nil {
		for k, v := range patrolCfg.Env {
			//testpolicy:allow prod-no-setenv — gt up publishes its environment to every server and agent it starts
			os.Setenv(k, v)
		}
	}
	applyConfiguredDoltEnv(townRoot)

	allOK := true
	var services []ServiceStatus

	rigs := discoverRigs(townRoot)

	// Safety: bring current agent out of DND on startup so orchestration nudges
	// are not silently muted after a previous incident/debug session.
	if changed, err := disableCurrentAgentDND(townRoot); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not reset DND state: %v\n", err)
	} else if changed && !upQuiet {
		fmt.Printf("%s DND was enabled; reset to normal for current agent\n", style.SuccessPrefix)
	}

	// Formulas land before any agent starts and cooks one.
	formulaOut := io.Writer(os.Stdout)
	if upQuiet || upJSON {
		formulaOut = io.Discard
	}
	syncUpFormulas(townRoot, formulaOut, os.Stderr)

	// Start Dolt, the daemon and the mayor in parallel
	var daemonErr error
	var daemonPID int
	// daemonNote is set when the town boot had to reload the supervisor job
	// the daemon runs under, which restarts it (gt-x872).
	var daemonNote string
	var mayorResult agentStartResult
	var doltOK bool
	var doltDetail string
	var doltSkipped bool

	var startupWg sync.WaitGroup
	startupWg.Add(3)

	// 0. Dolt server (if configured)
	go func() {
		defer startupWg.Done()
		cfg := doltserver.DefaultConfig(townRoot)
		if _, err := os.Stat(cfg.DataDir); os.IsNotExist(err) {
			doltSkipped = true
			return
		}
		running, _, _ := doltserver.IsRunning(townRoot)
		if running {
			doltOK = true
			doltDetail = "already running"
			return
		}
		if err := doltserver.Start(townRoot); err != nil {
			doltDetail = err.Error()
		} else {
			doltOK = true
			doltDetail = fmt.Sprintf("started (port %d)", cfg.Port)
		}
	}()

	// 1. Daemon (Go process)
	go func() {
		defer startupWg.Done()
		note, err := ensureDaemon(townRoot)
		if err != nil {
			daemonErr = err
			return
		}
		daemonNote = note
		running, pid, _ := daemon.IsRunning(townRoot)
		if running {
			daemonPID = pid
		}
	}()

	// 2. Mayor
	go func() {
		defer startupWg.Done()
		mayorMgr := mayor.NewManager(townRoot)
		// Replacing a dead Mayor session is a Respawn (gt-4k3fj.4.1).
		superviseMayor(mayorMgr, operatorSupervisor(townRoot), operatorActor("gt up"))
		if err := mayorMgr.Start(""); err != nil {
			if errors.Is(err, mayor.ErrAlreadyRunning) {
				mayorResult = agentStartResult{name: "Mayor", ok: true, detail: mayorMgr.SessionName()}
			} else {
				mayorResult = agentStartResult{name: "Mayor", ok: false, detail: err.Error()}
			}
		} else {
			mayorResult = agentStartResult{name: "Mayor", ok: true, detail: mayorMgr.SessionName()}
		}
	}()

	startupWg.Wait()

	// Ensure beads metadata points to the Dolt server
	if !doltSkipped && doltOK {
		_, _ = doltserver.EnsureAllMetadata(townRoot)
	}

	// Collect Dolt status (if configured)
	if !doltSkipped {
		services = append(services, ServiceStatus{Name: "Dolt", Type: "dolt", OK: doltOK, Detail: doltDetail})
		if !doltOK {
			allOK = false
		}
	}

	// Collect daemon/mayor results (always append daemon status)
	if daemonErr != nil {
		services = append(services, ServiceStatus{Name: "Daemon", Type: "daemon", OK: false, Detail: daemonErr.Error()})
		allOK = false
	} else {
		detail := "running (PID unknown)"
		if daemonPID > 0 {
			detail = fmt.Sprintf("PID %d", daemonPID)
		}
		if daemonNote != "" {
			detail = fmt.Sprintf("%s (%s)", detail, daemonNote)
		}
		services = append(services, ServiceStatus{Name: "Daemon", Type: "daemon", OK: true, Detail: detail})
	}
	services = append(services, ServiceStatus{Name: mayorResult.name, Type: constants.RoleMayor, OK: mayorResult.ok, Detail: mayorResult.detail})
	if !mayorResult.ok {
		allOK = false
	}

	// Ensure Dolt server is fully ready before starting agents that depend on it.
	// Agents run bd commands on startup (via gt prime) that connect to the Dolt
	// SQL server. Without this gate, they race the server
	// and get "connection refused" errors. (gt-zou1n)
	// Only wait if Dolt was actually started (or detected running). If it failed or
	// was skipped, polling the port would just burn the full timeout. (review finding #1)
	if !doltSkipped && doltOK {
		waitForDoltReady(townRoot)
		// Propagate Dolt connection info to process env so all subsequently spawned
		// agents (crew, polecats) inherit it. Without this,
		// bd auto-starts rogue Dolt instances in agent tmux sessions. (GH#2412)
		// Host propagation prevents bd from falling back to 127.0.0.1 when the
		// Dolt server runs on a remote machine (e.g., mini2 over Tailscale).
		doltCfg := doltserver.DefaultConfig(townRoot)
		portStr := fmt.Sprintf("%d", doltCfg.Port)
		publish := map[string]string{
			"GT_DOLT_PORT":           portStr,
			"BEADS_DOLT_SERVER_PORT": portStr,
			"BEADS_DOLT_PORT":        portStr,
		}
		if doltCfg.Host != "" {
			publish["GT_DOLT_HOST"] = doltCfg.Host
			publish["BEADS_DOLT_SERVER_HOST"] = doltCfg.Host
		}
		for k, v := range publish {
			//testpolicy:allow prod-no-setenv — gt up publishes its environment to every server and agent it starts
			os.Setenv(k, v)
		}
	}

	// 3. Crew (if --restore)
	if upRestore {
		for _, rigName := range rigs {
			crewStarted, crewErrors := startCrewFromSettings(townRoot, rigName)
			for _, name := range crewStarted {
				services = append(services, ServiceStatus{
					Name:   fmt.Sprintf("Crew (%s/%s)", rigName, name),
					Type:   constants.RoleCrew,
					Rig:    rigName,
					OK:     true,
					Detail: session.CrewSessionName(townRegistry().PrefixForRig(rigName), name),
				})
			}
			for name, err := range crewErrors {
				services = append(services, ServiceStatus{
					Name:   fmt.Sprintf("Crew (%s/%s)", rigName, name),
					Type:   constants.RoleCrew,
					Rig:    rigName,
					OK:     false,
					Detail: err.Error(),
				})
				allOK = false
			}
		}

		// 4. Polecats with pinned work (if --restore)
		for _, rigName := range rigs {
			polecatsStarted, polecatErrors := startPolecatsWithWork(townRoot, rigName)
			for _, name := range polecatsStarted {
				services = append(services, ServiceStatus{
					Name:   fmt.Sprintf("Polecat (%s/%s)", rigName, name),
					Type:   constants.RolePolecat,
					Rig:    rigName,
					OK:     true,
					Detail: session.PolecatSessionName(townRegistry().PrefixForRig(rigName), name),
				})
			}
			for name, err := range polecatErrors {
				services = append(services, ServiceStatus{
					Name:   fmt.Sprintf("Polecat (%s/%s)", rigName, name),
					Type:   constants.RolePolecat,
					Rig:    rigName,
					OK:     false,
					Detail: err.Error(),
				})
				allOK = false
			}
		}
	}

	// Log boot event for both JSON and text paths
	if allOK {
		startedServices := []string{"dolt", "daemon", "mayor"}
		_ = events.LogFeed(events.TypeBoot, events.ActorGt, events.BootPayload("town", startedServices))
	}

	// Output JSON or text
	if upJSON {
		return emitUpJSON(os.Stdout, services)
	}

	// Text output
	for _, svc := range services {
		printStatus(svc.Name, svc.OK, svc.Detail)
	}

	fmt.Println()
	if allOK {
		fmt.Printf("%s All services running\n", style.Bold.Render("✓"))
	} else {
		fmt.Printf("%s Some services failed to start\n", style.Bold.Render("✗"))
		return fmt.Errorf("not all services started")
	}

	return nil
}

// applyConfiguredDoltEnv points gt up's own environment at the target
// town's managed Dolt endpoint: every server and agent gt up starts inherits
// this process's environment.
func applyConfiguredDoltEnv(townRoot string) {
	doltEnv := config.ConfiguredDoltEnv(townRoot)
	for _, key := range config.DoltEndpointEnvKeys {
		//testpolicy:allow prod-no-setenv — gt up publishes its environment to every server and agent it starts
		_ = os.Unsetenv(key)
	}
	for key, value := range doltEnv {
		//testpolicy:allow prod-no-setenv — gt up publishes its environment to every server and agent it starts
		_ = os.Setenv(key, value)
	}
}

func printStatus(name string, ok bool, detail string) {
	if upQuiet && ok {
		return
	}
	if ok {
		fmt.Printf("%s %s: %s\n", style.SuccessPrefix, name, style.Dim.Render(detail))
	} else {
		fmt.Printf("%s %s: %s\n", style.ErrorPrefix, name, detail)
	}
}

// disableCurrentAgentDND resets DND for the current role context (if muted).
// Returns true when a change was applied.
func disableCurrentAgentDND(townRoot string) (bool, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return false, fmt.Errorf("getting current directory: %w", err)
	}

	roleInfo, err := GetRoleWithContext(cwd, townRoot)
	if err != nil {
		// No role context (or not in role workspace): nothing to change.
		return false, nil
	}

	ctx := RoleContext{
		Role:     roleInfo.Role,
		Rig:      roleInfo.Rig,
		Polecat:  roleInfo.Polecat,
		TownRoot: townRoot,
		WorkDir:  cwd,
	}
	agentBeadID := getAgentBeadID(ctx)
	if agentBeadID == "" {
		return false, nil
	}

	bd := beads.New(townRoot)
	level, err := bd.GetAgentNotificationLevel(agentBeadID)
	if err != nil {
		// Missing bead/field should not block startup.
		return false, nil
	}
	if level != beads.NotifyMuted {
		return false, nil
	}

	if err := bd.UpdateAgentNotificationLevel(agentBeadID, beads.NotifyNormal); err != nil {
		return false, fmt.Errorf("updating notification level for %s: %w", agentBeadID, err)
	}
	return true, nil
}

// ensureDaemon starts the daemon if not running.
//
// A daemon that is already up is left running, with one exception: when the
// supervisor job it runs under is defined by a file that no longer matches the
// binary on disk, that job is reloaded from the rewritten file (gt-x872). The
// note returned in that case says so — the daemon restarted, and the caller
// shows why rather than leaving the PID change unexplained.
func ensureDaemon(townRoot string) (note string, err error) {
	// GH#2656: Don't restart the daemon while gt down is running.
	// GH#2907: If the sentinel's PID is dead, remove stale sentinel.
	sentinelPath := filepath.Join(townRoot, ShutdownSentinel)
	if data, err := os.ReadFile(sentinelPath); err == nil {
		stale := false
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
			if process, err := os.FindProcess(pid); err != nil {
				stale = true
			} else if err := process.Signal(syscall.Signal(0)); err != nil {
				stale = true
			}
		} else {
			// Sentinel exists but has no valid PID — treat as stale.
			stale = true
		}
		if stale {
			os.Remove(sentinelPath)
		} else {
			return "", fmt.Errorf("shutdown in progress (sentinel exists: %s)", sentinelPath)
		}
	}

	running, pid, err := daemon.IsRunning(townRoot)
	if err != nil {
		return "", err
	}
	ctl := realDaemonControl()
	if running {
		return ctl.reconcileSupervisorJob(townRoot, pid)
	}

	// Start it — through the provisioned supervisor when there is one
	// (gt-3jrm: a daemon spawned here by hand leaves a KeepAlive launchd job
	// respawn-looping against it).
	if _, _, err := ctl.startDaemon(townRoot); err != nil {
		// A concurrent starter (gt mayor, another gt up) may have won the
		// race between the check above and the start; that is success.
		if running, _, chk := ctl.isRunning(townRoot); chk == nil && running {
			return "", nil
		}
		return "", err
	}
	return "", nil
}

// rigPrefetchResult holds the result of loading a single rig config.
type rigPrefetchResult struct {
	index int
	rig   *rig.Rig
	err   error
}

// agentTask represents a unit of work for the agent worker pool.
type agentTask struct {
	rigName string
	rigObj  *rig.Rig
}

// agentResultMsg carries result back from worker to collector.
type agentResultMsg struct {
	rigName string
	result  agentStartResult
}

// discoverRigs finds all rigs in the town.
func discoverRigs(townRoot string) []string {
	var rigs []string

	// Try rigs.json first
	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	if rigsConfig, err := config.LoadRigsConfig(rigsConfigPath); err == nil {
		for name := range rigsConfig.Rigs {
			rigs = append(rigs, name)
		}
		return rigs
	}

	// Fallback: scan directory for rig-like directories
	entries, err := os.ReadDir(townRoot)
	if err != nil {
		return rigs
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		name := entry.Name()
		// Skip known non-rig directories
		if name == "mayor" || name == "daemon" || name == "deacon" ||
			name == ".git" || name == "docs" || name[0] == '.' {
			continue
		}

		dirPath := filepath.Join(townRoot, name)

		// Check for .beads directory (indicates a rig)
		beadsPath := filepath.Join(dirPath, ".beads")
		if _, err := os.Stat(beadsPath); err == nil {
			rigs = append(rigs, name)
			continue
		}

		// Check for polecats directory (indicates a rig)
		polecatsPath := filepath.Join(dirPath, "polecats")
		if _, err := os.Stat(polecatsPath); err == nil {
			rigs = append(rigs, name)
		}
	}

	return rigs
}

// startCrewFromSettings starts crew members based on rig settings.
// Returns list of started crew names and map of errors.
func startCrewFromSettings(townRoot, rigName string) ([]string, map[string]error) {
	started := []string{}
	failed := map[string]error{}

	rigPath := filepath.Join(townRoot, rigName)

	// Load rig settings
	settingsPath := filepath.Join(rigPath, "settings", "config.json")
	settings, err := config.LoadRigSettings(settingsPath)
	if err != nil {
		// No settings file or error - skip crew startup
		return started, failed
	}

	if settings.Crew == nil || settings.Crew.Startup == "" {
		// No crew startup preference
		return started, failed
	}

	// Get available crew members using helper
	crewMgr, _, err := getCrewManager(rigName)
	if err != nil {
		return started, failed
	}

	crewWorkers, err := crewMgr.List()
	if err != nil {
		return started, failed
	}

	if len(crewWorkers) == 0 {
		return started, failed
	}

	// Extract crew names
	crewNames := make([]string, len(crewWorkers))
	for i, w := range crewWorkers {
		crewNames[i] = w.Name
	}

	// Parse startup preference and determine which crew to start
	toStart := parseCrewStartupPreference(settings.Crew.Startup, crewNames)

	// Start each crew member using Manager
	for _, crewName := range toStart {
		if err := crewMgr.Start(crewName, crew.StartOptions{}); err != nil {
			if errors.Is(err, crew.ErrSessionRunning) {
				started = append(started, crewName)
			} else {
				failed[crewName] = err
			}
		} else {
			started = append(started, crewName)
		}
	}

	return started, failed
}

// parseCrewStartupPreference parses the natural language crew startup preference.
// Examples: "max", "joe and max", "all", "none", "pick one"
func parseCrewStartupPreference(pref string, available []string) []string {
	pref = strings.ToLower(strings.TrimSpace(pref))

	// Special keywords
	switch pref {
	case "none", "":
		return []string{}
	case "all":
		return available
	case "pick one", "any", "any one":
		if len(available) > 0 {
			return []string{available[0]}
		}
		return []string{}
	}

	// Parse comma/and-separated list
	// "joe and max" -> ["joe", "max"]
	// "joe, max" -> ["joe", "max"]
	// "max" -> ["max"]
	pref = strings.ReplaceAll(pref, " and ", ",")
	pref = strings.ReplaceAll(pref, ", but not ", ",-")
	pref = strings.ReplaceAll(pref, " but not ", ",-")

	parts := strings.Split(pref, ",")

	include := []string{}
	exclude := map[string]bool{}

	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		if strings.HasPrefix(part, "-") {
			// Exclusion
			exclude[strings.TrimPrefix(part, "-")] = true
		} else {
			include = append(include, part)
		}
	}

	// Filter to only available crew members
	result := []string{}
	for _, name := range include {
		if exclude[name] {
			continue
		}
		// Check if this crew exists
		for _, avail := range available {
			if avail == name {
				result = append(result, name)
				break
			}
		}
	}

	return result
}

// startPolecatsWithWork starts polecats that have pinned beads (work attached).
// Returns list of started polecat names and map of errors.
func startPolecatsWithWork(townRoot, rigName string) ([]string, map[string]error) {
	// Get polecat session manager
	_, r, err := getRig(rigName)
	if err != nil {
		return []string{}, map[string]error{}
	}
	// A start over a dead session is a Respawn, which an e-stop refuses
	// (gt-4k3fj.4.1).
	polecatMgr := supervisedPolecatSessions(tmux.NewTmux(), r, "gt up", operatorActor("gt up"))
	start := func(polecatName string) error {
		return polecatMgr.Start(polecatName, polecat.SessionStartOptions{})
	}
	return startPolecatsWithWorkUsing(townRoot, rigName, polecatHasPinnedWork, start)
}

// polecatHasPinnedWork reports whether agentID has a pinned bead in the store
// at polecatPath.
func polecatHasPinnedWork(polecatPath, agentID string) bool {
	pinnedBeads, err := beads.New(polecatPath).List(beads.ListOptions{
		Status:   beads.StatusPinned,
		Assignee: agentID,
		Priority: -1,
	})
	return err == nil && len(pinnedBeads) > 0
}

// startPolecatsWithWorkUsing is startPolecatsWithWork with the pinned-work
// check and the session start passed in.
func startPolecatsWithWorkUsing(townRoot, rigName string, hasWork func(polecatPath, agentID string) bool, start func(polecatName string) error) ([]string, map[string]error) {
	started := []string{}
	failed := map[string]error{}

	rigPath := filepath.Join(townRoot, rigName)
	polecatsDir := filepath.Join(rigPath, "polecats")

	// List polecat directories
	entries, err := os.ReadDir(polecatsDir)
	if err != nil {
		// No polecats directory
		return started, failed
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}

		polecatName := entry.Name()
		polecatPath := filepath.Join(polecatsDir, polecatName)

		// A polecat the operator parked — gt agent pause, or the deliberate
		// stop gt session stop records (gt-fojqs) — stays parked across a
		// town restart: the marker is the choke point those stops rely on
		// (gt-ahik), and starting the session here would undo the stop.
		if polecatSessionParked(townRoot, rigName, polecatName) {
			fmt.Printf("  %s %s/%s is parked; not starting it (resume with gt session start)\n",
				style.Dim.Render("○"), rigName, polecatName)
			continue
		}

		// Check if this polecat has a pinned bead (work attached)
		agentID := fmt.Sprintf("%s/polecats/%s", rigName, polecatName)
		if !hasWork(polecatPath, agentID) {
			continue
		}

		// This polecat has work - start it using SessionManager
		if err := start(polecatName); err != nil {
			if errors.Is(err, polecat.ErrSessionRunning) {
				started = append(started, polecatName)
			} else {
				failed[polecatName] = err
			}
		} else {
			started = append(started, polecatName)
		}
	}

	return started, failed
}

// doltReadyTimeout is how long gt up waits for the Dolt SQL server to accept
// connections before proceeding with agent startup. 10 seconds is
// generous: doltserver.Start() already retries for 5s, so this covers the case
// where the daemon (not gt up) started Dolt and it's still initializing.
const doltReadyTimeout = 10 * time.Second

// waitForDoltReady waits for the Dolt SQL server to be reachable before
// starting agents that depend on beads database access. If the server is not
// configured (no server-mode metadata), this is a no-op. If the timeout
// expires, logs a warning and continues (graceful degradation). (gt-zou1n)
func waitForDoltReady(townRoot string) {
	if err := doltserver.WaitForReady(townRoot, doltReadyTimeout); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: %v (agents may see connection errors)\n", err)
	}
}
