// gt kill-all: end every agent session, through the supervisor (gt-4k3fj.4).
//
// An E-stop leaves running sessions alone; kill-all is the explicit verb that
// ends them. It sets the E-stop first, so nothing dispatches into or restarts
// the seats it kills, then kills each seat through supervisor.KillAll, the
// one verb an E-stop and a seat's hold do not refuse. Every kill lands in the
// supervisor's action log with the actor and reason.
package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/supervisor"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	killAllReason string
	killAllRig    string
	killAllCrew   bool
	killAllYes    bool
)

var killAllCmd = &cobra.Command{
	Use:     "kill-all",
	GroupID: GroupServices,
	Short:   "E-stop and kill every agent session, through the supervisor",
	Long: `Kill every agent session in the town (or one rig with --rig).

kill-all first sets the E-stop (gt estop), so nothing is dispatched or
restarted afterwards, then kills each agent seat's session through the
supervisor. It is the one kill an E-stop or a parked seat does not refuse;
each kill is logged with your actor and the reason in
.runtime/supervisor/actions.jsonl. A parked seat stays parked.

Crew sessions are human seats and are left alone unless --crew is given.
The overseer session is never killed. Sessions that do not name a known
seat are listed and skipped.

Without --yes it only prints what it would kill.

To resume afterwards: gt thaw [--rig <name>], then start what you need.

Examples:
  gt kill-all                          # Show what would be killed
  gt kill-all --yes -r "runaway spend" # E-stop and kill the town's agents
  gt kill-all --rig gastown --yes      # E-stop and kill gastown's agents`,
	Args: cobra.NoArgs,
	RunE: runKillAll,
}

func init() {
	killAllCmd.Flags().StringVarP(&killAllReason, "reason", "r", "", "Reason, recorded with the E-stop and every kill")
	killAllCmd.Flags().StringVar(&killAllRig, "rig", "", "Only this rig (instead of the whole town)")
	killAllCmd.Flags().BoolVar(&killAllCrew, "crew", false, "Also kill crew sessions")
	killAllCmd.Flags().BoolVar(&killAllYes, "yes", false, "Actually set the E-stop and kill (default: show the plan)")
	rootCmd.AddCommand(killAllCmd)
}

func runKillAll(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	reg := townRegistry()
	t := tmux.NewTmux()
	if !t.IsAvailable() {
		return fmt.Errorf("tmux not available: cannot list sessions to kill")
	}
	sup := supervisor.New(supervisor.Options{TownRoot: townRoot, Tmux: t, Prefixes: reg})
	req := killAllRequest{
		TownRoot: townRoot,
		Rig:      killAllRig,
		Reason:   killAllReason,
		Actor:    "gt kill-all/" + detectActor(),
		Crew:     killAllCrew,
		Yes:      killAllYes,
	}
	return killAll(cmd.OutOrStdout(), req, reg, collectGTSessions(reg, t, townRoot), sup)
}

// killAllRequest is one gt kill-all invocation.
type killAllRequest struct {
	TownRoot string
	Rig      string // "" for the whole town
	Reason   string
	Actor    string
	Crew     bool // include crew seats
	Yes      bool // act; otherwise only print the plan
}

// killAllPlan splits sessions into the seats kill-all ends and the sessions
// it leaves, each with why.
func killAllPlan(reg *session.PrefixRegistry, sessions []string, rig string, crew bool) (kill []supervisor.Seat, skip []string) {
	for _, name := range sessions {
		if name == session.OverseerSessionName() {
			skip = append(skip, name+": overseer")
			continue
		}
		seat, err := supervisor.SeatForSession(reg, name)
		switch {
		case err != nil:
			skip = append(skip, name+": not a known seat")
		case rig != "" && seat.Rig != rig:
			// Another rig's session: not this kill-all's business.
		case seat.Role == session.RoleCrew && !crew:
			skip = append(skip, name+": crew (use --crew)")
		default:
			kill = append(kill, seat)
		}
	}
	return kill, skip
}

// killAll sets the E-stop and kills the planned seats through sup, or only
// prints the plan when req.Yes is false. A failed kill is reported and the
// rest still run; the command fails if any did.
func killAll(w io.Writer, req killAllRequest, reg *session.PrefixRegistry, sessions []string, sup *supervisor.Supervisor) error {
	kill, skip := killAllPlan(reg, sessions, req.Rig, req.Crew)
	for _, s := range skip {
		fmt.Fprintf(w, "   %s %s (skipped)\n", style.Dim.Render("⏭"), s)
	}
	if !req.Yes {
		for _, seat := range kill {
			fmt.Fprintf(w, "   would kill %s\n", seat.SessionName())
		}
		fmt.Fprintf(w, "%d session(s) would be killed. Re-run with --yes to set the E-stop and kill them.\n", len(kill))
		return nil
	}

	if err := activateEstop(w, req.TownRoot, req.Rig, req.Reason); err != nil {
		return err
	}
	reason := "kill-all"
	if req.Reason != "" {
		reason += ": " + req.Reason
	}
	killed, failed := 0, 0
	for _, seat := range kill {
		if err := sup.KillAll(seat, reason, req.Actor); err != nil {
			fmt.Fprintf(w, "   %s %s: %v\n", style.Warning.Render("!"), seat.SessionName(), err)
			failed++
			continue
		}
		fmt.Fprintf(w, "   %s %s\n", style.Error.Render("✗"), seat.SessionName())
		killed++
	}
	fmt.Fprintf(w, "%s %d session(s) killed\n", style.Error.Render("⛔"), killed)
	if failed > 0 {
		return fmt.Errorf("kill-all: %d of %d kill(s) failed", failed, len(kill))
	}
	return nil
}
