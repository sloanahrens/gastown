package cmd

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/mail"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/ui"
	"github.com/steveyegge/gastown/internal/workspace"
)

var handoffCmd = &cobra.Command{
	Use:     "handoff [bead-or-role]",
	GroupID: GroupWork,
	Short:   "Hand off to a fresh session, work continues from hook",
	Long: `End watch. Hand off to a fresh agent session.

This is the canonical way to end any agent session. It handles all roles:

  - Crew: Respawns with fresh Claude instance
  - Polecats: Calls 'gt done --status DEFERRED'

When run without arguments, hands off the current session.
When given a bead ID (gt-xxx, hq-xxx), hooks that work first, then restarts.
When given a role name, hands off that role's session (and switches to it).

Examples:
  gt handoff                          # Hand off current session
  gt handoff gt-abc                   # Hook bead, then restart
  gt handoff gt-abc -s "Fix it"       # Hook with context, then restart
  gt handoff -s "Context" -m "Notes"  # Hand off with custom message
  gt handoff -c                       # Collect state into handoff message
  gt handoff crew                     # Hand off crew session

The --collect (-c) flag gathers current state (hooked work, inbox, ready beads,
in-progress items) and includes it in the handoff mail. This provides context
for the next session without manual summarization.

The --cycle flag triggers automatic session cycling (used by PreCompact hooks).
Unlike --auto (state only) or normal handoff (polecat→gt-done redirect), --cycle
always does a full respawn regardless of role. This enables crew workers and
polecats to get a fresh context window when the current one fills up.

Any molecule on the hook will be auto-continued by the new session.
The SessionStart hook runs 'gt prime' to restore context.`,
	RunE: runHandoff,
}

var (
	handoffWatch      bool
	handoffDryRun     bool
	handoffSubject    string
	handoffMessage    string
	handoffCollect    bool
	handoffStdin      bool
	handoffAuto       bool
	handoffCycle      bool
	handoffReason     string
	handoffNoGitCheck bool
	handoffYes        bool
)

// handoffSteps are the collaborators of handoffSession that reach tmux, the
// mail store, the beads store or the town feed. runHandoff passes
// liveHandoffSteps; a unit test passes fakes and records the calls in order.
// Each field is one call a self-handoff makes that a test cannot let run: the
// handoffSession body is runHandoff's tail moved over unchanged, so the order
// of the calls stays where it was.
type handoffSteps struct {
	buildPlan func(sessionName string, opts buildRestartCommandOpts) (restartPlan, error)
	// updateSessionEnv refreshes sessionName's tmux env from lookupEnv, the
	// process environment its restart command was built from.
	updateSessionEnv func(reg *session.PrefixRegistry, t *tmux.Tmux, sessionName string, lookupEnv func(string) (string, bool))
	// remoteOps are the tmux calls of a handoff to another session, on the
	// town socket townTmux.
	remoteOps func(townTmux *tmux.Tmux) remoteHandoffOps
	// cleanupMolecule closes the molecule steps the session leaves behind.
	cleanupMolecule func()
	sendMail        func(subject, message string) (string, error)
	// logNoPersist records that the handoff mail did not persist.
	logNoPersist func(agent, subject string, err error)
}

var liveHandoffSteps = handoffSteps{
	buildPlan:        buildRestartPlan,
	updateSessionEnv: updateSessionEnvForHandoff,
	remoteOps:        liveRemoteHandoffOps,
	cleanupMolecule:  cleanupMoleculeOnHandoff,
	sendMail:         sendHandoffMail,
	logNoPersist: func(agent, subject string, err error) {
		if townRoot, trErr := workspace.FindFromCwd(); trErr == nil && townRoot != "" {
			_ = events.LogFeedTo(townRoot, events.TypeHandoffNoPersist, agent,
				events.HandoffFailedPayload(subject, err))
		}
	},
}

// sessionHandoff is what handoffSession hands off: the session the caller is
// in, the session to restart (the same, for a self-handoff) and the mail.
type sessionHandoff struct {
	reg            *session.PrefixRegistry
	t              *tmux.Tmux // the caller's server, for pane operations
	townTmux       *tmux.Tmux // the town socket, for a remote session
	pane           string
	currentSession string
	targetSession  string
	dryRun         bool
	watch          bool // switch the caller's client to a remote target
	subject        string
	message        string
}

func init() {
	handoffCmd.Flags().BoolVarP(&handoffWatch, "watch", "w", true, "Switch to new session (for remote handoff)")
	handoffCmd.Flags().BoolVarP(&handoffDryRun, "dry-run", "n", false, "Show what would be done without executing")
	handoffCmd.Flags().StringVarP(&handoffSubject, "subject", "s", "", "Subject for handoff mail (optional)")
	handoffCmd.Flags().StringVarP(&handoffMessage, "message", "m", "", "Message body for handoff mail (optional)")
	handoffCmd.Flags().BoolVarP(&handoffCollect, "collect", "c", false, "Auto-collect state (status, inbox, beads) into handoff message")
	handoffCmd.Flags().BoolVar(&handoffStdin, "stdin", false, "Read message body from stdin (avoids shell quoting issues)")
	handoffCmd.Flags().BoolVar(&handoffAuto, "auto", false, "Save state only, no session cycling (for PreCompact hooks)")
	handoffCmd.Flags().BoolVar(&handoffCycle, "cycle", false, "Auto-cycle session (for PreCompact hooks that want full session replacement)")
	handoffCmd.Flags().StringVar(&handoffReason, "reason", "", "Reason for handoff (e.g., 'compaction', 'idle')")
	handoffCmd.Flags().BoolVar(&handoffNoGitCheck, "no-git-check", false, "Skip git workspace cleanliness check")
	handoffCmd.Flags().BoolVarP(&handoffYes, "yes", "y", false, "Skip confirmation prompt (for automation and scripting)")
	rootCmd.AddCommand(handoffCmd)
}

// handoffMessageInput is the handoff message: messageFlag, or with --stdin
// what readStdin returns. The two cannot be combined.
func handoffMessageInput(messageFlag string, stdin bool, readStdin func() ([]byte, error)) (string, error) {
	if !stdin {
		return messageFlag, nil
	}
	if messageFlag != "" {
		return "", fmt.Errorf("cannot use --stdin with --message/-m")
	}
	data, err := readStdin()
	if err != nil {
		return "", fmt.Errorf("reading stdin: %w", err)
	}
	return strings.TrimRight(string(data), "\n"), nil
}

// handoffPolecat reports whether the handing-off session is a polecat, which
// hands off through gt done instead, and its name. GT_ROLE decides first:
// coordinators (witness, etc.) may have a stale GT_POLECAT in their
// environment from spawning polecats. Only a role that parses as polecat
// counts (handles compound forms like "gastown/polecats/Toast"). If GT_ROLE
// is unset, GT_POLECAT decides.
func handoffPolecat(getenv func(string) string) (isPolecat bool, name string) {
	if role := getenv("GT_ROLE"); role != "" {
		parsedRole, _, roleName := parseRoleString(role)
		if parsedRole != RolePolecat {
			return false, ""
		}
		// Bare "polecat" role yields empty name; fall back to GT_POLECAT.
		if roleName == "" {
			roleName = getenv("GT_POLECAT")
		}
		return true, roleName
	}
	if name := getenv("GT_POLECAT"); name != "" {
		return true, name
	}
	return false, ""
}

// polecatHandoffDoneCmd is the gt done a polecat's handoff runs, in the
// environment environ. Polecats don't respawn themselves - Witness handles
// lifecycle - so it calls gt done with DEFERRED status to preserve work
// state, marked as handoff-originated so gt done preserves the session
// instead of retiring it (gt-5g3e): a mid-work handoff must keep the polecat
// (or its successor) going, exactly as polecat-CLAUDE.md promises.
func polecatHandoffDoneCmd(environ []string) *exec.Cmd {
	doneCmd := exec.Command("gt", "done", "--status", "DEFERRED")
	doneCmd.Env = append(environ, envDoneFromHandoff+"=1")
	return doneCmd
}

// polecatHandoff is the polecat branch of runHandoff. It reports handled=false
// when the caller is not a polecat. For a polecat it prints the plan and, unless
// dryRun, hands the gt done command to run. getenv, environ, out and run are
// injected so a unit test can observe whether the command ran.
func polecatHandoff(getenv func(string) string, environ []string, dryRun bool, out io.Writer, run func(*exec.Cmd) error) (handled bool, err error) {
	isPolecat, polecatName := handoffPolecat(getenv)
	if !isPolecat {
		return false, nil
	}
	fmt.Fprintf(out, "%s Polecat detected (%s) - using gt done for handoff\n",
		style.Bold.Render("🐾"), polecatName)
	doneCmd := polecatHandoffDoneCmd(environ)
	if dryRun {
		fmt.Fprintf(out, "Would execute: %s\n", strings.Join(doneCmd.Args, " "))
		return true, nil
	}
	return true, run(doneCmd)
}

// runPolecatDone runs the gt done a polecat's handoff hands it, wired to this
// process's stdout and stderr. A package variable so a test of runHandoff can
// count the runs instead of ending its own session.
var runPolecatDone = func(doneCmd *exec.Cmd) error {
	doneCmd.Stdout = os.Stdout
	doneCmd.Stderr = os.Stderr
	return doneCmd.Run()
}

