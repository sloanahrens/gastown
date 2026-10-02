package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/patrolscan"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/suggest"
	"github.com/steveyegge/gastown/internal/supervisor"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

// Session command flags
var (
	sessionIssue       string
	sessionForce       bool
	sessionRequestedBy string
	sessionLines       int
	sessionMessage     string
	sessionFile        string
	sessionRigFilter   string
	sessionListJSON    bool
	sessionStatusJSON  bool
)

var sessionCmd = &cobra.Command{
	Use:     "session",
	Aliases: []string{"sess"},
	GroupID: GroupAgents,
	Short:   "Manage polecat sessions",
	RunE:    requireSubcommand,
	Long: `Manage tmux sessions for polecats.

Sessions are tmux sessions running Claude for each polecat.
Use the subcommands to start, stop, attach, and monitor sessions.

Addresses are <rig>/<name>. <name> is a polecat, or one of the rig's roles
(witness, refinery), which own sessions named after themselves. An address
that names neither is refused as not found by every subcommand, so a session
that does not exist is never reported as a stopped or restarted one.

TIP: To send messages to a running session, use 'gt nudge' (not 'session inject').
The nudge command uses reliable delivery that works correctly with Claude Code.`,
}

var sessionStartCmd = &cobra.Command{
	Use:   "start <rig>/<polecat>",
	Short: "Start a polecat session",
	Long: `Start a new tmux session for a polecat.

Creates a tmux session, navigates to the polecat's working directory,
and launches claude. Optionally inject an initial issue to work on.

Examples:
  gt session start wyvern/Toast
  gt session start wyvern/Toast --issue gt-123`,
	Args: cobra.ExactArgs(1),
	RunE: runSessionStart,
}

var sessionStopCmd = &cobra.Command{
	Use:   "stop <rig>/<polecat>",
	Short: "Stop a polecat session",
	Long: `Stop a running polecat session.

Attempts graceful shutdown first (Ctrl-C), then kills the tmux session.
Use --force to skip graceful shutdown.

The stop is recorded as a deliberate one, so the witness zombie detector
and the stuck-agent dog leave the polecat alone instead of restarting it
and reporting a crash the operator asked for. The polecat stays stopped
until gt session start (or gt agent resume) clears the record.`,
	Args: cobra.ExactArgs(1),
	RunE: runSessionStop,
}

var sessionAtCmd = &cobra.Command{
	Use:     "at <rig>/<polecat>",
	Aliases: []string{"attach"},
	Short:   "Attach to a running session",
	Long: `Attach to a running polecat session.

Attaches the current terminal to the tmux session. Detach with Ctrl-B D.`,
	Args: cobra.ExactArgs(1),
	RunE: runSessionAttach,
}

var sessionListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all sessions",
	Long: `List all running polecat sessions.

Shows session status, rig, and polecat name. Use --rig to filter by rig.`,
	RunE: runSessionList,
}

var sessionCaptureCmd = &cobra.Command{
	Use:   "capture <rig>/<polecat> [count]",
	Short: "Capture recent session output",
	Long: `Capture recent output from a polecat session.

Returns the last N lines of terminal output. Useful for checking progress.

Examples:
  gt session capture wyvern/Toast        # Last 100 lines (default)
  gt session capture wyvern/Toast 50     # Last 50 lines
  gt session capture wyvern/Toast -n 50  # Same as above`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runSessionCapture,
}

var sessionInjectCmd = &cobra.Command{
	Use:   "inject <rig>/<polecat>",
	Short: "Send message to session (prefer 'gt nudge')",
	Long: `Send a message to a polecat session.

NOTE: For sending messages to Claude sessions, use 'gt nudge' instead.
It uses reliable delivery (literal mode + timing) that works correctly
with Claude Code's input handling.

This command is a low-level primitive for file-based injection or
cases where you need raw tmux send-keys behavior.

Examples:
  gt nudge greenplace/furiosa "Check your mail"     # Preferred
  gt session inject wyvern/Toast -f prompt.txt   # For file injection`,
	Args: cobra.ExactArgs(1),
	RunE: runSessionInject,
}

