package cmd

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/townstate"
	"github.com/steveyegge/gastown/internal/workspace"
)

// townCmd reuses a name the D7 cleanup deleted (gt-638go.6): the split needs a
// town-level verify surface, and this is the name for it (gt-gg9hv.67).
var townCmd = &cobra.Command{
	Use:     "town",
	GroupID: GroupDiag,
	Short:   "Inspect the ownership split of the town root",
	RunE:    requireSubcommand,
}

var townStateCmd = &cobra.Command{
	Use:   "state",
	Short: "Inspect, verify and copy the daemon/session split of the town root",
	Long: `Inspect the split that decides what moves to the town-state volume and
what stays on the host. internal/townstate holds the split as one manifest;
its YAML mirror is deploy/daemon/town-state.yaml.

The rows and what the three owners mean are documented in internal/townstate.
"export" and "import" copy the daemon-owned set to the volume and back: the
seed of the cut-over and its rollback.`,
	RunE: requireSubcommand,
}

var townStateVerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Check the town root against the split manifest",
	Long: `Walk the town root and report every path that does not match the split
manifest (internal/townstate; the rules, what is skipped and where the rig and
bird names come from are written there, once).

A failure is a path no manifest entry covers, or two entries that overlap with
different owners. A line starting "info:" is not a failure: file metadata cannot
say who wrote a file, so a recent change under a session entry is only reported.
The command exits 1 when there is a failure, and prints "town state: ok" and
exits 0 otherwise. Only metadata is read, never file contents.

Examples:
  gt town state verify`,
	Args: cobra.NoArgs,
	RunE: runTownStateVerify,
}

var townStateExportCmd = &cobra.Command{
	Use:   "export",
	Short: "Write the daemon-owned paths of a town root to standard output as a tar stream",
	Long: `Write the daemon-owned entries of the manifest to standard output as a tar
stream whose names are relative to the town root, so the copy crosses a
process boundary without a shared filesystem. A summary goes to standard
error: counts, bytes and the classes of entry left out (Dolt data, git trees,
sockets, lock files, special files, symlinks leaving the town root), never the
names of files below a top-level entry.

The walk never follows a symlink. The rig clones' .git directories are left
out and re-fetched from Forgejo after the import. Dolt data has its own backup
path and never crosses this stream. With --rollback only the rows tagged
rollback: true are written: the one-way state a rollback replays onto the Mac
tree, which with --since is the bounded replay.

Examples:
  gt town state export --town ~/gt > town-state.tar
  gt town state export --town ~/gt --since 2026-11-06T14:30:00Z > delta.tar
  gt town state export --town /volume --rollback --since 2026-11-06T14:30:00Z > rollback.tar`,
	Args: cobra.NoArgs,
	RunE: runTownStateExport,
}

var townStateImportCmd = &cobra.Command{
	Use:   "import",
	Short: "Restore a daemon-owned set from a tar stream on standard input",
	Long: `Read the tar stream "gt town state export" writes from standard input and
restore it into the town root --town names. The root must hold none of the
daemon-owned paths (the seed's empty volume). With --merge-newer the command is
the rollback's replay onto a populated tree: a regular file is written only
when the archive's copy is newer.

The archive is untrusted. A name that is absolute, contains "..", or resolves
through a symlink is refused, and so is any entry that is not a regular file, a
directory or a symlink within the town root. Nothing outside the daemon-owned
manifest rows is written.

Examples:
  gt town state import --town ~/gt < town-state.tar
  gt town state export --town /volume --rollback --since 2026-11-06T14:30:00Z > rollback.tar
  gt town state import --town ~/gt --merge-newer < rollback.tar`,
	Args: cobra.NoArgs,
	RunE: runTownStateImport,
}

func init() {
	townStateCmd.AddCommand(townStateVerifyCmd)
	townStateCmd.AddCommand(townStateExportCmd)
	townStateCmd.AddCommand(townStateImportCmd)
	townCmd.AddCommand(townStateCmd)
	rootCmd.AddCommand(townCmd)

	townStateExportCmd.Flags().String("town", "", "the town root to export (required)")
	townStateExportCmd.Flags().String("since", "", "keep only regular files modified after this RFC 3339 time (optional)")
	townStateExportCmd.Flags().Bool("rollback", false, "write only the rows tagged rollback: true (the one-way state a rollback replays)")
	_ = townStateExportCmd.MarkFlagRequired("town")
	townStateImportCmd.Flags().String("town", "", "the town root to import into (required)")
	townStateImportCmd.Flags().Bool("merge-newer", false, "write a regular file only when the archive's copy is newer (the rollback's replay)")
	_ = townStateImportCmd.MarkFlagRequired("town")
}