func runHandoff(cmd *cobra.Command, args []string) error {
	reg := townRegistry()
	// Handle --stdin: read message body from stdin (avoids shell quoting issues)
	msg, err := handoffMessageInput(handoffMessage, handoffStdin, func() ([]byte, error) { return io.ReadAll(os.Stdin) })
	if err != nil {
		return err
	}
	handoffMessage = msg

	// --auto mode: save state only, no session cycling.
	// Used by PreCompact hook to preserve state before compaction.
	// Note: auto-mode exits here, before the git-status warning check below.
	// This is intentional — auto-handoffs are triggered by hooks and should not
	// spam warnings. The --no-git-check flag has no effect in auto mode.
	if handoffAuto {
		return runHandoffAuto()
	}

	// --cycle mode: full session cycling, triggered by PreCompact hook.
	// Unlike --auto (state only), this replaces the current session with a fresh one.
	// Unlike normal handoff, this skips the polecat→gt-done redirect because
	// cycling preserves work state (the hook stays attached).
	//
	// Flow: collect state → send handoff mail → respawn pane (fresh Claude instance)
	// The successor session picks up hooked work via SessionStart hook (gt prime --hook).
	if handoffCycle {
		return runHandoffCycle()
	}

	// Check if we're a polecat - polecats use gt done instead.
	// Check GT_ROLE first: coordinators (witness, etc.) may have a stale
	// GT_POLECAT in their environment from spawning polecats. Only block if the
	// parsed role is actually polecat (handles compound forms like
	// "gastown/polecats/Toast"). If GT_ROLE is unset, fall back to GT_POLECAT.
	handled, err := polecatHandoff(os.Getenv, os.Environ(), handoffDryRun, cmd.OutOrStdout(), runPolecatDone)
	if handled {
		return err
	}

	// Prompt for confirmation unless --yes/-y was passed or stdin is not a TTY.
	// Only interactive (human) sessions get prompted; agent automation proceeds
	// without blocking on stdin (gas-6z0).
	if !handoffYes && !handoffDryRun && term.IsTerminal(int(os.Stdin.Fd())) {
		if !promptYesNo("Ready to hand off? This will restart the session.") {
			fmt.Println("Handoff canceled.")
			return nil
		}
	}

	// Enforce minimum handoff cooldown to prevent tight restart loops (gt-058d).
	// When a patrol agent (e.g., witness) completes quickly on idle rigs,
	// it can hand off immediately and the daemon respawns, creating a crash loop.
	enforceHandoffCooldown()

	// If --collect flag is set, auto-collect state into the message
	if handoffCollect {
		collected := collectHandoffState()
		if handoffMessage == "" {
			handoffMessage = collected
		} else {
			handoffMessage = handoffMessage + "\n\n---\n" + collected
		}
		if handoffSubject == "" {
			handoffSubject = "Session handoff with context"
		}
	}

	// Use a socket-aware Tmux for pane operations. The calling process may be
	// on a different tmux server than the town socket (e.g., default socket).
	// For self-handoff, pane operations (clear-history, respawn-pane) must target
	// the caller's own server. SocketFromEnv() reads $TMUX to find the right one.
	callerSocket := tmux.SocketFromEnv()
	t := tmux.NewTmuxWithSocket(callerSocket)
	// Town-socket Tmux for session-level queries (getSessionPane, etc.)
	townTmux := tmux.NewTmux()
	_ = townTmux // used later for remote handoff

	// Verify we're in tmux
	if !tmux.IsInsideTmux() {
		return fmt.Errorf("not running in tmux - cannot hand off")
	}

	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return fmt.Errorf("TMUX_PANE not set - cannot hand off")
	}

	// Get current session name from GT_ROLE (preferred) or tmux display-message.
	currentSession, err := getCurrentTmuxSession()
	if err != nil {
		return fmt.Errorf("getting session name: %w", err)
	}

	// Warn if workspace has uncommitted or unpushed work (wa-7967c).
	// Note: this checks the caller's cwd, not the target session's workdir.
	// For remote handoff (gt handoff <role>), the warning reflects the caller's
	// workspace state. Checking the target session's workdir would require tmux
	// pane introspection and is deferred to a future enhancement.
	if !handoffNoGitCheck {
		warnHandoffGitStatus()
	}

	// Determine target session and check for bead hook
	targetSession := currentSession
	if len(args) > 0 {
		arg := args[0]

		// Check if arg is a bead ID (gt-xxx, hq-xxx, bd-xxx, etc.)
		if looksLikeHandoffBeadID(reg, arg) {
			// Hook the bead first
			if err := hookBeadForHandoff(arg); err != nil {
				return fmt.Errorf("hooking bead: %w", err)
			}
			// Update subject if not set
			if handoffSubject == "" {
				handoffSubject = fmt.Sprintf("🪝 HOOKED: %s", arg)
			}
		} else {
			// User specified a role to hand off
			targetSession, err = resolveRoleToSession(reg, arg)
			if err != nil {
				return fmt.Errorf("resolving role: %w", err)
			}
		}
	}

	return handoffSession(sessionHandoff{
		reg:            reg,
		t:              t,
		townTmux:       townTmux,
		pane:           pane,
		currentSession: currentSession,
		targetSession:  targetSession,
		dryRun:         handoffDryRun,
		watch:          handoffWatch,
		subject:        handoffSubject,
		message:        handoffMessage,
	}, liveHandoffSteps)
}

// handoffSession restarts p.targetSession: another session through
// runRemoteHandoff, or the caller's own after a dry-run preview, the
// molecule cleanup and the handoff mail.
func handoffSession(p sessionHandoff, steps handoffSteps) error {
	reg, t, townTmux, pane := p.reg, p.t, p.townTmux, p.pane
	currentSession, targetSession := p.currentSession, p.targetSession

	// A restart of another session runs that session's agent, so it is built
	// without the caller's GT_AGENT, GT_AGENT_OVERRIDE and GT_PROCESS_NAMES:
	// they describe the caller's agent, and the session env fallback supplies
	// the target's own.
	remote := targetSession != currentSession
	lookupEnv := os.LookupEnv
	if remote {
		lookupEnv = remoteHandoffEnv(os.LookupEnv)
	}

	// Build the restart command
	plan, err := steps.buildPlan(targetSession, buildRestartCommandOpts{Registry: reg, LookupEnv: lookupEnv})
	if err != nil {
		return err
	}
	restartCmd := plan.Command

	// If handing off a different session, we need to find its pane and respawn there.
	// Remote sessions live on the town socket, so use townTmux for their operations.
	if remote {
		// The session env update is a side effect, so it runs inside the
		// remote handoff after the dry-run check, not before it.
		return runRemoteHandoff(steps.remoteOps(townTmux), targetSession, plan, p.dryRun, p.watch, func() {
			steps.updateSessionEnv(reg, townTmux, targetSession, lookupEnv)
		})
	}

	// Handing off ourselves - print feedback then respawn
	fmt.Printf("%s Handing off %s...\n", style.Bold.Render("🤝"), currentSession)

	// Resolve agent identity once for both success and failure paths.
	agent := sessionToGTRole(reg, currentSession)
	if agent == "" {
		agent = currentSession
	}

	// Dry run mode - show what would happen (BEFORE any side effects)
	if p.dryRun {
		if p.subject != "" || p.message != "" {
			fmt.Printf("Would send handoff mail: subject=%q (auto-hooked)\n", p.subject)
		}
		fmt.Printf("Would execute: tmux clear-history -t %s\n", pane)
		fmt.Printf("Would execute: tmux respawn-pane -k -t %s %s\n", pane, restartCmd)
		return nil
	}

	// Close any in-progress molecule steps before cycling (gt-e26g).
	// Without this, patrol agents that handoff mid-cycle leak orphaned wisps.
	// Below the dry-run guard: a preview must not close the molecule.
	steps.cleanupMolecule()

	// Update tmux session environment for liveness detection.
	// IsAgentAliveChecked reads GT_PROCESS_NAMES via tmux show-environment (session env),
	// not from shell exports. The restart command sets shell exports for the child
	// process, but we must also update the session env so liveness checks work.
	// Placed after the dry-run guard to avoid mutating session state during dry-run.
	steps.updateSessionEnv(reg, t, currentSession, lookupEnv)

	// Send handoff mail to self (defaults applied inside sendHandoffMail).
	// The mail is auto-hooked so the next session picks it up.
	// CRITICAL: Mail must persist to Dolt BEFORE logging the handoff event.
	// If Dolt is down, we must NOT log a handoff that did not happen.
	beadID, err := steps.sendMail(p.subject, p.message)
	if err != nil {
		// Handoff persistence failure is fatal — do not silently continue.
		// A silent failure causes the next session to find an empty hook,
		// losing all handoff context.
		steps.logNoPersist(agent, p.subject, err)
		fmt.Fprintf(os.Stderr, "The session was NOT respawned. Fix the issue and retry 'gt handoff'.\n")
		return fmt.Errorf("handoff mail failed to persist (Dolt may be down): %w", err)
	}
	fmt.Printf("%s Sent handoff mail %s (auto-hooked)\n", style.Bold.Render("📬"), beadID)

	// Log handoff event AFTER Dolt persistence succeeds.
	if townRoot, err := workspace.FindFromCwd(); err == nil && townRoot != "" {
		_ = events.LogFeed(events.TypeHandoff, agent, events.HandoffPayload(p.subject, true))
	}

	// NOTE: reportAgentState("stopped") removed (gt-zecmc)
	// Agent liveness is observable from tmux - no need to record it in bead.
	// "Discover, don't track" principle: reality is truth, state is derived.

	// Write handoff marker for successor detection (prevents handoff loop bug).
	// The marker is cleared by gt prime after it outputs the warning.
	// This tells the new session "you're post-handoff, don't re-run /handoff"
	// Both go under the successor's workDir, which may differ from our cwd.
	// Record handoff time for cooldown enforcement (gt-058d).
	cwd, _ := os.Getwd()
	recordHandoffState(plan, cwd, currentSession, "")

	// NOTE: For self-handoff, we do NOT call KillPaneProcesses here.
	// That would kill the gt handoff process itself before it can call RespawnPane,
	// leaving the pane dead with no respawn. RespawnPane's -k flag handles killing
	// atomically - tmux kills the old process and spawns the new one together.
	// See: https://github.com/steveyegge/gastown/issues/859 (pane is dead bug)
	//
	// For orphan prevention, we rely on respawn-pane -k which sends SIGHUP/SIGTERM.
	// If orphans still occur, the solution is to adjust the restart command to
	// kill orphans at startup, not to kill ourselves before respawning.
	return superviseHandoff(currentSession, "gt handoff", "handoff", func() error {
		plan.syncSettings()
		return respawnOwnPane(t, currentSession, pane, restartCmd)
	})
}

// runHandoffAuto saves state without cycling the session.
// Used by the PreCompact hook to preserve context before compaction.
// No tmux required — just collects state, sends handoff mail, and writes marker.
func runHandoffAuto() error {
	// Build subject
	subject := handoffSubject
	if subject == "" {
		reason := handoffReason
		if reason == "" {
			reason = "auto"
		}
		subject = fmt.Sprintf("🤝 HANDOFF: %s", reason)
	}

	// Auto-collect state if no explicit message
	message := handoffMessage
	if message == "" {
		message = collectHandoffState()
	}

	if handoffDryRun {
		fmt.Printf("[auto-handoff] Would send mail: subject=%q\n", subject)
		fmt.Printf("[auto-handoff] Would write handoff marker\n")
		return nil
	}

	// Close any in-progress molecule steps before state save (gt-e26g).
	cleanupMoleculeOnHandoff()

	// Send handoff mail to self
	beadID, err := sendHandoffMail(subject, message)
	if err != nil {
		// Non-fatal — log and continue
		fmt.Fprintf(os.Stderr, "auto-handoff: could not send mail: %v\n", err)
	} else {
		fmt.Fprintf(os.Stderr, "auto-handoff: saved state to %s\n", beadID)
	}

	// Write handoff marker so post-compact prime knows it's post-handoff
	sessionName := "auto-handoff"
	cwd, _ := os.Getwd()
	stateDir := cwd
	if tmux.IsInsideTmux() {
		if name, err := getCurrentTmuxSession(); err == nil {
			sessionName = name
			stateDir = autoHandoffStateDir(townRegistry(), name, detectTownRootFromCwd(), cwd)
		}
	}
	if stateDir != "" {
		writeHandoffMarker(stateDir, sessionName, "")
	}

	// Log handoff event
	if townRoot, err := workspace.FindFromCwd(); err == nil && townRoot != "" {
		agent := detectSender()
		if agent == "" || agent == "overseer" {
			agent = "unknown"
		}
		_ = events.LogFeed(events.TypeHandoff, agent, events.HandoffPayload(subject, false))
	}

	return nil
}