var sessionRestartCmd = &cobra.Command{
	Use:   "restart <rig>/<polecat>",
	Short: "Restart a polecat session",
	Long: `Restart a polecat session (stop + start).

Gracefully stops the current session and starts a fresh one.
Use --force to skip graceful shutdown.

Use --requested-by when an automated caller performs the restart, so the
town log's wake line names it instead of reading like an operator's own
"gt session start" (gt-tcrgb).`,
	Args: cobra.ExactArgs(1),
	RunE: runSessionRestart,
}

var sessionStatusCmd = &cobra.Command{
	Use:   "status <rig>/<polecat>",
	Short: "Show session status details",
	Long: `Show detailed status for a polecat session.

Displays running state, uptime, session info, and activity.`,
	Args: cobra.ExactArgs(1),
	RunE: runSessionStatus,
}

func init() {
	// Start flags
	sessionStartCmd.Flags().StringVar(&sessionIssue, "issue", "", "Issue ID to work on")

	// Stop flags
	sessionStopCmd.Flags().BoolVarP(&sessionForce, "force", "f", false, "Force immediate shutdown")

	// List flags
	sessionListCmd.Flags().StringVar(&sessionRigFilter, "rig", "", "Filter by rig name")
	sessionListCmd.Flags().BoolVar(&sessionListJSON, "json", false, "Output as JSON")

	// Capture flags
	sessionCaptureCmd.Flags().IntVarP(&sessionLines, "lines", "n", 100, "Number of lines to capture")

	// Inject flags
	sessionInjectCmd.Flags().StringVarP(&sessionMessage, "message", "m", "", "Message to inject")
	sessionInjectCmd.Flags().StringVarP(&sessionFile, "file", "f", "", "File to read message from")

	// Restart flags
	sessionRestartCmd.Flags().BoolVarP(&sessionForce, "force", "f", false, "Force immediate shutdown")
	sessionRestartCmd.Flags().StringVar(&sessionRequestedBy, "requested-by", "",
		"Caller performing an automated restart (e.g. \"witness\"); recorded in the town log wake line")

	// Status flags
	sessionStatusCmd.Flags().BoolVar(&sessionStatusJSON, "json", false, "Output as JSON")

	// Add subcommands
	sessionCmd.AddCommand(sessionStartCmd)
	sessionCmd.AddCommand(sessionStopCmd)
	sessionCmd.AddCommand(sessionAtCmd)
	sessionCmd.AddCommand(sessionListCmd)
	sessionCmd.AddCommand(sessionCaptureCmd)
	sessionCmd.AddCommand(sessionInjectCmd)
	sessionCmd.AddCommand(sessionRestartCmd)
	sessionCmd.AddCommand(sessionStatusCmd)

	rootCmd.AddCommand(sessionCmd)
}

// parseAddress parses "rig/polecat" format.
// If no "/" is present, attempts to infer rig from current directory.
func parseAddress(addr string) (rigName, polecatName string, err error) {
	parts := strings.SplitN(addr, "/", 2)
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		return parts[0], parts[1], nil
	}

	// No slash - try to infer rig from cwd
	if !strings.Contains(addr, "/") && addr != "" {
		townRoot, err := workspace.FindFromCwd()
		if err == nil && townRoot != "" {
			inferredRig, err := inferRigFromCwd(townRoot)
			if err == nil && inferredRig != "" {
				return inferredRig, addr, nil
			}
		}
	}

	return "", "", fmt.Errorf("invalid address format: expected 'rig/polecat', got '%s'", addr)
}

// getSessionManager creates a session manager for the given rig.
func getSessionManager(rigName string) (*polecat.SessionManager, error) {
	_, r, err := getRig(rigName)
	if err != nil {
		return nil, err
	}
	return polecat.NewSessionManager(tmux.NewTmux(), r, townRegistry()), nil
}

// sessionSeat is a resolved `gt session` address: the <rig>/<name> polecat
// the caller named and the session manager that serves its rig.
type sessionSeat struct {
	Rig  string
	Name string
	Mgr  *polecat.SessionManager
	// r is the rig the address names.
	r *rig.Rig
}

