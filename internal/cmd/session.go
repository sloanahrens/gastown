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
	sessionPark        bool
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

Addresses are <rig>/<name>, where <name> is one of the rig's polecats. An
address that names no polecat is refused as not found by every subcommand, so
a session that does not exist is never reported as a stopped or restarted one.

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
and reporting a crash the operator asked for. A host seat stays stopped
until gt session start (or gt agent resume) clears the record.

A sandbox seat's stop also deletes its sandbox, and the seat is free for
the next sling unless --park is given: parking is opt-in there. If the
sandbox registry cannot be read, the seat cannot be shown to be a sandbox
seat, so the stop parks it and warns: a restart of a seat the operator
stopped is worse than a park, which gt session start or gt agent resume
lifts.`,
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
	sessionStopCmd.Flags().BoolVar(&sessionPark, "park", false,
		"Park the seat as well as stopping it (a sandbox seat parks only with this flag)")

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
		"Caller performing an automated restart (e.g. \"daemon/patrol-scan\"); recorded in the town log wake line")

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

// startSeatSession starts a seat's session on the backend its slung agent names,
// in the town the seat belongs to, so an openshell seat starts inside its
// sandbox or is refused, never on host tmux with the host worktree and
// file-backed secrets (gt-zvuyn).
func startSeatSession(seat sessionSeat, starter polecatStarter, opts polecat.SessionStartOptions) error {
	// The seat's own town, not the caller's, holds the settings that place its
	// recorded agent (townSeatBackend).
	seatTownRoot := filepath.Dir(seat.r.Path)
	if err := restartPolecatOnSlungAgent(starter, townSeatBackend(seatTownRoot, tmux.NewTmux()), seatTownRoot, seat.Rig, seat.Name, opts); err != nil {
		return fmt.Errorf("starting session: %w", err)
	}
	return nil
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
	if err := startSeatSession(seat, seat.Mgr, opts); err != nil {
		return err
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
	polecatMgr := newStopSessions(r)

	townRoot, _ := workspace.FindFromCwd()

	if sessionForce {
		fmt.Printf("Force stopping session for %s/%s...\n", rigName, polecatName)
	} else {
		fmt.Printf("Stopping session for %s/%s...\n", rigName, polecatName)
	}

	// Record the stop before killing the session: a patrol reading between
	// the two would see a session-dead polecat with no record that the stop
	// was deliberate, and restart it (gt-fojqs).
	var recorded stopRecord
	if polecatMgr.HasPolecat(polecatName) {
		recorded = recordDeliberateStop(townRoot, rigName, polecatName, polecatMgr.SessionName(polecatName), sessionPark, os.Stderr)
	}

	if err := polecatMgr.Stop(polecatName, sessionForce); err != nil {
		// A stop that wraps polecat.ErrSandboxNotRemoved ended the session and
		// left only its sandbox behind, so the marker this run wrote stays:
		// withdrawing it hands the seat back to the patrol, which restarts a
		// polecat the operator stopped (gt-nbo3y). Any other error means
		// nothing was stopped, so a marker this run wrote is withdrawn rather
		// than leave the polecat looking deliberately stopped.
		if !errors.Is(err, polecat.ErrSandboxNotRemoved) && recorded.Written {
			clearParkedSession(townRoot, rigName, polecatName)
		}
		return fmt.Errorf("stopping session: %w", err)
	}

	fmt.Printf("%s Session stopped.\n", style.Bold.Render("✓"))
	switch {
	case recorded.Written:
		fmt.Printf("  Recorded as a deliberate stop — the daemon's supervised restart will leave it down.\n")
		fmt.Printf("  Resume with: %s\n", style.Dim.Render("gt session start "+rigName+"/"+polecatName))
	case recorded.Sandbox && !recorded.Parked:
		fmt.Printf("  Sandbox seat stopped and not parked: the next sling can reuse it. Add %s to park it.\n",
			style.Dim.Render("--park"))
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

// stopSeatManager is the part of a rig's polecat session manager that gt
// session stop drives. Naming the three calls lets a test hand the stop a
// manager whose Stop fails, so the stop's own handling of a failed stop runs
// without tmux or a sandbox (gt-nbo3y).
type stopSeatManager interface {
	HasPolecat(polecat string) bool
	SessionName(polecat string) string
	Stop(polecat string, force bool) error
}

// newStopSessions builds the session manager gt session stop drives.
var newStopSessions = func(r *rig.Rig) stopSeatManager {
	return supervisedPolecatSessions(tmux.NewTmux(), r, "session stop", operatorActor("gt session stop"))
}

// deliberateSessionStopReason is the pause-marker reason gt session stop
// writes. Reasons are shown verbatim by gt status and by scanner logs, so it
// names the command that wrote it.
const deliberateSessionStopReason = "deliberate stop (gt session stop)"

// pauseClearedNotice is printed by the two starts that clear a parked
// session's pause marker: gt session start and gt session restart.
const pauseClearedNotice = "  Cleared the pause marker — the daemon's supervised restart will bring this session back if it dies again.\n"

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

// stopRecord is what recording an operator's gt session stop left on the seat.
type stopRecord struct {
	// Written is true when this stop wrote the park marker.
	Written bool
	// Parked is true when the seat carries a park marker after the stop, from
	// this stop or from an earlier gt agent pause. A marker this stop failed
	// to write leaves it false: the seat is not parked.
	Parked bool
	// Sandbox is true when the seat's session lives in a sandbox.
	Sandbox bool
}

// recordDeliberateStop records an operator's stop so the patrol leaves the seat
// alone (gt-fojqs). A sandbox seat parks only when park is set: its sandbox is
// deleted on every stop and the seat is there to be slung again, so a park
// would only keep a reusable seat out of the pool (gt-itmpq). A host seat
// parks on every stop.
//
// A registry that cannot be read is not read as "host seat": the seat is
// parked and the warning names the failed read, because a restart of a seat
// the operator stopped is the incident and a park is the recoverable side.
// Warnings go to warn, which gt session stop points at stderr, so a test can
// read them.
func recordDeliberateStop(townRoot, rigName, polecatName, sessionID string, park bool, warn io.Writer) stopRecord {
	sandboxed, err := session.IsSandboxed(townRoot, sessionID)
	if err != nil {
		style.FprintWarning(warn, "could not tell whether %s runs in a sandbox: %v; recording the stop as a park", sessionID, err)
	}
	parks := stopParksSeat(sandboxed, err != nil, park)
	rec := stopRecord{Sandbox: err == nil && sandboxed}
	if !parks {
		// The sandbox seat is left alone, but an earlier park (gt agent
		// pause) still sits on it; calling the seat unparked would tell the
		// operator the next sling can reuse it while the reuse gate refuses.
		rec.Parked = polecatSessionParked(townRoot, rigName, polecatName)
		return rec
	}
	wrote, werr := writeDeliberateStopMarker(townRoot, rigName, polecatName)
	if werr != nil {
		style.FprintWarning(warn, "could not record the stop for %s/%s: %v (the daemon's supervised restart may bring it back)", rigName, polecatName, werr)
	}
	rec.Written = wrote
	// A marker already on the seat parks it too (an earlier gt agent pause
	// keeps its own reason); a failed write leaves no marker, so Parked stays
	// false and the warning above is the only record that the seat is exposed.
	rec.Parked = wrote || werr == nil
	return rec
}

// stopParksSeat reports whether a stop parks the seat: --park asks for it, a
// host seat parks on every stop as it always has, and a registry read that
// failed parks too, because a park is the recoverable side of the mistake.
func stopParksSeat(sandboxed, readFailed, park bool) bool {
	return park || readFailed || !sandboxed
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

// polecatSessionLister is the slice of polecat.SessionManager that gt session list
// reads. List returns every session carrying the rig's prefix, crew included;
// ListPolecats returns the polecat seats only.
type polecatSessionLister interface {
	List() ([]polecat.SessionInfo, error)
	ListPolecats() ([]polecat.SessionInfo, error)
}

// listPolecatSessions collects the polecat sessions of rigs, skipping a rig
// whose sessions cannot be listed. It asks ListPolecats, not List: a crew
// session such as gt-crew-sloan carries the rig prefix too, and listing it as
// polecat crew-sloan points `gt session stop` at a seat that does not exist.
func listPolecatSessions(rigs []*rig.Rig, open func(*rig.Rig) polecatSessionLister) []SessionListItem {
	var items []SessionListItem
	for _, r := range rigs {
		infos, err := open(r).ListPolecats()
		if err != nil {
			continue
		}
		for _, info := range infos {
			items = append(items, SessionListItem{
				Rig:       r.Name,
				Polecat:   info.Polecat,
				SessionID: info.SessionID,
				Running:   info.Running,
			})
		}
	}
	return items
}

// polecatSessionManagers opens the polecat.SessionManager that gt session list
// reads for each rig.
func polecatSessionManagers(t *tmux.Tmux) func(*rig.Rig) polecatSessionLister {
	return func(r *rig.Rig) polecatSessionLister {
		return polecat.NewSessionManager(t, r, townRegistry())
	}
}

func runSessionList(cmd *cobra.Command, args []string) error {
	// Find town root
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	// Load rigs config
	rigsConfigPath := filepath.Join(townRoot, "mayor", "rigs.json")
	rigsConfig, err := config.LoadRigsConfigOrEmpty(rigsConfigPath)
	if err != nil {
		return fmt.Errorf("loading rigs config: %w", err)
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
	allSessions := listPolecatSessions(rigs, polecatSessionManagers(t))

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

	// The seat's town root: the restart resolves the seat's agent from the
	// intent record there, and its backend from the same town's settings.
	seatTownRoot := filepath.Dir(seat.r.Path)

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
		// The seat comes back on the agent its sling recorded, not the role
		// default (gt-rsrnr), on that agent's backend (gt-da3ea).
		backendFor := townSeatBackend(seatTownRoot, tmux.NewTmux())
		if err := restartPolecatOnSlungAgent(polecatMgr, backendFor, seatTownRoot, rigName, polecatName, polecat.SessionStartOptions{}); err != nil {
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
