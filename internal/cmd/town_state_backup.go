package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/townbackup"
)

// townStateBackupRootSetting names the nightly backup root, which the pod mounts
// as a deployment value (docs/architecture.md).
const townStateBackupRootSetting = "maintenance.town_state_backup_root"

var townStateBackupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Judge the nightly town-state backup archives",
	Long: `Judge the nightly town-state backups the daemon writes (internal/townbackup):
one dated directory per night under a backup root outside the town root. The
root is the directory ` + townStateBackupRootSetting + ` names; before running
these, read how the daemon pod mounts it in docs/architecture.md (The
Deployment). A night's archive is its state.tar.gz.`,
	RunE: requireSubcommand,
}

var townStateBackupVerifyCmd = &cobra.Command{
	Use:   "verify <archive>",
	Short: "Check a backup archive holds a town state and no credential",
	Long: `Read one night's backup archive and print its regular files and bytes. A
refusal exits non-zero and names the entry and the rule, never a value, so a
doctor check can judge the night: an archive it cannot read in full, one with
no regular file or no mayor/town.json, and one carrying a secret-shaped name or
a credential are all refused.

The archive is <backup-root>/<date>/state.tar.gz, the root being the directory
` + townStateBackupRootSetting + ` names, which the daemon pod mounts the
host's copy at (docs/architecture.md).

Examples:
  gt town state backup verify <backup-root>/2026-11-18/state.tar.gz`,
	Args: cobra.ExactArgs(1),
	RunE: runTownStateBackupVerify,
}

func init() {
	townStateCmd.AddCommand(townStateBackupCmd)
	townStateBackupCmd.AddCommand(townStateBackupVerifyCmd)
}

// townStateBackupVerifyEnv is what the verify command reads from its session:
// the archive to check and the writer the counts go to.
type townStateBackupVerifyEnv struct {
	archive string
	out     io.Writer
}

func runTownStateBackupVerify(cmd *cobra.Command, args []string) error {
	return runTownStateBackupVerifyIn(townStateBackupVerifyEnv{
		archive: args[0],
		out:     cmd.OutOrStdout(),
	})
}

// runTownStateBackupVerifyIn is what `gt town state backup verify` runs: the
// archive checked and its counts printed, or the refusal returned.
func runTownStateBackupVerifyIn(env townStateBackupVerifyEnv) error {
	res, err := townbackup.Verify(env.archive)
	if err != nil {
		return fmt.Errorf("town state backup verify: %w", err)
	}
	fmt.Fprintf(env.out, "%s: %d files, %d bytes\n", env.archive, res.Files, res.Bytes)
	return nil
}