// supervise routes the seat's session manager's kills through the operator
// supervisor (gt-4k3fj.4.1), naming verb as the actor's command.
func (s sessionSeat) supervise(verb string) {
	s.Mgr.SetHooks(polecatSessionHooks(operatorSupervisor(filepath.Dir(s.r.Path)), townRegistry(), s.Rig, verb, operatorActor(verb)))
}

// resolveSessionSeat parses a <rig>/<name> address and resolves it against the
// rig it names.
//
// Every `gt session` verb resolves its argument here, so one address gets one
// answer wherever it is used. They used to disagree: `gt session status
// gastown/<gone>` printed "State: ○ stopped" and exited 0 for a polecat whose
// directory was gone, while `gt session start` refused that same address as
// not found and `gt session restart` reported "Session restarted" for it
// (gt-pud2g). A caller recovering a stalled polecat reads that success line
// and moves on, leaving the polecat with no session and nobody watching it.
//
// The seats are the rig's polecats: its polecats/ directories.
func resolveSessionSeat(args []string) (sessionSeat, error) {
	return resolveSessionSeatWith(args, getRig)
}

// resolveSessionSeatWith is resolveSessionSeat with the rig looked up by
// lookupRig (getRig, which finds the town from cwd, in production).
func resolveSessionSeatWith(args []string, lookupRig func(rigName string) (string, *rig.Rig, error)) (sessionSeat, error) {
	rigName, name, err := parseAddress(args[0])
	if err != nil {
		return sessionSeat{}, err
	}

	_, r, err := lookupRig(rigName)
	if err != nil {
		return sessionSeat{}, err
	}
	mgr := polecat.NewSessionManager(tmux.NewTmux(), r, townRegistry())

	if mgr.HasPolecat(name) {
		return sessionSeat{Rig: rigName, Name: name, Mgr: mgr, r: r}, nil
	}

	suggestions := suggest.FindSimilar(name, r.Polecats, 3)
	hint := fmt.Sprintf("Create with: gt polecat identity add %s %s", rigName, name)
	return sessionSeat{}, errors.New(suggest.FormatSuggestion("Polecat", name, suggestions, hint))
}

func runSessionStart(cmd *cobra.Command, args []string) error {
	seat, err := resolveSessionSeat(args)
	if err != nil {
		return err
	}
	rigName, polecatName := seat.Rig, seat.Name
	// A start over a session whose agent exited is a Respawn, a failed
	// startup's kill a Cleanup.
	seat.supervise("gt session start")

	opts := polecat.SessionStartOptions{
		Issue: sessionIssue,
	}

	fmt.Printf("Starting session for %s/%s...\n", rigName, polecatName)
	if err := seat.Mgr.Start(polecatName, opts); err != nil {
		return fmt.Errorf("starting session: %w", err)
	}

	fmt.Printf("%s Session started. Attach with: %s\n",
		style.Bold.Render("✓"),
		style.Dim.Render(fmt.Sprintf("gt session at %s/%s", rigName, polecatName)))

	// An explicit start is an explicit resume (gt-fojqs).
	townRoot, _ := workspace.FindFromCwd()
	if clearParkedSession(townRoot, rigName, polecatName) {
		fmt.Print(pauseClearedNotice)
	}

	// Log wake event
	if townRoot != "" {
		agent := fmt.Sprintf("%s/%s", rigName, polecatName)
		_ = logSessionWake(townRoot, agent, rigName, sessionIssue)
	}

	return nil
}