// runHandoffCycle performs a full session cycle — save state AND respawn.
// This is the PreCompact-triggered session succession mechanism (gt-op78).
//
// Unlike --auto (state only) or normal handoff (polecat→gt-done redirect),
// --cycle always does a full respawn regardless of role. This enables
// crew workers (and polecats) to get a fresh context window when the
// current one fills up.
//
// The flow:
//  1. Auto-collect state (inbox, ready beads, hooked work)
//  2. Send handoff mail to self (auto-hooked for successor)
//  3. Write handoff marker (prevents handoff loop)
//  4. Respawn the tmux pane with a fresh Claude instance
//
// The successor session starts via SessionStart hook (gt prime --hook),
// finds the hooked work, and continues from where we left off.
func runHandoffCycle() error {
	reg := townRegistry()
	// Build subject
	subject := handoffSubject
	if subject == "" {
		reason := handoffReason
		if reason == "" {
			reason = "context-cycle"
		}
		subject = fmt.Sprintf("🤝 HANDOFF: %s", reason)
	}

	// Auto-collect state if no explicit message
	message := handoffMessage
	if message == "" {
		message = collectHandoffState()
	}

	// Must be in tmux to respawn
	if !tmux.IsInsideTmux() {
		// Fall back to auto mode (save state only) if not in tmux
		fmt.Fprintf(os.Stderr, "handoff --cycle: not in tmux, falling back to state-save only\n")
		handoffMessage = message
		handoffSubject = subject
		return runHandoffAuto()
	}

	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		fmt.Fprintf(os.Stderr, "handoff --cycle: TMUX_PANE not set, falling back to state-save only\n")
		handoffMessage = message
		handoffSubject = subject
		return runHandoffAuto()
	}

	currentSession, err := getCurrentTmuxSession()
	if err != nil {
		fmt.Fprintf(os.Stderr, "handoff --cycle: could not get session: %v, falling back to state-save only\n", err)
		handoffMessage = message
		handoffSubject = subject
		return runHandoffAuto()
	}

	// Use the caller's socket for pane operations (same as runHandoff).
	callerSocket := tmux.SocketFromEnv()
	t := tmux.NewTmuxWithSocket(callerSocket)

	if handoffDryRun {
		fmt.Printf("[cycle] Would send handoff mail: subject=%q\n", subject)
		fmt.Printf("[cycle] Would write handoff marker\n")
		fmt.Printf("[cycle] Would execute: tmux clear-history -t %s\n", pane)
		fmt.Printf("[cycle] Would execute: tmux respawn-pane -k -t %s <restart-cmd>\n", pane)
		return nil
	}

	// Close any in-progress molecule steps before cycling (gt-e26g).
	cleanupMoleculeOnHandoff()

	// Send handoff mail to self (auto-hooked for successor).
	// Fatal on failure — same rationale as runHandoff: silent failure causes
	// the next session to find an empty hook and lose all context.
	beadID, err := sendHandoffMail(subject, message)
	if err != nil {
		agent := sessionToGTRole(reg, currentSession)
		if agent == "" {
			agent = currentSession
		}
		if townRoot, trErr := workspace.FindFromCwd(); trErr == nil && townRoot != "" {
			_ = events.LogFeedTo(townRoot, events.TypeHandoffNoPersist, agent,
				events.HandoffFailedPayload(subject, err))
		}
		fmt.Fprintf(os.Stderr, "The session was NOT respawned. Fix the issue and retry.\n")
		return fmt.Errorf("handoff --cycle: mail failed to persist: %w", err)
	}
	fmt.Fprintf(os.Stderr, "handoff --cycle: saved state to %s\n", beadID)

	// Write handoff marker so post-cycle prime knows it's post-handoff.
	// Format: "session_id\nreason" — the reason enables isCompactResume()
	// to detect compaction-triggered cycles and use a lighter continuation
	// directive instead of full re-initialization. (GH#1965)
	// The cooldown is recorded with it (gt-058d). Both go under the
	// successor's workDir, which may differ from our cwd, so the plan comes
	// first.
	//
	// Build restart command for a fresh session — the successor picks up
	// context from the handoff mail + hook, not from --continue.
	// Using --continue would resume the same over-threshold conversation,
	// causing PreCompact to fire again and loop indefinitely.
	plan, err := buildRestartPlan(currentSession, buildRestartCommandOpts{
		Registry:        reg,
		ContinueSession: false,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "handoff --cycle: could not build restart command: %v\n", err)
		return err
	}
	cwd, _ := os.Getwd()
	recordHandoffState(plan, cwd, currentSession, handoffReason)

	// Log cycle event AFTER persistence succeeds.
	if townRoot, err := workspace.FindFromCwd(); err == nil && townRoot != "" {
		agent := sessionToGTRole(reg, currentSession)
		if agent == "" {
			agent = currentSession
		}
		_ = events.LogFeed(events.TypeHandoff, agent, events.HandoffPayload(subject, true))
	}

	fmt.Fprintf(os.Stderr, "handoff --cycle: cycling session %s\n", currentSession)

	// Refresh the tmux session env, which liveness checks read, so it names the
	// agent the pane is about to run (gt-di8p).
	updateSessionEnvForHandoff(reg, t, currentSession, os.LookupEnv)

	// Respawn pane — this atomically kills current process and starts fresh
	return superviseHandoff(currentSession, "gt handoff", "handoff --cycle", func() error {
		plan.syncSettings()
		return respawnOwnPane(t, currentSession, pane, plan.Command)
	})
}

// getCurrentTmuxSession returns the current tmux session name.
func getCurrentTmuxSession() (string, error) {
	// Prefer GT_ROLE for session resolution. BuildCommand uses -L <town-socket>,
	// but the calling process may live on the default socket (e.g., Claude Code
	// spawned by tmux on the default server). In that case, display-message on
	// the town socket returns an arbitrary session instead of
	// the caller's actual session.
	if role := os.Getenv("GT_ROLE"); role != "" {
		resolved, err := resolveRoleToSession(townRegistry(), role)
		if err == nil && resolved != "" {
			return resolved, nil
		}
		// Fall through to tmux detection if role resolution fails
	}

	// Use TMUX_PANE for targeted display-message to avoid returning an
	// arbitrary session when multiple sessions share the town socket.
	pane := os.Getenv("TMUX_PANE")
	if pane == "" {
		return "", fmt.Errorf("TMUX_PANE not set")
	}
	return tmuxSessionForPane(pane)
}

// resolveRoleToSession converts a role name or path to a tmux session name.
// Accepts:
//   - Role shortcuts: "crew", "overseer"
//   - Full paths: "<rig>/crew/<name>"
//   - Direct session names (passed through)
//
// For role shortcuts that need context (crew), it auto-detects from environment.
func resolveRoleToSession(reg *session.PrefixRegistry, role string) (string, error) {
	// First, check if it's a path format (contains /)
	if strings.Contains(role, "/") {
		return resolvePathToSession(reg, role)
	}

	switch strings.ToLower(role) {
	case constants.RoleCrew:
		// Try to get rig and crew name from environment or cwd
		rig := os.Getenv("GT_RIG")
		crewName := os.Getenv("GT_CREW")
		if rig == "" || crewName == "" {
			// Try to detect from cwd
			detected, err := detectCrewFromCwd()
			if err == nil {
				rig = detected.rigName
				crewName = detected.crewName
			}
		}
		if rig == "" || crewName == "" {
			return "", fmt.Errorf("cannot determine crew identity - run from crew directory or specify GT_RIG/GT_CREW")
		}
		return session.CrewSessionName(reg.PrefixForRig(rig), crewName), nil

	case string(session.RoleOverseer):
		// GT_ROLE=overseer names the role, not the hq-overseer session.
		return session.OverseerSessionName(), nil

	default:
		// Assume it's a direct session name (e.g., gt-gastown-crew-max)
		return role, nil
	}
}

// resolvePathToSession converts a path like "<rig>/crew/<name>" to a session name.
// Supported formats:
//   - <rig>/crew/<name> -> gt-<rig>-crew-<name>
//   - <rig>/polecats/<name> -> gt-<rig>-<name> (explicit polecat)
//   - <rig>/<name> -> gt-<rig>-<name> (polecat shorthand, if name isn't a known role)
func resolvePathToSession(reg *session.PrefixRegistry, path string) (string, error) {
	parts := strings.Split(path, "/")
	for _, part := range parts {
		if !safeAgentPathSegment(part) {
			return "", fmt.Errorf("invalid target path segment in %q", path)
		}
	}

	// Handle <rig>/crew/<name> format
	if len(parts) == 3 && parts[1] == constants.RoleCrew {
		rig := parts[0]
		name := parts[2]
		return session.CrewSessionName(reg.PrefixForRig(rig), name), nil
	}

	// Handle <rig>/polecats/<name> format (explicit polecat path)
	if len(parts) == 3 && parts[1] == "polecats" {
		rig := parts[0]
		name := strings.ToLower(parts[2]) // normalize polecat name
		return session.PolecatSessionName(reg.PrefixForRig(rig), name), nil
	}

	// Handle <rig>/<role-or-polecat> format
	if len(parts) == 2 {
		rig := parts[0]
		second := parts[1]
		secondLower := strings.ToLower(second)

		// Check for known roles first
		switch secondLower {
		case constants.RoleCrew:
			// Just "<rig>/crew" without a name - need more info
			return "", fmt.Errorf("crew path requires name: %s/crew/<name>", rig)
		case "polecats":
			// Just "<rig>/polecats" without a name - need more info
			return "", fmt.Errorf("polecats path requires name: %s/polecats/<name>", rig)
		default:
			// Not a known role - check if it's a crew member before assuming polecat.
			// Crew members exist at <townRoot>/<rig>/crew/<name>.
			// This fixes: gt sling gt-375 gastown/max failing because max is crew, not polecat.
			townRoot := detectTownRootFromCwd()
			if townRoot != "" {
				crewPath := filepath.Join(townRoot, rig, "crew", second)
				if info, err := os.Stat(crewPath); err == nil && info.IsDir() {
					return session.CrewSessionName(reg.PrefixForRig(rig), second), nil
				}
			}
			// Not a crew member - treat as polecat name (e.g., gastown/nux)
			return session.PolecatSessionName(reg.PrefixForRig(rig), secondLower), nil
		}
	}

	return "", fmt.Errorf("cannot parse path '%s' - expected <rig>/<polecat> or <rig>/crew/<name>", path)
}

