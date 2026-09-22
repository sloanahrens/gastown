// Per-agent sanctioned pause (gt agent pause / gt agent resume).
//
// A mayor/operator freeze (SIGSTOP) of a misbehaving agent is
// indistinguishable from a stuck agent: the stuck-agent dog respawned a
// frozen flint 20 minutes later, and the witness patrol restarted parked
// agents through the done-intent-dead path (gt-ahik). This command is the
// sanctioned freeze: it writes a durable pause marker (the only source of
// truth every scanner reads), then mirrors agent_state=paused onto the agent
// bead for display, and freezes the session's process group.
//
// Every scanner that can resurrect a session (witness zombie detection /
// patrol scan, the polecat staleness assessor, the stuck-agent dog)
// consults the pause marker and reports "do not touch" instead of
// restarting. gt status shows PAUSED with the reason.
package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

var agentPauseReason string

var agentCmd = &cobra.Command{
	Use:     "agent",
	GroupID: GroupAgents,
	Short:   "Manage agents (pause/resume individual agents)",
	RunE:    requireSubcommand,
	Long: `Manage agents.

  gt agent pause <rig>/<name> --reason "..."   Freeze a polecat in place
  gt agent resume <address>                    Thaw a paused agent

Pause targets a polecat: it is only meaningful where a scanner reads the
marker it writes. Resume accepts any agent address, including a stale
mayor/deacon/witness/refinery/crew marker or bead mirror left by an older
build.`,
}

var agentPauseCmd = &cobra.Command{
	Use:   "pause <rig>/<name>",
	Short: "Pause a polecat: write a pause marker, sync agent_state=paused, and freeze the session",
	Long: `Pause a polecat so no scanner (witness, patrol scan, stuck-agent dog,
polecat staleness) will restart or nuke it.

Writes a durable pause marker, syncs the agent bead to
agent_state=paused, and freezes the tmux session's process group
(context is preserved — no work is lost).

Only polecat is supported: it is the only role any scanner consults the
pause marker for. Other roles are refused (deacon has its own, separate
"gt deacon pause").

Use --reason to record why the agent was paused (shown in gt status
and in scanner log output).

Resume with: gt agent resume <address>`,
	Args: cobra.ExactArgs(1),
	RunE: runAgentPause,
}

var agentResumeCmd = &cobra.Command{
	Use:   "resume <address>",
	Short: "Resume a paused agent: clear the pause marker, restore agent_state, and thaw the session",
	Long: `Resume an agent that was paused with gt agent pause.

Clears the pause marker, restores the agent bead's agent_state to
what it was before the pause, and thaws the session's process group
(SIGCONT).

Unlike pause, resume accepts any agent address: it exists to clear a
stale marker or bead mirror, not just to undo a pause this command
wrote.`,
	Args: cobra.ExactArgs(1),
	RunE: runAgentResume,
}

func init() {
	agentPauseCmd.Flags().StringVarP(&agentPauseReason, "reason", "r", "", "Reason for the pause (shown in gt status)")
	agentCmd.AddCommand(agentPauseCmd)
	agentCmd.AddCommand(agentResumeCmd)
	rootCmd.AddCommand(agentCmd)
}

// agentAddr is a parsed pause target: identity plus its agent bead ID.
type agentAddr struct {
	*session.AgentIdentity
	BeadID string
}

// pauseGatedRoles lists the roles at least one scanner actually consults the
// pause marker for: the witness zombie/stall paths (DetectZombiePolecats,
// DetectStalledPolecats, RestartPolecatSession) and the stuck-agent dog's
// polecat loop all gate on agentpause.PauseGate, and all of them only ever
// act on polecats. Witness, refinery, mayor, deacon, and crew restarts run
// through code that never reads this marker, so a pause written for them
// would look like it worked and would not (gt-ahik, om kgx0).
var pauseGatedRoles = map[session.Role]bool{
	session.RolePolecat: true,
}

// checkPauseGated refuses a pause target that no scanner honors, rather
// than writing a marker that silently does nothing. deacon has its own,
// separate pause command (`gt deacon pause`) predating this one — the
// "paused" bead state it writes still shows in `gt status` — and this
// command is not a substitute for it.
func checkPauseGated(role session.Role) error {
	if pauseGatedRoles[role] {
		return nil
	}
	hint := ""
	if role == session.RoleDeacon {
		hint = " use `gt deacon pause` / `gt deacon resume` instead"
	}
	return fmt.Errorf("gt agent pause does not support role %q: no scanner consults its pause marker for this role%s", role, hint)
}