func runSessionStop(cmd *cobra.Command, args []string) error {
	rigName, polecatName, err := parseAddress(args[0])
	if err != nil {
		return err
	}

	_, r, err := getRig(rigName)
	if err != nil {
		return err
	}
	// An operator stop: the supervisor's Stop, logged, not refused by an
	// e-stop or a park (gt-4k3fj.4.1).
	polecatMgr := supervisedPolecatSessions(tmux.NewTmux(), r, "session stop", operatorActor("gt session stop"))

	townRoot, _ := workspace.FindFromCwd()

	if sessionForce {
		fmt.Printf("Force stopping session for %s/%s...\n", rigName, polecatName)
	} else {
		fmt.Printf("Stopping session for %s/%s...\n", rigName, polecatName)
	}

	// Record the stop before killing the session: a patrol reading between
	// the two would see a session-dead polecat with no record that the stop
	// was deliberate, and restart it (gt-fojqs).
	markerWritten := false
	if polecatMgr.HasPolecat(polecatName) {
		wrote, werr := writeDeliberateStopMarker(townRoot, rigName, polecatName)
		if werr != nil {
			style.PrintWarning("could not record the stop for %s/%s: %v (the witness may restart it)", rigName, polecatName, werr)
		}
		markerWritten = wrote
	}

	if err := polecatMgr.Stop(polecatName, sessionForce); err != nil {
		// Nothing was stopped, so nothing is parked: withdraw a marker this
		// run wrote rather than leave the polecat looking deliberately stopped.
		if markerWritten {
			clearParkedSession(townRoot, rigName, polecatName)
		}
		return fmt.Errorf("stopping session: %w", err)
	}

	fmt.Printf("%s Session stopped.\n", style.Bold.Render("✓"))
	if markerWritten {
		fmt.Printf("  Recorded as a deliberate stop — the witness will not restart it.\n")
		fmt.Printf("  Resume with: %s\n", style.Dim.Render("gt session start "+rigName+"/"+polecatName))
	}

	// Log kill event
	if townRoot != "" {
		agent := fmt.Sprintf("%s/%s", rigName, polecatName)
		reason := "gt session stop"
		if sessionForce {
			reason = "gt session stop --force"
		}
		_ = logSessionKill(townRoot, agent, rigName, polecatName, reason)
	}

	return nil
}

// deliberateSessionStopReason is the pause-marker reason gt session stop
// writes. Reasons are shown verbatim by gt status and by scanner logs, so it
// names the command that wrote it.
const deliberateSessionStopReason = "deliberate stop (gt session stop)"

// pauseClearedNotice is printed by the two starts that clear a parked
// session's pause marker: gt session start and gt session restart.
const pauseClearedNotice = "  Cleared the pause marker — the witness will restart this session if it dies again.\n"

// polecatSessionParked reports whether an operator parked this polecat's
// session — gt agent pause, or the deliberate stop gt session stop records —
// so every gate in this package reads one coordinate (gt-fojqs).
func polecatSessionParked(townRoot, rigName, polecatName string) bool {
	if townRoot == "" {
		return false
	}
	paused, _, _ := agentpause.PauseGate(townRoot, rigName, constants.RolePolecat, polecatName)
	return paused
}

// writeDeliberateStopMarker records an operator-initiated stop as a pause
// marker, the choke point the witness zombie detector and the stuck-agent dog
// already honor (gt-ahik). Without it the detector restarts the polecat the
// operator just stopped and alerts on it as if it had crashed (gt-fojqs).
//
// Reports whether it wrote. It writes nothing when a marker is already there,
// so a polecat parked by gt agent pause keeps its own reason.
func writeDeliberateStopMarker(townRoot, rigName, polecatName string) (bool, error) {
	if townRoot == "" {
		// The marker path is built from the town root; without one it would
		// land under the current directory, where no scanner looks.
		return false, fmt.Errorf("no Gas Town workspace found from %s", rigName)
	}
	if polecatSessionParked(townRoot, rigName, polecatName) {
		return false, nil
	}
	actor := agentActor(os.Getenv)
	if actor == "" {
		actor = "human operator"
	}
	// No prior agent state is recorded: the stop does not mirror the bead
	// (gt agent pause owns that), and killing a runaway session must not wait
	// on a Dolt read first.
	err := agentpause.Pause(townRoot, rigName, constants.RolePolecat, polecatName,
		deliberateSessionStopReason, actor, "")
	return err == nil, err
}