// townStateVerifyEnv is what the verify command reads from its session.
type townStateVerifyEnv struct {
	findTown func() (string, error)
	now      func() time.Time
	out      io.Writer
}

func runTownStateVerify(cmd *cobra.Command, _ []string) error {
	return runTownStateVerifyIn(townStateVerifyRunEnv(cmd))
}

// townStateVerifyRunEnv is the session values the command runs with.
func townStateVerifyRunEnv(cmd *cobra.Command) townStateVerifyEnv {
	return townStateVerifyEnv{
		findTown: workspace.FindFromCwdOrError,
		now:      time.Now,
		out:      cmd.OutOrStdout(),
	}
}

// runTownStateVerifyIn is what `gt town state verify` runs.
func runTownStateVerifyIn(env townStateVerifyEnv) error {
	townRoot, err := env.findTown()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	return verifyTownState(env.out, os.DirFS(townRoot), townstate.Default(), env.now())
}

// verifyTownState is what `gt town state verify` runs: the split checked
// against fsys, one line per problem, and an exit status of 1 when any.
func verifyTownState(w io.Writer, fsys fs.FS, m *townstate.Manifest, now time.Time) error {
	problems, err := townstate.Verify(fsys, m, now)
	if err != nil {
		return fmt.Errorf("verifying the town root: %w", err)
	}
	for _, p := range problems {
		fmt.Fprintln(w, p.String())
	}
	if n := townstate.Failures(problems); n > 0 {
		return &ExitCodeError{Code: 1, Err: fmt.Errorf("town state: %d paths do not match the split", n)}
	}
	fmt.Fprintln(w, "town state: ok")
	return nil
}

// townStateExportEnv is what the export command reads from its session.
type townStateExportEnv struct {
	town     string
	since    string
	rollback bool
	out      io.Writer
	summary  io.Writer
}

func runTownStateExport(cmd *cobra.Command, _ []string) error {
	town, err := cmd.Flags().GetString("town")
	if err != nil {
		return fmt.Errorf("reading --town: %w", err)
	}
	since, err := cmd.Flags().GetString("since")
	if err != nil {
		return fmt.Errorf("reading --since: %w", err)
	}
	rollback, err := cmd.Flags().GetBool("rollback")
	if err != nil {
		return fmt.Errorf("reading --rollback: %w", err)
	}
	return runTownStateExportIn(townStateExportEnv{
		town:     town,
		since:    since,
		rollback: rollback,
		out:      cmd.OutOrStdout(),
		summary:  cmd.ErrOrStderr(),
	})
}

// runTownStateExportIn is what `gt town state export` runs: the archive to
// out, the summary to summary. The summary is written even when the export
// fails, because its counts say what reached the stream before the failure.
func runTownStateExportIn(env townStateExportEnv) error {
	var since time.Time
	if env.since != "" {
		parsed, err := time.Parse(time.RFC3339, env.since)
		if err != nil {
			return fmt.Errorf("--since %q is not an RFC 3339 time", env.since)
		}
		since = parsed
	}
	m := townstate.Default()
	if env.rollback {
		m = &townstate.Manifest{Entries: m.Rollback()}
	}
	s, err := townstate.Export(env.out, env.town, m, townstate.ExportOptions{Since: since})
	fmt.Fprintln(env.summary, s.String())
	if err != nil {
		return fmt.Errorf("exporting the town state: %w", err)
	}
	return nil
}

// townStateImportEnv is what the import command reads from its session.
type townStateImportEnv struct {
	town       string
	mergeNewer bool
	in         io.Reader
	out        io.Writer
}

func runTownStateImport(cmd *cobra.Command, _ []string) error {
	town, err := cmd.Flags().GetString("town")
	if err != nil {
		return fmt.Errorf("reading --town: %w", err)
	}
	mergeNewer, err := cmd.Flags().GetBool("merge-newer")
	if err != nil {
		return fmt.Errorf("reading --merge-newer: %w", err)
	}
	return runTownStateImportIn(townStateImportEnv{
		town:       town,
		mergeNewer: mergeNewer,
		in:         cmd.InOrStdin(),
		out:        cmd.OutOrStdout(),
	})
}

// runTownStateImportIn is what `gt town state import` runs. The summary is
// written even when the import fails, so a refused archive still reports what
// its entries up to the refusal wrote.
func runTownStateImportIn(env townStateImportEnv) error {
	s, err := townstate.Import(env.in, env.town, townstate.Default(), townstate.ImportOptions{MergeNewer: env.mergeNewer})
	fmt.Fprintln(env.out, s.String())
	if err != nil {
		return fmt.Errorf("importing the town state: %w", err)
	}
	return nil
}
