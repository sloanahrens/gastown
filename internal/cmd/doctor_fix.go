package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/doctor"
	"github.com/steveyegge/gastown/internal/ui"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	doctorFixRig             string
	doctorFixVerbose         bool
	doctorFixRestartSessions bool
	doctorFixNoStart         bool
	doctorFixAuthorizedBy    string
)

var doctorFixCmd = &cobra.Command{
	Use:   "fix <check>",
	Short: "Repair exactly one named check",
	Long: `Repair one named health check.

'gt doctor' only reports. A repair names the single check to run, so there is
no town-wide '--fix' — an agent following remediation text can no longer repair
parts of the town it never meant to touch (deep review G4-09).

The fixer refuses when it cannot know what to repair:
  - an unknown check name,
  - a check that could not determine a result (UNKNOWN / skipped), because a
    fixer acting on an unverified assumption is exactly the gt-fcxe9.1 hazard,
  - a report-only check with no repair.

A repair that can destroy state or end a running process — it kills a session,
removes a repository, purges rows — requires recorded authorization when an
agent runs it. Pass --authorized-by <bead-id>, an open bead labeled
'doctor-fix-auth' that the overseer created. A human at a terminal is not
asked.

Exit code is non-zero when the repair refused, failed, or did not clear the
check.`,
	Args: cobra.ExactArgs(1),
	RunE: runDoctorFix,
}

func init() {
	doctorFixCmd.Flags().BoolVarP(&doctorFixVerbose, "verbose", "v", false, "Show detailed output")
	doctorFixCmd.Flags().StringVar(&doctorFixRig, "rig", "", "Check specific rig only")
	doctorFixCmd.Flags().BoolVar(&doctorFixRestartSessions, "restart-sessions", false, "Restart patrol sessions when fixing stale settings")
	doctorFixCmd.Flags().BoolVar(&doctorFixNoStart, "no-start", false, "Suppress starting the daemon and agents")
	doctorFixCmd.Flags().StringVar(&doctorFixAuthorizedBy, "authorized-by", "", "Bead ID recording authorization for a destructive repair (required for one when run by an agent)")
}

func runDoctorFix(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	name := args[0]

	ctx := &doctor.CheckContext{
		TownRoot:        townRoot,
		RigName:         doctorFixRig,
		Verbose:         doctorFixVerbose,
		RestartSessions: doctorFixRestartSessions,
		NoStart:         doctorFixNoStart,
	}

	d := newDoctorForCommand(doctorFixRig)
	d.Only([]string{name})
	if len(d.Checks()) == 0 {
		return fmt.Errorf("%w: %q (run 'gt doctor' to list checks)", doctor.ErrUnknownCheck, name)
	}

	fmt.Println() // Initial blank line
	result, err := d.FixOne(ctx, name, os.Stdout, func(check doctor.Check) error {
		return authorizeDoctorFix(townRoot, check)
	})
	if err != nil {
		return fmt.Errorf("gt doctor fix %s: %w", name, err)
	}
	if !result.Fixed && result.Status != doctor.StatusOK {
		return fmt.Errorf("gt doctor fix %s: still %s after repair", name, result.Status)
	}
	return nil
}

// authorizeDoctorFix enforces the gt-638go.3 guardrail for the command's own
// flags and environment.
func authorizeDoctorFix(townRoot string, check doctor.Check) error {
	return authorizeDestructiveFix(townRoot, check, agentActor(os.Getenv), ui.IsTerminal(), doctorFixAuthorizedBy)
}

// authorizeDestructiveFix enforces the gt-638go.3 guardrail: a destructive
// repair run by an agent needs recorded authorization. Identity is corroborated,
// not just read from env — unset GT_ROLE/BD_ACTOR off-terminal is treated as an
// agent, so an agent cannot pose as a human operator by unsetting its env
// (gt-2oy). A human at an interactive terminal approves by typing the command.
//
// The actor and the --authorized-by bead are parameters rather than reads of the
// environment and the command's flag variables, so a test can exercise each
// refusal without mutating process state (already-commissioned test policy).
func authorizeDestructiveFix(townRoot string, check doctor.Check, envActor string, stdoutIsTTY bool, authorizedBy string) error {
	actor, isAgent := resolveDestructiveActor(envActor, stdoutIsTTY)
	if !isAgent {
		return nil
	}
	if authorizedBy == "" {
		return fmt.Errorf(`%q can destroy state or end a running process, and agent actor %q may not run it without recorded authorization (gt-638go.3)

Get approval from the overseer, then name the bead that records the
decision:
  gt doctor fix %s --authorized-by <bead-id>`, check.Name(), actor, check.Name())
	}
	issue, err := beads.New(townRoot).Show(authorizedBy)
	if err != nil {
		return fmt.Errorf("--authorized-by bead %q not found: %w", authorizedBy, err)
	}
	return doctor.ValidateFixAuthorization(issue, actor)
}