// clearParkedSession drops the pause marker that parked a polecat's session —
// written by gt session stop or gt agent pause — because an explicit start
// runs the polecat again, and the detectors should watch it again (gt-fojqs).
// Reports whether a marker was cleared. A marker that cannot be removed is
// warned about rather than silently left: it keeps the detectors away.
func clearParkedSession(townRoot, rigName, polecatName string) bool {
	if !polecatSessionParked(townRoot, rigName, polecatName) {
		return false
	}
	if err := agentpause.Resume(townRoot, rigName, constants.RolePolecat, polecatName); err != nil {
		style.PrintWarning("could not clear the pause marker for %s/%s: %v (the detectors will leave it alone)", rigName, polecatName, err)
		return false
	}
	return true
}

func runSessionAttach(cmd *cobra.Command, args []string) error {
	seat, err := resolveSessionSeat(args)
	if err != nil {
		return err
	}

	running, err := seat.Mgr.IsRunning(seat.Name)
	if err != nil {
		return fmt.Errorf("checking session: %w", err)
	}
	if !running {
		return polecat.ErrSessionNotFound
	}

	// Hand the terminal off to tmux via syscall.Exec so tmux inherits our
	// controlling TTY directly. Running tmux as a subprocess with buffered
	// stdio triggers "open terminal failed: not a terminal".
	return attachToTmuxSession(seat.Mgr.SessionName(seat.Name))
}

// SessionListItem represents a session in list output.
type SessionListItem struct {
	Rig       string `json:"rig"`
	Polecat   string `json:"polecat"`
	SessionID string `json:"session_id"`
	Running   bool   `json:"running"`
}

func runSessionList(cmd *cobra.Command, args []string) error {
	// Find town root
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Load rigs config
	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfig(rigsConfigPath)
	if err != nil {
		rigsConfig = &config.RigsConfig{Rigs: make(map[string]config.RigEntry)}
	}

	// Get all rigs
	g := git.NewGit(townRoot)
	rigMgr := rig.NewManager(townRoot, rigsConfig, g)
	rigs, err := rigMgr.DiscoverRigs()
	if err != nil {
		return fmt.Errorf("discovering rigs: %w", err)
	}

	// Filter if requested
	if sessionRigFilter != "" {
		var filtered []*rig.Rig
		for _, r := range rigs {
			if r.Name == sessionRigFilter {
				filtered = append(filtered, r)
			}
		}
		rigs = filtered
	}

	// Collect sessions from all rigs
	t := tmux.NewTmux()
	var allSessions []SessionListItem

	for _, r := range rigs {
		polecatMgr := polecat.NewSessionManager(t, r, townRegistry())
		infos, err := polecatMgr.List()
		if err != nil {
			continue
		}

		for _, info := range infos {
			allSessions = append(allSessions, SessionListItem{
				Rig:       r.Name,
				Polecat:   info.Polecat,
				SessionID: info.SessionID,
				Running:   info.Running,
			})
		}
	}

	// Output
	if sessionListJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(allSessions)
	}

	if len(allSessions) == 0 {
		fmt.Println("No active sessions.")
		return nil
	}

	fmt.Printf("%s\n\n", style.Bold.Render("Active Sessions"))
	for _, s := range allSessions {
		status := style.Bold.Render("●")
		if !s.Running {
			status = style.Dim.Render("○")
		}
		fmt.Printf("  %s %s/%s\n", status, s.Rig, s.Polecat)
		fmt.Printf("    %s\n", style.Dim.Render(s.SessionID))
	}

	return nil
}

func runSessionCapture(cmd *cobra.Command, args []string) error {
	seat, err := resolveSessionSeat(args)
	if err != nil {
		return err
	}

	// Use positional count if provided, otherwise use flag value
	lines := sessionLines
	if len(args) > 1 {
		n, err := strconv.Atoi(args[1])
		if err != nil {
			return fmt.Errorf("invalid line count '%s': must be a number", args[1])
		}
		if n <= 0 {
			return fmt.Errorf("line count must be positive, got %d", n)
		}
		lines = n
	}

	output, err := seat.Mgr.Capture(seat.Name, lines)
	if err != nil {
		return fmt.Errorf("capturing output: %w", err)
	}

	fmt.Print(output)
	return nil
}