// buildRestartCommand creates the command to run when respawning a session's pane.
// This needs to be the actual command to execute (e.g., claude), not a session attach command.
// The command includes a cd to the correct working directory for the role.
//
// buildRestartCommandOpts controls restart command generation.
type buildRestartCommandOpts struct {
	// ContinueSession adds --continue and omits the beacon prompt,
	// so the agent resumes its previous conversation silently.
	ContinueSession bool
	// ContinuePrompt overrides the default continuation prompt when
	// ContinueSession is true. If empty, falls back to a generic
	// continuation message.
	ContinuePrompt string
	// Registry supplies the rig prefixes the session name parses with.
	Registry *session.PrefixRegistry
	// TownRoot is the town the session belongs to; "" detects it from the
	// working directory and environment.
	TownRoot string
	// LookupEnv reads the handing-off process's environment; nil is
	// os.LookupEnv.
	LookupEnv func(key string) (string, bool)
	// SessionEnv reads the session's tmux environment, the fallback when
	// GT_AGENT is not in the process environment; nil asks tmux.
	SessionEnv func(sessionName, key string) (string, error)
}

func buildRestartCommand(reg *session.PrefixRegistry, sessionName string) (string, error) {
	return buildRestartCommandWithOpts(sessionName, buildRestartCommandOpts{Registry: reg})
}

// agentNameForRole returns the worker name a role keys its agent config and
// system-prompt file on. Polecat and crew system-prompt files are per agent;
// other roles have none and pass "".
func agentNameForRole(role, workerName string) string {
	if role == constants.RolePolecat || role == constants.RoleCrew {
		return workerName
	}
	return ""
}

// respawnAgentPin returns the agent a respawn of this identity must run, or ""
// when the caller should resolve it from config instead.
//
// A pinned GT_AGENT records the agent the session was spawned with, not what
// the role should run now. Once role_agents maps the role, live config is the
// authority — re-pinning the old preset made role_agents.<role> a write-once
// setting, so the gastown witness kept spawning claude-opus after
// role_agents.witness was set to claude-opus-cycle, and only an out-of-band
// restart applied it (gt-di8p).
//
// Two pins describe something live config does not, and are kept:
//   - an explicit --agent override (config.EnvAgentOverride), which nothing
//     else records — a sling --agent claude-haiku or a polecat_pool seat would
//     otherwise be swapped for the role's preset on the worker's next handoff;
//   - a crew worker's worker_agents/crew_agents mapping, which outranks
//     role_agents.crew and belongs to this identity rather than the role.
func respawnAgentPin(role, agentName, townRoot, rigPath, pinnedAgent string, pinnedByOverride bool) string {
	if pinnedAgent == "" || pinnedByOverride || role == "" ||
		!config.HasExplicitRoleAgent(role, townRoot, rigPath) {
		return pinnedAgent
	}
	if role == constants.RoleCrew && agentName != "" {
		if rc := config.ResolveWorkerAgentConfig(agentName, townRoot, rigPath); rc != nil && rc.ResolvedAgent == pinnedAgent {
			return pinnedAgent
		}
	}
	return ""
}

// liveRespawnConfig resolves the config a respawn of this identity runs when no
// pin stands. A crew worker resolves its own worker_agents/crew_agents mapping
// first: it outranks role_agents.crew, so resolving the role alone would drop
// the worker's mapping — including a change to it — where a fresh spawn would
// honor it. Every other role resolves through the role, which carries the
// role's --settings and --append-system-prompt-file flags.
func liveRespawnConfig(role, agentName, townRoot, rigPath string) (*config.RuntimeConfig, error) {
	if role == constants.RoleCrew && agentName != "" {
		if rc := config.ResolveWorkerAgentConfig(agentName, townRoot, rigPath); rc != nil {
			return rc, nil
		}
	}
	return config.ResolveRoleAgentConfigWithOverride(role, townRoot, rigPath, "", agentName)
}

func buildRestartCommandWithOpts(sessionName string, opts buildRestartCommandOpts) (string, error) {
	plan, err := buildRestartPlan(sessionName, opts)
	return plan.Command, err
}

// buildRestartPlan is buildRestartCommandWithOpts plus what the respawn's
// settings sync needs: the role, its settings and working directories, and
// the runtime config the restart command runs.
func buildRestartPlan(sessionName string, opts buildRestartCommandOpts) (restartPlan, error) {
	reg := opts.Registry
	lookupEnv := opts.LookupEnv
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	getenv := func(key string) string { v, _ := lookupEnv(key); return v }
	sessionEnv := opts.SessionEnv
	if sessionEnv == nil {
		sessionEnv = tmux.NewTmux().GetEnvironment
	}

	// Detect town root from current directory
	townRoot := opts.TownRoot
	if townRoot == "" {
		townRoot = detectTownRootFromCwd()
	}
	if townRoot == "" {
		return restartPlan{}, fmt.Errorf("cannot detect town root - run from within a Gas Town workspace")
	}

	// Determine the working directory for this session type
	workDir, err := sessionWorkDir(reg, sessionName, townRoot)
	if err != nil {
		return restartPlan{}, err
	}

	// Parse the session name to get the identity (used for GT_ROLE and beacon)
	identity, err := session.ParseSessionNameWithRegistry(sessionName, reg)
	if err != nil {
		return restartPlan{}, fmt.Errorf("cannot parse session name %q: %w", sessionName, err)
	}
	gtRole := identity.GTRole()
	simpleRole := config.ExtractSimpleRole(gtRole)

	// Derive rigPath from session identity for --settings flag resolution
	rigPath := ""
	if identity.Rig != "" {
		rigPath = filepath.Join(townRoot, identity.Rig)
	}

	// Build startup beacon for predecessor discovery via /resume.
	// When ContinueSession is set, use a continuation prompt instead of
	// the full handoff beacon — the agent resumes its previous context.
	beacon := ""
	if opts.ContinueSession {
		if opts.ContinuePrompt != "" {
			beacon = opts.ContinuePrompt
		} else {
			beacon = "Your account was rotated to avoid a rate limit. Continue your previous task."
		}
	} else {
		beacon = session.FormatStartupBeacon(session.BeaconConfig{
			Recipient: identity.BeaconAddress(),
			Sender:    "self",
			Topic:     "handoff",
		})
	}

	// For respawn-pane, we:
	// 1. cd to the right directory (role's canonical home)
	// 2. export GT_ROLE and BD_ACTOR so role detection works correctly
	// 3. export Claude-related env vars (not inherited by fresh shell)
	// 4. run claude with the startup beacon (triggers immediate context loading)
	// Use exec to ensure clean process replacement.
	//
	// Check if current session is using a non-default agent (GT_AGENT env var).
	// If so, preserve it across handoff by using the override variant.
	// Fall back to tmux session environment if process env doesn't have it,
	// since exec env vars may not propagate through all agent runtimes.
	currentAgent, agentInEnv := lookupEnv("GT_AGENT")
	agentPinFromOverride := getenv(config.EnvAgentOverride) == "1"
	if !agentInEnv {
		// GT_AGENT not in process env at all — try tmux session environment
		// as fallback, since exec env vars may not propagate through all runtimes.
		if val, err := sessionEnv(sessionName, config.EnvAgent); err == nil && val != "" {
			currentAgent = val
			if marker, err := sessionEnv(sessionName, config.EnvAgentOverride); err == nil && marker == "1" {
				agentPinFromOverride = true
			}
		}
	}
	// Polecat, crew and dog system-prompt files are per agent; the identity's
	// Name is the worker name for those roles and unused for the rest.
	agentName := agentNameForRole(simpleRole, identity.Name)

	// The pin only stands when it still describes what this identity should
	// run; when it does not, resolve the agent from live config instead.
	staleAgentPin := currentAgent != "" &&
		respawnAgentPin(simpleRole, agentName, townRoot, rigPath, currentAgent, agentPinFromOverride) == ""
	if staleAgentPin {
		currentAgent = ""
	}

	var runtimeCmd string
	var roleRuntimeConfig *config.RuntimeConfig
	// syncHooks is set for a session with a role: its managed settings are
	// synced before the respawn.
	var syncHooks bool
	if currentAgent != "" {
		// Resolve with the override but still through the role-aware path so
		// the respawn carries --settings and --append-system-prompt-file
		// exactly like a daemon spawn would.
		rc, err := config.ResolveRoleAgentConfigWithOverride(simpleRole, townRoot, rigPath, currentAgent, agentName)
		if err != nil {
			return restartPlan{}, fmt.Errorf("resolving agent config: %w", err)
		}
		syncHooks = true
		runtimeCmd = rc.BuildCommandWithPrompt(beacon)
	} else if simpleRole != "" {
		// Preserve role_agents model selection across self-handoff by resolving
		// runtime command via live config (instead of default-agent lookup).
		// Resolved through the *WithOverride path with an empty override so the
		// per-agent roles keep --append-system-prompt-file on a plain handoff.
		rc, err := liveRespawnConfig(simpleRole, agentName, townRoot, rigPath)
		if err != nil {
			return restartPlan{}, fmt.Errorf("resolving agent config: %w", err)
		}
		roleRuntimeConfig = rc
		syncHooks = true
		runtimeCmd = rc.BuildCommandWithPrompt(beacon)
	} else {
		runtimeCmd = config.GetRuntimeCommandWithPrompt(rigPath, beacon)
	}

	// The agent the successor will actually run. When a stale pin was dropped
	// that is whatever role_agents resolved to; recording it keeps the
	// successor's session env, liveness lookups and next handoff in agreement
	// with its runtime instead of with the preset it used to run.
	resolvedAgent := currentAgent
	if staleAgentPin && roleRuntimeConfig != nil {
		resolvedAgent = roleRuntimeConfig.ResolvedAgent
	}

	// Add --continue flag to resume the most recent session.
	// Note: runtimeCmd starts with the command name (e.g., "claude --settings ..."),
	// not "exec claude" — the "exec" prefix is added later in the Sprintf.
	if opts.ContinueSession {
		// Handle both Unix ("claude ") and Windows ("claude.exe ") binary names
		if n := strings.Replace(runtimeCmd, "claude.exe ", "claude.exe --continue ", 1); n != runtimeCmd {
			runtimeCmd = n
		} else {
			runtimeCmd = strings.Replace(runtimeCmd, "claude ", "claude --continue ", 1)
		}
	}

	// Build environment variables map — role vars first, then Claude vars.
	// Uses config.PrependEnv for OS-aware export syntax (bash export on
	// Unix, $env: on Windows).
	envMap := make(map[string]string)
	var agentEnv map[string]string // agent config Env (rc.toml [agents.X.env])
	var secretKeys map[string]string
	var secretPrefix string
	if gtRole != "" {
		// When GT_AGENT is set, resolve config with the override so we pick up
		// the active agent's env (e.g., NODE_OPTIONS from [agents.X.env]).
		// Otherwise, fall back to role-based resolution.
		var runtimeConfig *config.RuntimeConfig
		if currentAgent != "" {
			// Same role-aware resolution as the command above, so the
			// exported env carries GT_SYSTEM_PROMPT_FILE when the flag does.
			rc, err := config.ResolveRoleAgentConfigWithOverride(simpleRole, townRoot, rigPath, currentAgent, agentName)
			if err == nil {
				runtimeConfig = rc
			} else {
				runtimeConfig = config.ResolveRoleAgentConfig(simpleRole, townRoot, rigPath)
			}
		} else if simpleRole != "" {
			// Same resolution as the command above (empty override), so the
			// exported env carries GT_SYSTEM_PROMPT_FILE when the flag does and
			// a crew worker's own agent mapping is not lost.
			rc, err := liveRespawnConfig(simpleRole, agentName, townRoot, rigPath)
			if err == nil {
				runtimeConfig = rc
			} else {
				runtimeConfig = config.ResolveRoleAgentConfig(simpleRole, townRoot, rigPath)
			}
		} else {
			runtimeConfig = config.ResolveAgentConfig(townRoot, rigPath)
		}
		// Resolve ${VAR} references the same way the first-spawn path does, so
		// a handoff re-exports the key rather than the reference (gt-yih1).
		// A reference to a settings/daemon.env entry is read by the
		// command when it runs, so the secret stays out of argv (G3-18).
		spawnEnv, err := config.ResolveSpawnEnv(townRoot, runtimeConfig.Env)
		if err != nil {
			return restartPlan{}, err
		}
		if runtime.GOOS == "windows" {
			agentEnv = spawnEnv.Values()
		} else {
			agentEnv = spawnEnv.Inline
			secretKeys = spawnEnv.FromFile
			secretPrefix = spawnEnv.ShellPrefix()
		}
		envMap["GT_ROLE"] = gtRole
		envMap["BD_ACTOR"] = gtRole
		// The seat's bare name, matching config.AgentEnv at first spawn and
		// the name gt done's rework lease compares; the compound address made
		// every commit after the respawn read as another seat's divergence
		// (gt-7a955).
		maps.Copy(envMap, config.GitIdentityEnv(git.SeatIdentityFor(gtRole, "").Name))
		if runtimeConfig.Session != nil && runtimeConfig.Session.SessionIDEnv != "" {
			envMap["GT_SESSION_ID_ENV"] = runtimeConfig.Session.SessionIDEnv
		}
	}

	// Record the agent the successor runs, so agent selection survives the
	// handoff. After a role_agents re-resolution that is the newly resolved
	// agent, not the preset this session was spawned with (gt-di8p).
	if resolvedAgent != "" {
		envMap[config.EnvAgent] = resolvedAgent
	}
	// Hand the override's provenance on with the override itself, so the
	// successor keeps honoring it on its own next handoff.
	if agentPinFromOverride && !staleAgentPin {
		envMap[config.EnvAgentOverride] = "1"
	}

	// Preserve GT_PROCESS_NAMES across handoff for accurate liveness detection.
	// Without this, custom agents that shadow built-in presets (e.g., custom
	// "claude" running a wrapper script) would revert to GT_AGENT-based lookup after
	// handoff, causing false liveness failures. A re-resolved agent is not the
	// one those names describe, so it is recomputed from its own preset.
	if processNames := getenv("GT_PROCESS_NAMES"); processNames != "" && !staleAgentPin {
		envMap["GT_PROCESS_NAMES"] = processNames
	} else if resolvedAgent != "" {
		var command string
		var args []string
		if staleAgentPin && roleRuntimeConfig != nil {
			command, args = roleRuntimeConfig.Command, roleRuntimeConfig.Args
		}
		resolved := config.AgentRegistryFor(townRoot, rigPath).ResolveProcessNames(resolvedAgent, command, args...)
		envMap["GT_PROCESS_NAMES"] = strings.Join(resolved, ",")
	}

	// Add the provider settings from the current environment (not inherited
	// by respawn-pane's fresh shell). Credentials are never among them: the
	// respawn command is argv (gt-y3pgh.10). The agent's own env wins, as it
	// does at first spawn.
	for k, v := range config.ProviderPassthroughEnv(getenv) {
		if _, set := agentEnv[k]; !set {
			envMap[k] = v
		}
	}

	// Merge all agent preset env vars from config.json [agents.X.env] so the
	// new session inherits the same custom configuration (e.g. ANTHROPIC_BASE_URL,
	// CLAUDE_CODE_OAUTH_TOKEN, ANTHROPIC_CUSTOM_HEADERS for proxied Claude).
	// This mirrors the first-spawn path in config/loader.go where preset.Env is
	// merged into RuntimeConfig.Env. Existing keys (GT_ROLE, BD_ACTOR, etc.) take
	// precedence over agent-defined keys.
	for k, v := range agentEnv {
		if _, exists := envMap[k]; !exists {
			envMap[k] = v
		}
	}

	// Special case: clear NODE_OPTIONS when not explicitly set in the agent
	// config, to prevent debugger flags (e.g., --inspect from VSCode) from
	// being inherited through tmux into Claude's Node.js runtime.
	// Note: agentEnv is intentionally nil when gtRole is empty (non-role
	// handoffs), which causes the nil map lookup to return ("", false) —
	// clearing NODE_OPTIONS.
	if _, hasNodeOpts := agentEnv["NODE_OPTIONS"]; !hasNodeOpts {
		envMap["NODE_OPTIONS"] = ""
	}
	config.SanitizeAgentEnv(envMap, agentEnv)
	for k := range secretKeys {
		delete(envMap, k)
	}

	// Build the full command with OS-appropriate env prefix
	var cdPrefix string
	if runtime.GOOS == "windows" {
		cdPrefix = fmt.Sprintf("cd %s; ", workDir)
	} else {
		cdPrefix = fmt.Sprintf("cd %s && ", workDir)
	}

	var execPrefix string
	if runtime.GOOS != "windows" {
		execPrefix = "exec "
	}

	envCmd := secretPrefix + config.PrependEnv(execPrefix+runtimeCmd, envMap)
	plan := restartPlan{Command: cdPrefix + envCmd, WorkDir: workDir}
	if syncHooks {
		settingsDir := config.RoleSettingsDir(simpleRole, rigPath)
		if settingsDir == "" {
			settingsDir = workDir
		}
		plan.Hooks = &respawnHooks{
			TownRoot:    townRoot,
			Actor:       gtRole,
			Session:     sessionName,
			Role:        simpleRole,
			SettingsDir: settingsDir,
			WorkDir:     workDir,
		}
	}
	return plan, nil
}

