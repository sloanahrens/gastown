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
	Short: "Inspect and verify the daemon/session split of the town root",
	Long: `Inspect the split that decides what moves to the town-state volume and
what stays on the host. internal/townstate holds the split as one manifest;
its YAML mirror is deploy/daemon/town-state.yaml.

The rows and what the three owners mean are documented in internal/townstate.`,
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

func init() {
	townStateCmd.AddCommand(townStateVerifyCmd)
	townCmd.AddCommand(townStateCmd)
	rootCmd.AddCommand(townCmd)
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