func runSessionInject(cmd *cobra.Command, args []string) error {
	seat, err := resolveSessionSeat(args)
	if err != nil {
		return err
	}

	// Get message
	message := sessionMessage
	if sessionFile != "" {
		data, err := os.ReadFile(sessionFile)
		if err != nil {
			return fmt.Errorf("reading file: %w", err)
		}
		message = string(data)
	}

	if message == "" {
		return fmt.Errorf("no message provided (use -m or -f)")
	}

	if err := seat.Mgr.Inject(seat.Name, message); err != nil {
		return fmt.Errorf("injecting message: %w", err)
	}

	fmt.Printf("%s Message sent to %s/%s\n",
		style.Bold.Render("✓"), seat.Rig, seat.Name)
	return nil
}

func runSessionRestart(cmd *cobra.Command, args []string) error {
	// Resolve the seat before anything else: restart must refuse exactly the
	// addresses start refuses, and refuse them before it prints a "Starting
	// session for …" line that reads as a restart in progress (gt-pud2g).
	seat, err := resolveSessionSeat(args)
	if err != nil {
		return err
	}
	rigName, polecatName := seat.Rig, seat.Name
	polecatMgr := seat.Mgr

	// Check if running
	running, err := polecatMgr.IsRunning(polecatName)
	if err != nil {
		return fmt.Errorf("checking session: %w", err)
	}

	restart := func() error {
		if running {
			// Stop first
			if sessionForce {
				fmt.Printf("Force stopping session for %s/%s...\n", rigName, polecatName)
			} else {
				fmt.Printf("Stopping session for %s/%s...\n", rigName, polecatName)
			}
			if err := polecatMgr.Stop(polecatName, sessionForce); err != nil {
				return fmt.Errorf("stopping session: %w", err)
			}

			// Wait for session to fully terminate before starting a new one.
			// Without this, Start may fail or create a duplicate if the old
			// session hasn't been cleaned up by tmux yet.
			for i := 0; i < 10; i++ {
				still, _ := polecatMgr.IsRunning(polecatName)
				if !still {
					break
				}
				clockwork.NewRealClock().Sleep(200 * time.Millisecond)
			}
		}

		// Start fresh session
		fmt.Printf("Starting session for %s/%s...\n", rigName, polecatName)
		opts := polecat.SessionStartOptions{}
		if err := polecatMgr.Start(polecatName, opts); err != nil {
			return fmt.Errorf("starting session: %w", err)
		}
		return nil
	}
	if err := superviseSessionRestart(seat, running, sessionRequestedBy, restart); err != nil {
		return err
	}

	fmt.Printf("%s Session restarted. Attach with: %s\n",
		style.Bold.Render("✓"),
		style.Dim.Render(fmt.Sprintf("gt session at %s/%s", rigName, polecatName)))

	// An explicit restart is an explicit resume (gt-fojqs).
	townRoot, _ := workspace.FindFromCwd()
	if clearParkedSession(townRoot, rigName, polecatName) {
		fmt.Print(pauseClearedNotice)
	}

	// Log wake event, same as an explicit start (gt-tcrgb). A restart used to
	// leave no trace at all, so the session it created could not be
	// attributed to it: a hooked-but-idle polecat that the witness restarted
	// looked like a session appearing from nowhere, and the only visible
	// correlation left was whichever patrol happened to be running.
	if townRoot != "" {
		agent := fmt.Sprintf("%s/%s", rigName, polecatName)
		_ = logSessionWake(townRoot, agent, rigName, sessionRestartWakeContext(sessionRequestedBy))
	}

	return nil
}