// parseAgentAddr parses an address for gt agent pause/resume.
// Accepts <rig>/<name> (polecat), <rig>/witness, <rig>/refinery,
// mayor, and deacon.
func parseAgentAddr(address string) (*agentAddr, error) {
	id, err := session.ParseAddress(address)
	if err != nil {
		return nil, fmt.Errorf("invalid agent address %q: %w", address, err)
	}
	addr := &agentAddr{AgentIdentity: id}
	// One role/name mapping (roleAndName), so the bead ID derived here and
	// the marker coordinates derived from the same *agentAddr elsewhere never
	// disagree (gt-ahik: a duplicated switch here once drifted from
	// roleAndName's).
	role, name := addr.roleAndName()
	switch id.Role {
	case session.RoleMayor:
		addr.BeadID = beads.MayorBeadIDTown()
	case session.RoleDeacon:
		addr.BeadID = beads.DeaconBeadIDTown()
	default:
		addr.BeadID = beads.AgentBeadIDWithPrefix(session.PrefixFor(id.Rig), id.Rig, role, name)
	}
	return addr, nil
}

// roleAndName returns the agent-bead role and name for this target
// (singleton roles — witness, refinery — have an empty name).
func (a *agentAddr) roleAndName() (string, string) {
	switch a.Role {
	case session.RoleWitness:
		return constants.RoleWitness, ""
	case session.RoleRefinery:
		return constants.RoleRefinery, ""
	case session.RoleCrew:
		return constants.RoleCrew, a.Name
	case session.RolePolecat:
		return constants.RolePolecat, a.Name
	default:
		return string(a.Role), a.Name
	}
}

// displayAddress returns the address to print for this target, derived from
// the marker coordinates rather than from the caller's string. One agent
// under two names is a copy-paste bug waiting to happen at the moment an
// operator is freezing something (gt-wisp-6ajo), and the derived form still
// parses, so the printed "resume with" line is copy-pasteable.
func (a *agentAddr) displayAddress(role, name string) string {
	if addr := agentpause.AddressFor(a.Rig, role, name); addr != "" {
		return addr
	}
	return a.Address()
}

// readAgentState reads the agent bead's current agent_state verbatim, or ""
// when the bead cannot be read or carries no agent fields.
func readAgentState(townRoot, beadID string) string {
	issue, fields, err := beads.New(townRoot).ForAgentBead().GetAgentBead(beadID)
	if err != nil || issue == nil || fields == nil {
		return ""
	}
	return beads.ResolveAgentState(issue.Description, fields.AgentState)
}

// currentAgentState returns the agent bead's current agent_state, or "" when
// the bead cannot be read, carries no agent fields, or already reads
// "paused". Used to capture the pre-pause state so `gt agent resume` can
// restore it instead of blindly writing "idle" (gt-ahik).
func currentAgentState(townRoot, beadID string) string {
	state := readAgentState(townRoot, beadID)
	if beads.AgentState(state) == beads.AgentStatePaused {
		// Already paused (idempotent re-pause after a repair): there is no
		// real prior state to capture, so fall back to the caller's default.
		return ""
	}
	return state
}

func runAgentPause(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	target, err := parseAgentAddr(args[0])
	if err != nil {
		return err
	}
	if err := checkPauseGated(target.Role); err != nil {
		return err
	}
	role, name := target.roleAndName()
	display := target.displayAddress(role, name)

	// Idempotency: already paused? A marker that exists but cannot be read
	// is reported as paused *and* as an error (agentpause.IsPaused fails
	// closed). Don't treat that as "already paused" — Pause rewrites the
	// marker atomically, which is the repair.
	if paused, st, perr := agentpause.IsPaused(townRoot, target.Rig, role, name); perr != nil {
		style.PrintWarning("existing pause marker for %s is unreadable (%v); rewriting it", display, perr)
	} else if paused {
		fmt.Printf("%s %s is already paused\n", style.Dim.Render("○"), display)
		if st != nil && st.Reason != "" {
			fmt.Printf("  Reason: %s\n", st.Reason)
		}
		return nil
	}

	// Capture the bead's current agent_state before overwriting it, so resume
	// can restore it instead of blindly writing "idle" (gt-ahik). Best-effort:
	// an unreadable bead just means resume falls back to idle, same as before.
	priorState := currentAgentState(townRoot, target.BeadID)

	// 1. Durable marker file (the only source of truth for scanners).
	if err := agentpause.Pause(townRoot, target.Rig, role, name, agentPauseReason, "human", priorState); err != nil {
		return fmt.Errorf("writing pause marker: %w", err)
	}

	// 2. Mirror agent_state=paused onto the agent bead for display (`gt
	//    polecat identity show`, dashboards). Best-effort and never
	//    consulted by any scanner — the marker file above is what gates
	//    restarts, so a Dolt blip here cannot undo the pause.
	if err := beads.New(townRoot).ForAgentBead().UpdateAgentState(target.BeadID, string(beads.AgentStatePaused)); err != nil {
		style.PrintWarning("could not mirror agent_state=paused to bead %s: %v", target.BeadID, err)
	}

	// 3. Freeze the session process group (SIGSTOP/SIGTSTP). Best-effort:
	//    a dead session still has its marker, so scanners stay away.
	froze, ferr := freezeAgentSession(target)
	if ferr != nil {
		style.PrintWarning("could not freeze session %s: %v (marker still written — scanners will honor it)", target.SessionName(), ferr)
	}

	fmt.Printf("%s %s paused\n", style.Bold.Render("⏸️"), display)
	if agentPauseReason != "" {
		fmt.Printf("  Reason: %s\n", agentPauseReason)
	}
	if froze {
		fmt.Printf("  Session: %s (frozen)\n", target.SessionName())
	} else {
		fmt.Printf("  Session: %s\n", target.SessionName())
	}
	fmt.Printf("  Marker: %s\n", agentpause.FilePath(townRoot, target.Rig, role, name))
	fmt.Println()
	fmt.Println("Witness, patrol scan, and the stuck-agent dog will not touch it.")
	fmt.Printf("Resume with: %s\n", style.Dim.Render("gt agent resume "+display))
	return nil
}