// updateSessionEnvForHandoff updates the tmux session environment with the
// agent name and process names for liveness detection. IsAgentAliveChecked reads
// GT_PROCESS_NAMES from the tmux session env (via tmux show-environment), not
// from shell exports in the pane. Without this, post-handoff liveness checks
// would use stale values from the previous agent.
//
// Every caller that respawns a pane from buildRestartCommand must call this
// too, or the session env keeps naming the agent the pane no longer runs.
//
// lookupEnv is the process environment the restart command was built from:
// os.LookupEnv for a session restarting itself, remoteHandoffEnv's view for
// another session, whose agent is the one in its own tmux env.
func updateSessionEnvForHandoff(reg *session.PrefixRegistry, t *tmux.Tmux, sessionName string, lookupEnv func(string) (string, bool)) {
	getenv := func(key string) string { v, _ := lookupEnv(key); return v }

	// Resolve current agent using the same priority as buildRestartCommandWithOpts
	currentAgent := getenv(config.EnvAgent)
	if currentAgent == "" {
		if val, err := t.GetEnvironment(sessionName, config.EnvAgent); err == nil && val != "" {
			currentAgent = val
		}
	}

	townRoot := detectTownRootFromCwd()
	identity, identityErr := session.ParseSessionNameWithRegistry(sessionName, reg)
	role := ""
	agentName := ""
	rigPath := ""
	if identityErr == nil {
		role = config.ExtractSimpleRole(identity.GTRole())
		agentName = agentNameForRole(role, identity.Name)
		if identity.Rig != "" && townRoot != "" {
			rigPath = filepath.Join(townRoot, identity.Rig)
		}
	}

	// Same precedence rule as buildRestartCommandWithOpts: the pin loses to
	// live config unless it is an explicit override. The session env must name
	// the agent the pane is about to run, or liveness checks keep matching the
	// previous agent's process names (gt-di8p). Pin and marker are both read
	// with the same priority as in buildRestartCommandWithOpts — a session env
	// that disagrees with the command would report the wrong agent alive.
	pinnedByOverride := getenv(config.EnvAgentOverride) == "1"
	if !pinnedByOverride {
		if marker, err := t.GetEnvironment(sessionName, config.EnvAgentOverride); err == nil && marker == "1" {
			pinnedByOverride = true
		}
	}
	var roleRuntimeConfig *config.RuntimeConfig
	agentChanged := false
	if currentAgent != "" && townRoot != "" && role != "" &&
		respawnAgentPin(role, agentName, townRoot, rigPath, currentAgent, pinnedByOverride) == "" {
		rc, err := liveRespawnConfig(role, agentName, townRoot, rigPath)
		// A mapping that no longer resolves (uninstalled preset, typo) leaves
		// the pin in place rather than blanking the session env.
		if err == nil && rc != nil && rc.ResolvedAgent != "" {
			currentAgent = rc.ResolvedAgent
			roleRuntimeConfig = rc
			agentChanged = true
		}
	}

	if currentAgent == "" {
		return
	}

	// Update GT_AGENT in session env
	_ = t.SetEnvironment(sessionName, config.EnvAgent, currentAgent)
	// Keep the override marker in step with the agent just written, so the next
	// handoff reads the same provenance this session had.
	if pinnedByOverride {
		_ = t.SetEnvironment(sessionName, config.EnvAgentOverride, "1")
	} else if agentChanged {
		_ = t.UnsetEnvironment(sessionName, config.EnvAgentOverride)
	}

	// Resolve and update GT_PROCESS_NAMES in session env
	// When switching agents, recompute from config. When preserving, use env value.
	var processNames string
	if agentChanged {
		// Agent is changing — resolve config to get the command for process name resolution
		if roleRuntimeConfig == nil && townRoot != "" {
			if rc, _, err := config.ResolveAgentConfigWithOverride(townRoot, rigPath, currentAgent); err == nil {
				roleRuntimeConfig = rc
			}
		}
		if roleRuntimeConfig != nil {
			resolved := config.AgentRegistryFor(townRoot, rigPath).ResolveProcessNames(currentAgent, roleRuntimeConfig.Command, roleRuntimeConfig.Args...)
			processNames = strings.Join(resolved, ",")
		}
	}
	if processNames == "" {
		// Preserve existing value or compute from current agent
		if pn := getenv("GT_PROCESS_NAMES"); pn != "" {
			processNames = pn
		} else {
			resolved := config.AgentRegistryFor(townRoot, rigPath).ResolveProcessNames(currentAgent, "")
			processNames = strings.Join(resolved, ",")
		}
	}

	_ = t.SetEnvironment(sessionName, "GT_PROCESS_NAMES", processNames)
}

