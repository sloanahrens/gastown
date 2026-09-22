// Per-agent sanctioned pause (gt agent pause / gt agent resume).
//
// A mayor/operator freeze (SIGSTOP) of a misbehaving agent is
// indistinguishable from a stuck agent: the stuck-agent dog respawned a
// frozen flint 20 minutes later, and the witness patrol restarted parked
// agents through the done-intent-dead path (gt-ahik). This command is the
// sanctioned freeze: it writes a durable pause marker, syncs the agent
// bead to agent_state=paused, and freezes the session's process group.
//
// Every scanner that can resurrect a session (witness zombie detection /
// patrol scan, the polecat staleness assessor, the stuck-agent dog)
// consults the pause state and reports "do not touch" instead of
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

var (
	agentPauseReason string
	agentResumeForce bool
)

var agentCmd = &cobra.Command{
	Use:     "agent",
	GroupID: GroupAgents,
	Short:   "Manage agents (pause/resume individual agents)",
	RunE:    requireSubcommand,
	Long: `Manage agents.

  gt agent pause <address> --reason "..."   Freeze an agent in place
  gt agent resume <address>                 Thaw a paused agent

The address is a Gas Town address: <rig>/<name> for a polecat,
<rig>/witness, <rig>/refinery, or mayor / deacon.`,
}

var agentPauseCmd = &cobra.Command{
	Use:   "pause <address>",
	Short: "Pause an agent: write a pause marker, sync agent_state=paused, and freeze the session",
	Long: `Pause an agent so no scanner (witness, patrol scan, stuck-agent dog,
polecat staleness) will restart or nuke it.

Writes a durable pause marker, syncs the agent bead to
agent_state=paused, and freezes the tmux session's process group
(context is preserved — no work is lost).

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

Clears the pause marker, restores the agent bead state to idle,
and thaws the session's process group (SIGCONT).

Use --force to resume even if the session is not currently frozen.`,
	Args: cobra.ExactArgs(1),
	RunE: runAgentResume,
}

func init() {
	agentPauseCmd.Flags().StringVarP(&agentPauseReason, "reason", "r", "", "Reason for the pause (shown in gt status)")
	agentResumeCmd.Flags().BoolVar(&agentResumeForce, "force", false, "Resume even if the session is not frozen")
	agentCmd.AddCommand(agentPauseCmd)
	agentCmd.AddCommand(agentResumeCmd)
	rootCmd.AddCommand(agentCmd)
}

// agentAddr is a parsed pause target: identity plus its agent bead ID.
type agentAddr struct {
	*session.AgentIdentity
	BeadID string
}

// parseAgentAddr parses an address for gt agent pause/resume.
// Accepts <rig>/<name> (polecat), <rig>/witness, <rig>/refinery,
// mayor, and deacon.
func parseAgentAddr(address string) (*agentAddr, error) {
	id, err := session.ParseAddress(address)
	if err != nil {
		return nil, fmt.Errorf("invalid agent address %q: %w", address, err)
	}
	var role, name string
	switch id.Role {
	case session.RoleWitness:
		role, name = constants.RoleWitness, ""
	case session.RoleRefinery:
		role, name = constants.RoleRefinery, ""
	case session.RoleCrew:
		role, name = constants.RoleCrew, id.Name
	case session.RolePolecat:
		role, name = constants.RolePolecat, id.Name
	default:
		role, name = string(id.Role), id.Name
	}
	var beadID string
	switch id.Role {
	case session.RoleMayor:
		beadID = beads.MayorBeadIDTown()
	case session.RoleDeacon:
		beadID = beads.DeaconBeadIDTown()
	default:
		beadID = beads.AgentBeadIDWithPrefix(session.PrefixFor(id.Rig), id.Rig, role, name)
	}
	return &agentAddr{AgentIdentity: id, BeadID: beadID}, nil
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

func runAgentPause(cmd *cobra.Command, args []string) error {
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

	// 1. Durable marker file (primary source of truth for scanners).
	if err := agentpause.Pause(townRoot, target.Rig, role, name, agentPauseReason, "human"); err != nil {
		return fmt.Errorf("writing pause marker: %w", err)
	}

	// 2. Sync agent_state=paused to the agent bead so the stuck-agent
	//    dog (which reads `gt polecat identity show --json`) and any
	//    ZFC reader see authoritative pause state. Best-effort: a
	//    Dolt blip should not undo the marker file.
	if err := beads.New(townRoot).ForAgentBead().UpdateAgentState(target.BeadID, string(beads.AgentStatePaused)); err != nil {
		style.PrintWarning("could not sync agent_state=paused to bead %s: %v", target.BeadID, err)
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
	client := beads.New(townRoot)

	// Resume must consult BOTH layers, exactly as PauseGate does: a pause
	// held in the bead layer alone (the marker-file-loss case the fallback
	// exists for) is otherwise unclearable, and the stuck-agent dog would
	// skip a live agent forever (gt-wisp-6ajo). The already-resolved bead ID
	// is passed through so town-level agents (mayor, deacon), which have no
	// rig segment to derive one from, are covered too.
	layers := agentpause.ReadLayers(townRoot, target.Rig, role, name, target.BeadID, client.ForAgentBead())
	if layers.FileErr != nil {
		// The marker is unreadable, not absent: it counts as paused, and
		// Resume below removes (rather than parses) it.
		style.PrintWarning("pause marker for %s is unreadable (%v); clearing it anyway", display, layers.FileErr)
	}
	if layers.BeadErr != nil {
		// Unknown, not unpaused: warn and still clear both layers, since
		// clearing is idempotent and this is the operator's only undo.
		style.PrintWarning("could not read agent bead %s (%v); clearing both layers anyway", target.BeadID, layers.BeadErr)
	}
	if !layers.Paused() {
		fmt.Printf("%s %s is not paused\n", style.Dim.Render("○"), display)
		return nil
	}
	switch st := layers.State(); {
	case st != nil && st.Reason != "":
		fmt.Printf("  Was paused: %s\n", st.Reason)
	case layers.BeadPaused && !layers.FilePaused:
		// The marker file was gone; the bead layer is what held it.
		fmt.Println("  Was paused: agent bead was still agent_state=paused")
	}

	// 1. Clear the marker file (no-op if it is already gone).
	if err := agentpause.Resume(townRoot, target.Rig, role, name); err != nil {
		return fmt.Errorf("removing pause marker: %w", err)
	}

	// 2. Restore agent_state. The agent was running (or idle) before the
	//    freeze; "idle" is the safe canonical state — polecat_spawn writes
	//    "working" on the next spawn, and nothing treats "idle" as terminal.
	//    Done unconditionally: a bead-layer-only pause is exactly the case
	//    that must be cleared here.
	if err := client.ForAgentBead().UpdateAgentState(target.BeadID, string(beads.AgentStateIdle)); err != nil {
		style.PrintWarning("could not restore agent_state=idle on bead %s: %v", target.BeadID, err)
	}

	// 3. Thaw the session process group (SIGCONT). Best-effort.
	thawed, terr := thawAgentSession(target)
	if terr != nil {
		if agentResumeForce {
			fmt.Printf("  (note: thaw failed: %v)\n", terr)
		} else {
			style.PrintWarning("could not thaw session %s: %v (marker cleared — agent will run when next started)", target.SessionName(), terr)
		}
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