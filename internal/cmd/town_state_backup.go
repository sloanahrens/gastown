package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/townbackup"
)

// townStateBackupMount is where the daemon pod sees the backup root. It is the
// path the command's help names, since the command is meant to run in the pod.
const townStateBackupMount = "/Users/Shared/gt-backups/town-state"

var townStateBackupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Judge the nightly town-state backup archives",
	Long: `Judge the nightly town-state backups the daemon writes (internal/townbackup):
one dated directory per night under a backup root outside the town root.

Run these in the daemon pod through the wrapper (gt-wrap), where the backup
root is mounted at ` + townStateBackupMount + `; on the Mac it is
<home>/gt-backups/town-state. The pod's mount is the view the daemon writes
through, so a person or a doctor check judges the night the daemon took.`,
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

Run it in the daemon pod through the wrapper (gt-wrap), where the backup root
is mounted at ` + townStateBackupMount + `: the archive of a night is its
state.tar.gz.

Examples:
  gt town state backup verify ` + townStateBackupMount + `/2026-11-18/state.tar.gz`,
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