// sessionWorkDir returns the correct working directory for a session.
// This is the canonical home for each role type. reg supplies the rig prefixes.
func sessionWorkDir(reg *session.PrefixRegistry, sessionName, townRoot string) (string, error) {
	switch {
	case strings.Contains(sessionName, "-crew-"):
		// gt-<rig>-crew-<name> -> <townRoot>/<rig>/crew/<name>
		rig, name, _, ok := parseCrewSessionName(reg, sessionName)
		if !ok {
			return "", fmt.Errorf("cannot parse crew session name: %s", sessionName)
		}
		return fmt.Sprintf("%s/%s/crew/%s", townRoot, rig, name), nil

	default:
		// Parse session name to determine role and resolve paths
		identity, err := session.ParseSessionNameWithRegistry(sessionName, reg)
		if err != nil {
			return "", fmt.Errorf("unknown session type: %s (%w)", sessionName, err)
		}
		switch identity.Role {
		case session.RoleOverseer:
			// The overseer is the human operator, working from the town root.
			return townRoot, nil
		case session.RolePolecat:
			// The git worktree is polecats/<name>/<rig>; the legacy layout
			// keeps the clone at polecats/<name> itself.
			polecatDir := filepath.Join(townRoot, identity.Rig, "polecats", identity.Name)
			worktree := filepath.Join(polecatDir, identity.Rig)
			info, err := os.Stat(worktree)
			switch {
			case err == nil && info.IsDir():
				return worktree, nil
			case err == nil || errors.Is(err, fs.ErrNotExist):
				return polecatDir, nil
			default:
				// Only a missing worktree means the legacy layout; any other
				// failure (permissions, a file in the path) must not quietly
				// respawn the pane in the wrong directory.
				return "", fmt.Errorf("stat polecat worktree %s: %w", worktree, err)
			}
		default:
			return "", fmt.Errorf("unknown session type: %s (role %s, try specifying role explicitly)", sessionName, identity.Role)
		}
	}
}

// sessionToGTRole converts a session name to a GT_ROLE value.
// Uses session.ParseSessionNameWithRegistry for consistent parsing across the codebase.
func sessionToGTRole(reg *session.PrefixRegistry, sessionName string) string {
	identity, err := session.ParseSessionNameWithRegistry(sessionName, reg)
	if err != nil {
		return ""
	}
	return identity.GTRole()
}

// townRootFrom is handoff's one town-root resolver: whatever walk returns, and
// nothing else. A walk that fails or finds no town resolves to "" — there is no
// second source to fall back to (gt-y3pgh.2.12).
func townRootFrom(walk func() (string, error)) string {
	townRoot, err := walk()
	if err != nil {
		return ""
	}
	return townRoot
}

// detectTownRootFromCwd walks up from the current directory to find the town
// root, falling back to the town root the session was spawned with
// (workspace.FindFromCwdOrError) when cwd detection fails: detached HEAD,
// wrong branch, deleted worktree. The walker is the one source: handoff no
// longer rescues a missing town root from the tmux global environment, which
// went stale across a town move (gt-y3pgh.2.12).
func detectTownRootFromCwd() string {
	return townRootFrom(workspace.FindFromCwdOrError)
}

// remoteHandoffEnv is lookup without the keys that name the caller's own
// agent: GT_AGENT, GT_AGENT_OVERRIDE and GT_PROCESS_NAMES. A restart of
// another session reads its agent from that session's tmux env instead, so a
// handoff between agents of different kinds keeps the target's agent.
func remoteHandoffEnv(lookup func(string) (string, bool)) func(string) (string, bool) {
	return func(key string) (string, bool) {
		switch key {
		case config.EnvAgent, config.EnvAgentOverride, "GT_PROCESS_NAMES":
			return "", false
		}
		return lookup(key)
	}
}

// remoteHandoffOps are the tmux calls a remote handoff makes, so a unit test
// can drive runRemoteHandoff without a tmux server.
type remoteHandoffOps struct {
	hasSession   func(sessionName string) (bool, error)
	sessionPane  func(sessionName string) (string, error)
	respawn      func(sessionName, pane string, plan restartPlan) error
	switchClient func(sessionName string) error
}

func liveRemoteHandoffOps(t *tmux.Tmux) remoteHandoffOps {
	return remoteHandoffOps{
		hasSession:  t.HasSession,
		sessionPane: getSessionPane,
		respawn: func(sessionName, pane string, plan restartPlan) error {
			return superviseHandoff(sessionName, "gt handoff", "handoff (remote)", func() error {
				plan.syncSettings()
				return respawnRemotePane(t, sessionName, pane, plan.Command)
			})
		},
		switchClient: func(sessionName string) error {
			return tmux.BuildCommand("switch-client", "-t", sessionName).Run()
		},
	}
}

// runRemoteHandoff respawns a different session and optionally switches to
// it. updateEnv refreshes the target's tmux env; it runs once the target is
// found and the dry-run check has passed, because a preview must not rewrite
// the target's env.
func runRemoteHandoff(ops remoteHandoffOps, targetSession string, plan restartPlan, dryRun, watch bool, updateEnv func()) error {
	restartCmd := plan.Command
	// Check if target session exists
	exists, err := ops.hasSession(targetSession)
	if err != nil {
		return fmt.Errorf("checking session: %w", err)
	}
	if !exists {
		return fmt.Errorf("session '%s' not found - is the agent running?", targetSession)
	}

	// Get the pane ID for the target session
	targetPane, err := ops.sessionPane(targetSession)
	if err != nil {
		return fmt.Errorf("getting target pane: %w", err)
	}

	fmt.Printf("%s Handing off %s...\n", style.Bold.Render("🤝"), targetSession)

	// Dry run mode
	if dryRun {
		fmt.Printf("Would execute: tmux clear-history -t %s\n", targetPane)
		fmt.Printf("Would execute: tmux respawn-pane -k -t %s %s\n", targetPane, restartCmd)
		if watch {
			fmt.Printf("Would execute: tmux switch-client -t %s\n", targetSession)
		}
		return nil
	}

	// Before the respawn: liveness checks read this env, and the respawn
	// replaces the agent it names.
	updateEnv()

	if err := ops.respawn(targetSession, targetPane, plan); err != nil {
		return err
	}

	// If --watch, switch to that session
	if watch {
		fmt.Printf("Switching to %s...\n", targetSession)
		// Use tmux switch-client to move our view to the target session
		if err := ops.switchClient(targetSession); err != nil {
			// Non-fatal - they can manually switch
			fmt.Printf("Note: Could not auto-switch (use: tmux switch-client -t %s)\n", targetSession)
		}
	}

	return nil
}

// respawnRemotePane replaces the agent in another session's pane: it kills
// the pane's processes, clears its history and respawns restartCmd there.
func respawnRemotePane(t *tmux.Tmux, targetSession, targetPane, restartCmd string) error {
	// Set remain-on-exit so the pane survives process death during handoff.
	// Without this, killing processes causes tmux to destroy the pane before
	// we can respawn it. This is essential for tmux session reuse.
	if err := t.SetRemainOnExit(targetPane, true); err != nil {
		style.PrintWarning("could not set remain-on-exit: %v", err)
	}

	// Kill all processes in the pane before respawning to prevent orphan leaks
	// RespawnPane's -k flag only sends SIGHUP which Claude/Node may ignore
	if err := t.KillPaneProcesses(targetPane); err != nil {
		// Non-fatal but log the warning
		style.PrintWarning("could not kill pane processes: %v", err)
	}

	// Clear scrollback history before respawn (resets copy-mode from [0/N] to [0/0])
	if err := t.ClearHistory(targetPane); err != nil {
		// Non-fatal - continue with respawn even if clear fails
		style.PrintWarning("could not clear history: %v", err)
	}

	// Respawn the remote session's pane, handling deleted working directories
	respawnErr := func() error {
		paneWorkDir, _ := t.GetPaneWorkDir(targetSession)
		if paneWorkDir != "" {
			if _, statErr := os.Stat(paneWorkDir); statErr != nil {
				if townRoot := detectTownRootFromCwd(); townRoot != "" {
					style.PrintWarning("pane working directory deleted, using town root")
					return t.RespawnPaneWithWorkDir(targetPane, townRoot, restartCmd)
				}
			}
		}
		return t.RespawnPane(targetPane, restartCmd)
	}()
	if respawnErr != nil {
		return fmt.Errorf("respawning pane: %w", respawnErr)
	}
	return nil
}

// superviseHandoff runs run, which replaces sessionName's agent in its pane,
// through the supervisor's Respawn (gt-4k3fj.4.1): a handoff is a restart,
// so an e-stop, a parked seat or a gt down in progress refuses it and the
// session keeps running. Respawn records the restart before run, because a
// self-handoff's respawn ends this process. Outside a town there is nothing
// to refuse it and run goes ahead. verb names the command in the actor.
func superviseHandoff(sessionName, verb, reason string, run func() error) error {
	townRoot := detectTownRootFromCwd()
	if townRoot == "" {
		return run()
	}
	return respawnSession(operatorSupervisor(townRoot), townRegistry(), sessionName, reason, operatorActor(verb), run)
}

// getSessionPane returns the pane identifier for the pane a session's agent
// runs in. gt creates an agent session as one window with one pane, so that is
// the session's first window, first pane (`session:0.0`, the target
// GetPaneWorkDir reads). `list-panes -t <session>` targets the session's
// *current* window instead, which follows whoever last attached, so a shell in
// a second window is what that lists first (gt-uf8y2). The lookup asks through
// a tmux client, so the session router sends a sandboxed session's question to
// the sandbox's own tmux server; a host session is answered by the default
// socket's server (sling 8, gt-nb840).
func getSessionPane(sessionName string) (string, error) {
	return tmux.NewTmuxWithSocket(tmux.GetDefaultSocket()).GetPaneID(sessionName)
}

