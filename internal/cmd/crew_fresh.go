package cmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/crewfresh"
	"github.com/steveyegge/gastown/internal/workspace"
)

// crewFreshTimeout bounds the one network call gt crew fresh makes. A fetch
// that outlives it is reported as failed, so the command never stalls a
// session on an unreachable remote.
const crewFreshTimeout = 5 * time.Second

// crewFreshRun asks git about one crew clone and makes the move crewfresh
// allows. It is a variable so a test drives the command without git.
var crewFreshRun = func(root string) (crewfresh.Report, error) {
	return crewfresh.Run(crewfresh.NewGit(root), crewFreshTimeout)
}

var crewFreshCmd = &cobra.Command{
	Use:   "fresh",
	Short: "Fetch in your own crew clone and fast-forward main when that is safe",
	Long: `Fetch origin in the crew clone that holds your current directory and
fast-forward its local main to origin/main when the clone is on main, clean
and a strict ancestor of origin/main.

The command acts only on a clone under <town>/<rig>/crew/, so it never fetches
in mayor/rig or a polecat worktree. A failed fetch is a message and exit 0:
the command informs, it never blocks.

Run it from your own clone:

  gt crew fresh`,
	Args: cobra.NoArgs,
	RunE: runCrewFresh,
}

func runCrewFresh(cmd *cobra.Command, args []string) error {
	return crewFresh(cmd.OutOrStdout(), os.Getwd, workspace.FindFromCwdOrError, crewFreshRun)
}

// crewFresh runs the freshness check for the clone that holds the caller's
// directory and writes the report's one line. The lookups and the runner are
// parameters so a test drives every branch without a process.
//
// Run's error is returned, so a clone that could not be read, or whose
// fast-forward failed, is not reported as a clean one; a failed fetch arrives
// as a message with no error, which is why it still exits 0.
func crewFresh(
	out io.Writer,
	getwd func() (string, error),
	findTownRoot func() (string, error),
	run func(string) (crewfresh.Report, error),
) error {
	cwd, err := getwd()
	if err != nil {
		return fmt.Errorf("read the current directory: %w", err)
	}
	townRoot, err := findTownRoot()
	if err != nil {
		return fmt.Errorf("find the town root: %w", err)
	}
	root, err := crewCloneRoot(cwd, townRoot)
	if err != nil {
		return err
	}
	report, err := run(root)
	if report.Message != "" {
		fmt.Fprintln(out, report.Message)
	}
	return err
}

// crewCloneRoot returns the crew clone that holds cwd, the one place gt crew
// fresh may fetch and move main: <townRoot>/<rig>/crew/<dir>.
//
// It refuses a path outside the town, a path outside a rig's crew directory
// (mayor/rig and polecat worktrees included), and a path with a ".." segment,
// whose real target the lexical test below cannot vouch for. It reads no
// environment variable.
func crewCloneRoot(cwd, townRoot string) (string, error) {
	if !filepath.IsAbs(townRoot) {
		return "", fmt.Errorf("town root %q is not an absolute path", townRoot)
	}
	if !filepath.IsAbs(cwd) {
		return "", fmt.Errorf("%q is not an absolute crew clone path", cwd)
	}
	if hasParentSegment(cwd) {
		return "", fmt.Errorf("%q is not a crew clone: a path with .. cannot be checked", cwd)
	}

	cleanTown := filepath.Clean(townRoot)
	prefix := cleanTown
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	rel, under := strings.CutPrefix(filepath.Clean(cwd), prefix)
	// A clone is <town>/<rig>/crew/<dir>, and anything below <dir> belongs to
	// it. The town root itself, a sibling whose name merely starts with the
	// town's, and every path outside all fail the cut.
	parts := strings.Split(rel, string(filepath.Separator))
	if !under || len(parts) < 3 || parts[1] != "crew" || parts[2] == "" {
		return "", fmt.Errorf("%q is not a crew clone under %s", cwd, cleanTown)
	}
	return filepath.Join(cleanTown, parts[0], "crew", parts[2]), nil
}

// hasParentSegment reports whether p holds a ".." segment. Cleaning first would
// erase it, and with it the reason to refuse.
func hasParentSegment(p string) bool {
	for _, part := range strings.Split(p, string(filepath.Separator)) {
		if part == ".." {
			return true
		}
	}
	return false
}
