package cmd

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/townbackup"
	"github.com/steveyegge/gastown/internal/townstate"
)

// townStateBackupRootSetting names the nightly backup root, which the pod mounts
// as a deployment value (docs/architecture.md).
const townStateBackupRootSetting = "maintenance.town_state_backup_root"

var townStateBackupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Judge the nightly town-state backup archives",
	Long: `Judge the nightly town-state backups the daemon writes (internal/townbackup):
one dated directory per night under a backup root outside the town root. The
root is the directory ` + townStateBackupRootSetting + ` names; the daemon pod
mounts the host's copy there, as docs/architecture.md (The daemon) says. A
night's archive is its state.tar.gz.`,
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
` + townStateBackupRootSetting + ` names; docs/architecture.md (The daemon) says
where the daemon pod mounts the host's copy.

Examples:
  gt town state backup verify <backup-root>/2026-11-18/state.tar.gz`,
	Args: cobra.ExactArgs(1),
	RunE: runTownStateBackupVerify,
}

var townStateBackupRestoreCmd = &cobra.Command{
	Use:   "restore <archive>",
	Short: "Verify a night's archive, then extract it into an empty town root",
	Long: `Extract one night's archive into the town root --town names. The archive is
judged first by the check ` + "`verify`" + ` runs, and only one it accepts is
extracted, so a refused night never leaves a root a seed could run over. The
root must hold none of the backup set: it is the scratch town the restore
runbook seeds from.`,
	Args: cobra.ExactArgs(1),
	RunE: runTownStateBackupRestore,
}

func init() {
	townStateCmd.AddCommand(townStateBackupCmd)
	townStateBackupCmd.AddCommand(townStateBackupVerifyCmd)
	townStateBackupCmd.AddCommand(townStateBackupRestoreCmd)
	townStateBackupRestoreCmd.Flags().String("town", "", "the town root to restore into (required)")
	_ = townStateBackupRestoreCmd.MarkFlagRequired("town")
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

func runTownStateBackupRestore(cmd *cobra.Command, args []string) error {
	root, err := cmd.Flags().GetString("town")
	if err != nil {
		return fmt.Errorf("reading --town: %w", err)
	}
	return restoreBackupNight(args[0], root, cmd.OutOrStdout())
}

// restoreBackupNight is what `gt town state backup restore` runs: the archive
// judged by the verify step, then imported into root only when verify accepts
// it, so a refused night leaves no root a seed could run over. Import writes
// daemon-owned rows only, so the backup set's shared rows are passed as
// daemon-owned: a fresh restore root has no session to own them. It writes the
// town settings file without the Mac's daemon.supervisor, which the seed strips
// from the claim as well.
func restoreBackupNight(archive, root string, out io.Writer) error {
	if err := runTownStateBackupVerifyIn(townStateBackupVerifyEnv{archive: archive, out: out}); err != nil {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("creating the restore root %s: %w", root, err)
	}
	f, err := os.Open(archive)
	if err != nil {
		return fmt.Errorf("restoring %s: %w", archive, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("restoring %s: %w", archive, err)
	}
	m := &townstate.Manifest{Entries: townstate.Default().Backup()}
	for i := range m.Entries {
		m.Entries[i].Owner = townstate.OwnerDaemon
	}
	s, err := townstate.Import(gz, root, m, townstate.ImportOptions{})
	if cerr := gz.Close(); cerr != nil && err == nil {
		err = cerr
	}
	fmt.Fprintln(out, s.String())
	if err != nil {
		return fmt.Errorf("restoring the town state: %w", err)
	}
	return nil
}
