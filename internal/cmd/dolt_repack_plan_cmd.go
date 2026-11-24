package cmd

import (
	"fmt"
	"io"
	"os/exec"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/workspace"
)

// runDoltRepackPlanCmd prints the plan for the town's archive repository,
// resolved the way the backup patrol resolves it. The reads, writer and git
// runner are parameters so a test drives both refusals and the printed plan.
func runDoltRepackPlanCmd(w io.Writer, findTownRoot func() (string, error), readPatrol func(string) (*daemon.DaemonPatrolConfig, error), run func(argv ...string) ([]byte, error)) error {
	townRoot, err := findTownRoot()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	patrol, err := readPatrol(townRoot)
	if err != nil {
		return fmt.Errorf("reading the daemon config: %w", err)
	}
	repo, _ := daemon.JsonlBackupRepo(townRoot, patrol)
	return runDoltRepackPlan(w, repo, run)
}

// `gt dolt repack-plan` prints the bounded repack plan for the town's archive
// repository. It reads count-objects only and never runs the repack.
var doltRepackPlanCmd = &cobra.Command{
	Use:   "repack-plan",
	Short: "Print the bounded repack plan for the archive repository",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		run := func(argv ...string) ([]byte, error) { return exec.Command(argv[0], argv[1:]...).Output() }
		return runDoltRepackPlanCmd(cmd.OutOrStdout(), workspace.FindFromCwdOrError, daemon.ReadPatrolConfig, run)
	},
}