func runAgentResume(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	target, err := parseAgentAddr(args[0])
	if err != nil {
		return err
	}
	role, name := target.roleAndName()
	display := target.displayAddress(role, name)

	// The marker file is the only source of truth (gt-ahik): resume reads it,
	// not the bead. An unreadable marker counts as paused (fail closed, same
	// as PauseGate) — clear it below rather than leave a broken file behind.
	paused, st, ferr := agentpause.IsPaused(townRoot, target.Rig, role, name)
	if ferr != nil {
		style.PrintWarning("pause marker for %s is unreadable (%v); clearing it anyway", display, ferr)
	}
	if !paused {
		// No marker: check for a stale bead-layer mirror left by an earlier
		// `gt agent pause`/`resume` whose bead write raced or failed (the
		// mirror is display-only and best-effort, so this can drift from the
		// marker, which is long gone by the time anyone notices — minor
		// finding on om kgx0). Nothing else clears it, so resume does.
		if beads.AgentState(readAgentState(townRoot, target.BeadID)) == beads.AgentStatePaused {
			if err := beads.New(townRoot).ForAgentBead().UpdateAgentState(target.BeadID, string(beads.AgentStateIdle)); err != nil {
				return fmt.Errorf("clearing stale agent_state=paused mirror on bead %s: %w", target.BeadID, err)
			}
			fmt.Printf("%s %s bead mirror was still agent_state=paused (no pause marker); cleared to idle\n",
				style.Dim.Render("○"), display)
			return nil
		}
		fmt.Printf("%s %s is not paused\n", style.Dim.Render("○"), display)
		return nil
	}
	if st != nil && st.Reason != "" {
		fmt.Printf("  Was paused: %s\n", st.Reason)
	}

	// 1. Clear the marker file (no-op if it is already gone).
	if err := agentpause.Resume(townRoot, target.Rig, role, name); err != nil {
		return fmt.Errorf("removing pause marker: %w", err)
	}

	// 2. Restore agent_state to what it was before the pause, not blindly
	//    "idle" — a pause taken mid-work must resume mid-work. Falls back to
	//    idle when the marker carried no prior state (unreadable marker, or
	//    paused by an older `gt agent pause` before this field existed).
	priorState := string(beads.AgentStateIdle)
	if st != nil && st.PriorAgentState != "" {
		priorState = st.PriorAgentState
	}
	if err := beads.New(townRoot).ForAgentBead().UpdateAgentState(target.BeadID, priorState); err != nil {
		style.PrintWarning("could not restore agent_state=%s on bead %s: %v", priorState, target.BeadID, err)
	}

	// 3. Thaw the session process group (SIGCONT). Best-effort.
	thawed, terr := thawAgentSession(target)
	if terr != nil {
		style.PrintWarning("could not thaw session %s: %v (marker cleared — agent will run when next started)", target.SessionName(), terr)
	}

	fmt.Printf("%s %s resumed\n", style.Bold.Render("▶️"), display)
	if thawed {
		fmt.Printf("  Session: %s (thawed)\n", target.SessionName())
	} else {
		fmt.Printf("  Session: %s\n", target.SessionName())
	}
	return nil
}

// freezeAgentSession freezes the agent's tmux session process group.
// Returns (froze, error): froze is true when the signal was delivered.
func freezeAgentSession(target *agentAddr) (bool, error) {
	t := tmux.NewTmux()
	if running, _ := t.HasSession(target.SessionName()); !running {
		return false, nil
	}
	if err := signalSessionGroup(t, target.SessionName(), sigFreeze); err != nil {
		return false, err
	}
	return true, nil
}

// thawAgentSession thaws the agent's tmux session process group.
func thawAgentSession(target *agentAddr) (bool, error) {
	t := tmux.NewTmux()
	if running, _ := t.HasSession(target.SessionName()); !running {
		return false, nil
	}
	if err := signalSessionGroup(t, target.SessionName(), sigThaw); err != nil {
		return false, err
	}
	return true, nil
}
