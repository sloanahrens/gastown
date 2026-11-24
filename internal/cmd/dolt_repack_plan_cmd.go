package cmd

import (
	"fmt"
	"os/exec"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/workspace"
)

// doltArchiveRepo is the archive repository the town's repack plan covers,
// resolved the way the backup patrol does. The reads are injected for tests.
func doltArchiveRepo(findTownRoot func() (string, error), readPatrol func(string) (*daemon.DaemonPatrolConfig, error)) (string, error) {
	townRoot, err := findTownRoot()
	if err != nil {
		return "", fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	patrol, err := readPatrol(townRoot)
	if err != nil {
		return "", fmt.Errorf("reading the daemon config: %w", err)
	}
	repo, _ := daemon.JsonlBackupRepo(townRoot, patrol)
	return repo, nil
}

// `gt dolt repack-plan` prints the bounded repack plan for the town's archive
// repository. It reads count-objects only and never runs the repack.
var doltRepackPlanCmd = &cobra.Command{
	Use:   "repack-plan",
	Short: "Print the bounded repack plan for the archive repository",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		repo, err := doltArchiveRepo(workspace.FindFromCwdOrError, daemon.ReadPatrolConfig)
		if err != nil {
			return err
		}
		run := func(argv ...string) ([]byte, error) { return exec.Command(argv[0], argv[1:]...).Output() }
		return runDoltRepackPlan(cmd.OutOrStdout(), repo, run)
	},
}