// superviseSessionRestart runs a gt session restart through the supervisor
// (gt-4k3fj.4.1). Replacing a running session is a Respawn, which an e-stop,
// a park or a gt down in progress refuses; the stop inside it kills straight
// through tmux, since the Respawn is the restart's one record. With no
// session running it is a start: a dead session is replaced through Start's
// own Respawn hook and a fresh one needs no record. When the daemon's
// restart executor asks (--requested-by daemon/patrol-scan), supervisor.Restart
// has already guarded, budgeted and recorded this restart, so it runs with
// only the Cleanup hook rather than being recorded twice.
func superviseSessionRestart(seat sessionSeat, running bool, requestedBy string, restart func() error) error {
	actor := operatorActor("gt session restart")
	if requestedBy != "" {
		actor = operatorActorFor("gt session restart", requestedBy)
	}
	return superviseSessionRestartWith(operatorSupervisor(filepath.Dir(seat.r.Path)), townRegistry(), seat, running, requestedBy, actor, restart)
}

// superviseSessionRestartWith is superviseSessionRestart over sup and reg.
func superviseSessionRestartWith(sup *supervisor.Supervisor, reg *session.PrefixRegistry, seat sessionSeat, running bool, requestedBy, actor string, restart func() error) error {
	hooks := polecatSessionHooks(sup, reg, seat.Rig, "session restart", actor)
	hooks.Stop = nil
	if requestedBy == patrolscan.Actor {
		hooks.Respawn = nil
		seat.Mgr.SetHooks(hooks)
		return restart()
	}
	seat.Mgr.SetHooks(hooks)
	if !running {
		return restart()
	}
	return sup.Respawn(supervisor.SeatIn(reg, seat.Rig, constants.RolePolecat, seat.Name), "session restart", actor, restart)
}

// sessionRestartWakeContext is the parenthetical of the restart path's wake
// line. `gt session start` logs a bare `resumed (<issue>)`; a restart logging
// the same shape would be indistinguishable from an operator's own start, so
// the restart names itself and, when the caller identified itself, who asked
// for it (gt-tcrgb).
func sessionRestartWakeContext(requestedBy string) string {
	if requestedBy = strings.TrimSpace(requestedBy); requestedBy != "" {
		return "restart requested by " + requestedBy
	}
	return "restart"
}

func runSessionStatus(cmd *cobra.Command, args []string) error {
	// Resolve the seat first. A status answer is read as a fact about a seat
	// that exists: without this, `gt session status gastown/<gone>` reported
	// "State: ○ stopped" with exit 0 for a polecat that was never there,
	// indistinguishable from a real polecat between restarts (gt-pud2g).
	seat, err := resolveSessionSeat(args)
	if err != nil {
		return err
	}
	rigName, polecatName := seat.Rig, seat.Name

	info, err := seat.Mgr.Status(polecatName)
	if err != nil {
		return fmt.Errorf("getting status: %w", err)
	}

	if sessionStatusJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(info)
	}

	fmt.Printf("%s Session: %s/%s\n\n", style.Bold.Render("📺"), rigName, polecatName)

	if info.Running {
		fmt.Printf("  State: %s\n", style.Bold.Render("● running"))
	} else {
		fmt.Printf("  State: %s\n", style.Dim.Render("○ stopped"))
		return nil
	}

	fmt.Printf("  Session ID: %s\n", info.SessionID)

	if info.Attached {
		fmt.Printf("  Attached: yes\n")
	} else {
		fmt.Printf("  Attached: no\n")
	}

	if !info.Created.IsZero() {
		uptime := time.Since(info.Created)
		fmt.Printf("  Created: %s\n", info.Created.Format("2006-01-02 15:04:05"))
		fmt.Printf("  Uptime: %s\n", formatDuration(uptime))
	}

	fmt.Printf("\nAttach with: %s\n", style.Dim.Render(fmt.Sprintf("gt session at %s/%s", rigName, polecatName)))
	return nil
}

// formatDuration formats a duration for human display.
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	hours := int(d.Hours())
	mins := int(d.Minutes()) % 60
	if hours >= 24 {
		days := hours / 24
		hours = hours % 24
		return fmt.Sprintf("%dd %dh %dm", days, hours, mins)
	}
	return fmt.Sprintf("%dh %dm", hours, mins)
}