// sendHandoffMail sends a handoff mail to self and auto-hooks it.
// Returns the created bead ID and any error.
func sendHandoffMail(subject, message string) (string, error) {
	// Build subject with handoff prefix if not already present
	if subject == "" {
		subject = "🤝 HANDOFF: Session cycling"
	} else if !strings.Contains(subject, "HANDOFF") {
		subject = "🤝 HANDOFF: " + subject
	}

	// Default message if not provided
	if message == "" {
		message = "Context cycling. Check bd ready for pending work."
	}

	// Detect agent identity for self-mail
	agentID, _, _, err := resolveSelfTarget()
	if err != nil {
		return "", fmt.Errorf("detecting agent identity: %w", err)
	}

	// Normalize identity to match mailbox query format
	agentID = mail.AddressToIdentity(agentID)

	// Detect town root for beads location
	townRoot := detectTownRootFromCwd()
	if townRoot == "" {
		return "", fmt.Errorf("cannot detect town root")
	}

	// Close stale hooked mail beads from previous sessions before creating a new one.
	// Without this, each handoff cycle accumulates beads in status=hooked. (GH#3859)
	townB := beads.New(filepath.Join(townRoot, ".beads"))
	if n, closeErr := beads.CloseStaleHookedMailBeads(townB, agentID); closeErr != nil {
		style.PrintWarning("couldn't close previous hooked mail bead(s): %v", closeErr)
	} else if n > 0 {
		fmt.Printf("%s Closed %d stale hooked mail bead(s)\n", style.Dim.Render("🧹"), n)
	}

	return createHandoffMailIn(beads.NewPinned(filepath.Join(townRoot, ".beads")), agentID, subject, message)
}

// createHandoffMailIn creates the handoff mail in the town database db,
// assigned to agentID, and hooks it. It returns the mail's ID, or an error
// when the create failed; a failed hook is a warning, since the mail exists.
func createHandoffMailIn(db beads.Client, agentID, subject, message string) (string, error) {
	// NOT ephemeral: handoff mail must be in issues table so gt hook can find it.
	// Ephemeral wisps are invisible to hook queries and may be reaped before successor reads.
	created, err := db.Create(beads.CreateOptions{
		Title:       subject,
		Description: message,
		Priority:    1, // high — handoffs should float above normal mail
		Labels:      []string{"from:" + agentID, "gt:message"},
		Assignee:    agentID,
		Actor:       agentID,
	})
	if err != nil {
		return "", fmt.Errorf("creating handoff mail: %w", err)
	}

	// Auto-hook the created mail bead
	hooked := string(beads.StatusHooked)
	if err := db.Update(created.ID, beads.UpdateOptions{Status: &hooked, Assignee: &agentID}); err != nil {
		// Non-fatal: mail was created, just couldn't hook
		style.PrintWarning("created mail %s but failed to auto-hook: %v", created.ID, err)
	}
	return created.ID, nil
}

// warnHandoffGitStatus checks the current workspace for uncommitted or unpushed
// work and prints a warning if found. Non-blocking — handoff continues regardless.
// Skips .beads/ changes since those are managed by Dolt and not a concern.
func warnHandoffGitStatus() {
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	warnHandoffGitStatusIn(os.Stderr, cwd)
}

// handoffGit is the part of *git.Git the handoff's workspace checks read.
type handoffGit interface {
	IsRepo() bool
	CurrentBranch() (string, error)
	CheckUncommittedWork() (*git.UncommittedWorkStatus, error)
	RecentCommits(n int) (string, error)
}

// warnHandoffGitStatusIn writes warnHandoffGitStatus's warning for the
// repository at dir to w.
func warnHandoffGitStatusIn(w io.Writer, dir string) {
	warnHandoffGitStatusWith(w, git.NewGit(dir))
}

// warnHandoffGitStatusWith is warnHandoffGitStatusIn over g.
func warnHandoffGitStatusWith(w io.Writer, g handoffGit) {
	if !g.IsRepo() {
		return
	}
	status, err := g.CheckUncommittedWork()
	if err != nil || status.CleanExcludingBeads() {
		return
	}
	fmt.Fprintf(w, "%s workspace has uncommitted work: %s\n", ui.IconWarn, status.String())
	if len(status.ModifiedFiles) > 0 {
		fmt.Fprintf(w, "%s   modified: %s\n", ui.IconWarn, strings.Join(status.ModifiedFiles, ", "))
	}
	if len(status.UntrackedFiles) > 0 {
		fmt.Fprintf(w, "%s   untracked: %s\n", ui.IconWarn, strings.Join(status.UntrackedFiles, ", "))
	}
	if status.UnpushedCommits > 0 {
		fmt.Fprintf(w, "%s   %d unpushed commit(s) — run 'git push' before handoff\n", ui.IconWarn, status.UnpushedCommits)
	}
	fmt.Fprintln(w, "  (use --no-git-check to suppress this warning)")
}

// looksLikeHandoffBeadID reports whether a gt handoff argument names a bead to
// hook rather than a session to restart. Crew and overseer session names
// (gt-crew-max, hq-overseer) have the shape of a short-prefix bead ID, so they
// are ruled out first; otherwise they take the bead branch and fail with
// "bead not found" before resolveRoleToSession sees them. Only a crew marker in
// a session position counts: a bead ID that merely contains "-crew-" (a sub-ID
// like gt-fix-crew-1) is still a bead.
func looksLikeHandoffBeadID(reg *session.PrefixRegistry, arg string) bool {
	if arg == session.OverseerSessionName() || isCrewSessionName(reg, arg) {
		return false
	}
	return looksLikeBeadID(arg)
}

// isCrewSessionName reports whether name is a crew session: <prefix>-crew-<name>
// (any short prefix, registered or not, since a rig missing from the registry
// still has live crew sessions to restart), or the legacy
// <prefix>-<rig>-crew-<name> of a registered rig.
func isCrewSessionName(reg *session.PrefixRegistry, name string) bool {
	if id, err := session.ParseSessionNameWithRegistry(name, reg); err == nil && id.Role == session.RoleCrew {
		return true
	}
	if _, rest, ok := strings.Cut(name, "-"); ok && strings.HasPrefix(rest, "crew-") && len(rest) > len("crew-") {
		return true
	}
	for rigName, prefix := range reg.AllRigs() {
		marker := prefix + "-" + rigName + "-crew-"
		if strings.HasPrefix(name, marker) && len(name) > len(marker) {
			return true
		}
	}
	return false
}

// looksLikeBeadID checks if a string looks like a bead ID.
// Bead IDs have format: prefix-xxxx where prefix is 1-5 lowercase letters and xxxx is alphanumeric.
// Examples: "gt-abc123", "bd-ka761", "hq-cv-abc", "beads-xyz", "ap-qtsup.16"
func looksLikeBeadID(s string) bool {
	// Find the first hyphen
	idx := strings.Index(s, "-")
	if idx < 1 || idx > 5 {
		// No hyphen, or prefix is empty/too long
		return false
	}

	// Check prefix is all lowercase letters
	prefix := s[:idx]
	for _, c := range prefix {
		if c < 'a' || c > 'z' {
			return false
		}
	}

	// Check there's something after the hyphen
	rest := s[idx+1:]
	if len(rest) == 0 {
		return false
	}

	// Check rest starts with alphanumeric and contains only alphanumeric, dots, hyphens
	for i, c := range rest {
		if i == 0 {
			// First char must be alphanumeric
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')) {
				return false
			}
		} else {
			// Subsequent chars: alphanumeric, dots, hyphens
			if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == '-') {
				return false
			}
		}
	}

	return true
}

// hookBeadForHandoff attaches a bead to the current agent's hook.
func hookBeadForHandoff(beadID string) error {
	// Verify the bead exists first
	db := beads.NewPlain("", nil)
	if _, err := db.Show(beadID); err != nil {
		return fmt.Errorf("bead '%s' not found", beadID)
	}

	// Determine agent identity
	agentID, _, _, err := resolveSelfTarget()
	if err != nil {
		return fmt.Errorf("detecting agent identity: %w", err)
	}

	fmt.Printf("%s Hooking %s...\n", style.Bold.Render("🪝"), beadID)

	if handoffDryRun {
		fmt.Printf("Would run: bd update %s --status=pinned --assignee=%s\n", beadID, agentID)
		return nil
	}

	// Pin the bead using bd update (discovery-based approach)
	pinned := string(beads.StatusPinned)
	if err := db.Update(beadID, beads.UpdateOptions{Status: &pinned, Assignee: &agentID}); err != nil {
		return fmt.Errorf("pinning bead: %w", err)
	}

	fmt.Printf("%s Work attached to hook (pinned bead)\n", style.Bold.Render("✓"))
	return nil
}

// collectHandoffState gathers current state for handoff context.
// Collects: git workspace state (deterministic), inbox summary, ready beads, hooked work.
// Git state is always collected first via Go library calls (no shelling out) to ensure
// the handoff always contains useful context even when external commands fail. (GH#1996)
func collectHandoffState() string {
	var parts []string

	// Deterministic git state — always collected via Go library, never empty. (GH#1996)
	if gitState := collectGitState(); gitState != "" {
		parts = append(parts, gitState)
	}

	// Get hooked work
	hookOutput, err := exec.Command("gt", "hook").Output()
	if err == nil {
		hookStr := strings.TrimSpace(string(hookOutput))
		if hookStr != "" && !strings.Contains(hookStr, "Nothing on hook") {
			parts = append(parts, "## Hooked Work\n"+hookStr)
		}
	}

	// Get inbox summary (first few messages)
	inboxOutput, err := exec.Command("gt", "mail", "inbox").Output()
	if err == nil {
		inboxStr := strings.TrimSpace(string(inboxOutput))
		if inboxStr != "" && !strings.Contains(inboxStr, "Inbox empty") {
			// Limit to first 10 lines for brevity
			lines := strings.Split(inboxStr, "\n")
			if len(lines) > 10 {
				lines = append(lines[:10], "... (more messages)")
			}
			parts = append(parts, "## Inbox\n"+strings.Join(lines, "\n"))
		}
	}

	// Get ready beads
	db := beads.NewPlain("", nil)
	if lines := issueSummaryLines(db.Ready()); len(lines) > 0 {
		// Limit to first 10 lines
		if len(lines) > 10 {
			lines = append(lines[:10], "... (more issues)")
		}
		parts = append(parts, "## Ready Work\n"+strings.Join(lines, "\n"))
	}

	// Get in-progress beads
	if lines := issueSummaryLines(db.List(beads.ListOptions{Status: "in_progress", Priority: -1})); len(lines) > 0 {
		if len(lines) > 5 {
			lines = append(lines[:5], "... (more)")
		}
		parts = append(parts, "## In Progress\n"+strings.Join(lines, "\n"))
	}

	if len(parts) == 0 {
		return "No active state to report."
	}

	return strings.Join(parts, "\n\n")
}

// issueSummaryLines renders a bd read's issues as "id [P<n>] title" lines.
// It returns nil when the read failed or found nothing, so the caller omits
// the section.
func issueSummaryLines(issues []*beads.Issue, err error) []string {
	if err != nil || len(issues) == 0 {
		return nil
	}
	lines := make([]string, 0, len(issues))
	for _, issue := range issues {
		lines = append(lines, fmt.Sprintf("%s [P%d] %s", issue.ID, issue.Priority, issue.Title))
	}
	return lines
}

// collectGitState captures deterministic workspace state using the Go git library.
// This uses only the git.Git wrapper (no shelling out to gt/bd), so it works
// reliably even when PATH is broken or external commands are unavailable.
// Returns empty string if git state cannot be read (e.g., not in a git repo). (GH#1996)
func collectGitState() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return collectGitStateIn(cwd)
}

// collectGitStateIn is collectGitState for the repository at dir.
func collectGitStateIn(dir string) string {
	return collectGitStateWith(git.NewGit(dir))
}

// collectGitStateWith is collectGitStateIn over g.
func collectGitStateWith(g handoffGit) string {
	if !g.IsRepo() {
		return ""
	}

	var lines []string

	// Branch
	if branch, err := g.CurrentBranch(); err == nil && branch != "" {
		lines = append(lines, "Branch: "+branch)
	}

	// Uncommitted work summary (skip section on error, don't bail entirely)
	if work, err := g.CheckUncommittedWork(); err == nil {
		if work.HasUncommittedChanges {
			if len(work.ModifiedFiles) > 0 {
				files := work.ModifiedFiles
				if len(files) > 10 {
					files = append(files[:10], fmt.Sprintf("... (+%d more)", len(work.ModifiedFiles)-10))
				}
				lines = append(lines, "Modified: "+strings.Join(files, ", "))
			}
			if len(work.UntrackedFiles) > 0 {
				files := work.UntrackedFiles
				if len(files) > 5 {
					files = append(files[:5], fmt.Sprintf("... (+%d more)", len(work.UntrackedFiles)-5))
				}
				lines = append(lines, "Untracked: "+strings.Join(files, ", "))
			}
		}
		if work.StashCount > 0 {
			lines = append(lines, fmt.Sprintf("Stashes: %d", work.StashCount))
		}
		if work.UnpushedCommits > 0 {
			lines = append(lines, fmt.Sprintf("Unpushed commits: %d", work.UnpushedCommits))
		}
	}

	// Recent commits (last 5) for context on what was being worked on.
	if logStr, err := g.RecentCommits(5); err == nil && logStr != "" {
		lines = append(lines, "Recent commits:\n"+logStr)
	}

	if len(lines) == 0 {
		return ""
	}

	return "## Workspace State\n" + strings.Join(lines, "\n")
}

// cleanupMoleculeOnHandoff closes any in-progress molecule steps before session
// handoff, preventing orphaned wisps from accumulating. (gt-e26g)
//
// Without this, patrol agents (witness, refinery, deacon) that handoff mid-cycle
// leave unfinished molecule steps open forever. The next session pours a new
// molecule, so the old steps are never completed.
//
// All errors are non-fatal — handoff must succeed even if cleanup fails.
func cleanupMoleculeOnHandoff() {
	cwd, err := os.Getwd()
	if err != nil {
		return
	}

	townRoot, err := workspace.FindFromCwd()
	if err != nil || townRoot == "" {
		return
	}

	// Detect agent identity
	roleInfo, err := GetRoleWithContext(cwd, townRoot)
	if err != nil {
		return
	}
	roleCtx := RoleContext{
		Role:     roleInfo.Role,
		Rig:      roleInfo.Rig,
		Polecat:  roleInfo.Polecat,
		TownRoot: townRoot,
		WorkDir:  cwd,
	}
	agentID := buildAgentIdentity(roleCtx)
	if agentID == "" {
		return
	}

	workDir, err := findLocalBeadsDir()
	if err != nil {
		return
	}

	b := beads.New(workDir)

	// Extract the role name for FindHandoffBead
	parts := strings.Split(agentID, "/")
	role := parts[len(parts)-1]

	handoffBead, err := beads.FindHandoffBead(b, role)
	if err != nil || handoffBead == nil {
		return
	}

	// Check for attached molecule on the handoff bead
	attachment := beads.ParseAttachmentFields(handoffBead)
	if attachment == nil || attachment.AttachedMolecule == "" {
		return
	}

	molID := attachment.AttachedMolecule

	// Close descendant steps (the leaked wisps)
	if n := closeDescendants(b, molID); n > 0 {
		fmt.Fprintf(os.Stderr, "handoff: closed %d molecule step(s) for %s\n", n, molID)
	}

	// Detach molecule with audit trail
	if _, err := beads.DetachMoleculeWithAudit(b, handoffBead.ID, beads.DetachOptions{
		Operation: "squash",
		Reason:    "handoff: session cycling",
	}); err != nil {
		fmt.Fprintf(os.Stderr, "handoff: warning: detach molecule audit failed: %v\n", err)
	}

	// Close all descendant wisps first, then the molecule root.
	// Without this, handoff leaks orphan wisps into the DB.
	// Best-effort in handoff path — log but proceed.
	if _, err := forceCloseDescendants(b, molID); err != nil {
		style.PrintWarning("handoff: could not close descendants of %s: %v", molID, err)
	}

	// Force-close the molecule root wisp
	if err := b.ForceCloseWithReason("handoff", molID); err != nil {
		fmt.Fprintf(os.Stderr, "handoff: warning: couldn't close molecule %s: %v\n", molID, err)
	}
}

// enforceHandoffCooldown sleeps if the last handoff was too recent.
// This prevents tight restart loops when patrol agents (e.g., witness)
// complete quickly on idle rigs and immediately hand off. (gt-058d)
//
// The cooldown is based on the modification time of the last_handoff_ts
// file in the .runtime directory. If the file exists and was written
// less than MinHandoffCooldown ago, the function sleeps for the remaining
// time. This ensures at least MinHandoffCooldown passes between handoffs.
//
// Crew is exempt — it hands off on human request,
// not on patrol loops, so the cooldown just gets in the way.
func enforceHandoffCooldown() {
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	enforceHandoffCooldownIn(os.Stdout, os.Getenv("GT_ROLE"), cwd, time.Now, clockwork.NewRealClock().Sleep)
}

// enforceHandoffCooldownIn is enforceHandoffCooldown for a session whose
// GT_ROLE is role, recording handoffs under dir, with now and the wait passed
// in so tests can name the remaining cooldown without a wall clock or a sleep.
func enforceHandoffCooldownIn(w io.Writer, role, dir string, now func() time.Time, sleep func(time.Duration)) {
	if role != "" {
		parsed, _, _ := parseRoleString(role)
		switch parsed {
		case RoleCrew:
			return
		}
	}

	age, ok := lastHandoffAge(dir, now())
	if !ok || age >= constants.MinHandoffCooldown {
		return
	}

	remaining := constants.MinHandoffCooldown - age
	fmt.Fprintf(w, "%s Handoff cooldown: waiting %v (last handoff %v ago, min %v)\n",
		style.Dim.Render("⏳"), remaining.Round(time.Second),
		age.Round(time.Second), constants.MinHandoffCooldown)
	sleep(remaining)
}

// handoffStateDir is the directory whose .runtime/ holds the handoff marker
// and cooldown: the one the successor starts in, which gt prime reads the
// marker from. It falls back to cwd when the plan names none, and is empty
// only when neither is known.
func handoffStateDir(plan restartPlan, cwd string) string {
	if plan.WorkDir != "" {
		return plan.WorkDir
	}
	return cwd
}

// autoHandoffStateDir is handoffStateDir for runHandoffAuto, which respawns
// nothing and so has no plan: it resolves the session's workDir, where the
// post-compact prime looks, and keeps cwd when that does not resolve.
func autoHandoffStateDir(reg *session.PrefixRegistry, sessionName, townRoot, cwd string) string {
	var plan restartPlan
	if townRoot != "" {
		if dir, err := sessionWorkDir(reg, sessionName, townRoot); err == nil {
			plan.WorkDir = dir
		}
	}
	return handoffStateDir(plan, cwd)
}

// recordHandoffState tells the successor of session it is post-handoff and
// starts the cooldown, both under the plan's workDir, not the caller's cwd
// (crew run gt handoff from crew/<name>/src, and the successor starts in
// crew/<name>). Called before respawning. With neither a workDir nor a cwd
// there is nowhere to record, so nothing is written.
func recordHandoffState(plan restartPlan, cwd, session, reason string) {
	dir := handoffStateDir(plan, cwd)
	if dir == "" {
		return
	}
	writeHandoffMarker(dir, session, reason)
	recordHandoffTimeIn(dir, time.Now())
}

// lastHandoffAge reports how long before now the last handoff recorded in dir
// was. ok is false when none has been recorded there.
func lastHandoffAge(dir string, now time.Time) (age time.Duration, ok bool) {
	info, err := os.Stat(filepath.Join(dir, constants.DirRuntime, constants.FileLastHandoffTS))
	if err != nil {
		return 0, false
	}
	return now.Sub(info.ModTime()), true
}

// recordHandoffTimeIn writes the handoff cooldown timestamp under dir, dating
// it at now so the recorded age and the file's mtime cannot disagree.
func recordHandoffTimeIn(dir string, now time.Time) {
	runtimeDir := filepath.Join(dir, constants.DirRuntime)
	_ = os.MkdirAll(runtimeDir, 0755)
	tsPath := filepath.Join(runtimeDir, constants.FileLastHandoffTS)
	_ = os.WriteFile(tsPath, []byte(fmt.Sprintf("%d", now.Unix())), 0644)
	_ = os.Chtimes(tsPath, now, now)
}

// writeHandoffMarker tells the successor's gt prime it is post-handoff.
// Format "session\nreason"; see isCompactResume (prime.go:513).
func writeHandoffMarker(dir, session, reason string) {
	runtimeDir := filepath.Join(dir, constants.DirRuntime)
	_ = os.MkdirAll(runtimeDir, 0755)
	content := session
	if reason != "" {
		content += "\n" + reason
	}
	_ = os.WriteFile(filepath.Join(runtimeDir, constants.FileHandoffMarker), []byte(content), 0644)
}

// respawnOwnPane replaces the caller's own pane process with restartCmd.
// It must not kill pane processes first: that kills this process before
// respawn-pane runs (gh#859). respawn-pane -k does the kill atomically.
func respawnOwnPane(t *tmux.Tmux, currentSession, pane, restartCmd string) error {
	if err := t.SetRemainOnExit(pane, true); err != nil {
		style.PrintWarning("could not set remain-on-exit: %v", err)
	}
	if err := t.ClearHistory(pane); err != nil {
		style.PrintWarning("could not clear history: %v", err)
	}
	paneWorkDir, _ := t.GetPaneWorkDir(currentSession)
	if paneWorkDir != "" {
		if _, err := os.Stat(paneWorkDir); err != nil {
			if townRoot := detectTownRootFromCwd(); townRoot != "" {
				style.PrintWarning("pane working directory deleted, using town root")
				return t.RespawnPaneWithWorkDir(pane, townRoot, restartCmd)
			}
		}
	}
	return t.RespawnPane(pane, restartCmd)
}

// tmuxSessionForPane returns the session that owns pane. Unlike
// getCurrentTmuxSession it never consults GT_ROLE, so it tells where the
// process really runs.
func tmuxSessionForPane(pane string) (string, error) {
	out, err := tmux.BuildCommand("display-message", "-t", pane, "-p", "#{session_name}").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
