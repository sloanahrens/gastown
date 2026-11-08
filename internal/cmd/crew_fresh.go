package cmd

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/callercwd"
	"github.com/steveyegge/gastown/internal/crewfresh"
	"github.com/steveyegge/gastown/internal/workspace"
)

// crewFreshTimeout bounds the one network call gt crew fresh makes. A fetch
// that outlives it is reported as failed, so the command never stalls a
// session on an unreachable remote.
const crewFreshTimeout = 5 * time.Second

// crewFreshRun is the production runner runCrewFresh hands crewFresh: the real
// crewfresh package, run in the clone crewFresh resolved. crewFresh takes the
// runner as a parameter, so no test calls this one.
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
	return crewFresh(cmd.OutOrStdout(), callercwd.Require, workspace.FindFromCwdOrError, crewFreshRun, filepath.EvalSymlinks)
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
	eval func(string) (string, error),
) error {
	cwd, err := getwd()
	if err != nil {
		return fmt.Errorf("read the current directory: %w", err)
	}
	townRoot, err := findTownRoot()
	if err != nil {
		return fmt.Errorf("find the town root: %w", err)
	}
	root, err := crewCloneRootResolved(cwd, townRoot, eval)
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
//
// The lexical answer is then resolved through symlinks and must still be a
// crew clone: a crew entry that is a symlink out of the crew directory — a
// polecat worktree, say — would otherwise pass the shape test and have git run
// in the place it points at.
func crewCloneRoot(cwd, townRoot string) (string, error) {
	return crewCloneRootResolved(cwd, townRoot, filepath.EvalSymlinks)
}

// crewCloneRootResolved is crewCloneRoot with symlink resolution given, so a
// test drives both answers without the host filesystem.
//
// A path that does not exist has no target to resolve to, so the resolution's
// ENOENT leaves the lexical answer standing: there is no symlink there to
// escape through, and a clone that is not there fails the fetch it was found
// for. Any other resolution failure is refused.
func crewCloneRootResolved(cwd, townRoot string, eval func(string) (string, error)) (string, error) {
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
	// A clone is <town>/<rig>/crew/<dir>, and anything below <dir> belongs to
	// it. The town root itself, a sibling whose name merely starts with the
	// town's, and every path outside all fail the cut.
	rig, dir, ok := crewCloneParts(cleanTown, cwd)
	if !ok {
		return "", fmt.Errorf("%q is not a crew clone under %s", cwd, cleanTown)
	}
	root := filepath.Join(cleanTown, rig, "crew", dir)

	// The same cut, on what the paths really are. A clone that resolves
	// outside the crew directory is refused, and so is one that cannot be
	// read: an unreadable clone is not a crew clone.
	resolvedTown, ok, err := crewResolve(cleanTown, eval)
	if err != nil {
		return "", fmt.Errorf("resolve the town root %s: %w", cleanTown, err)
	}
	if !ok {
		return root, nil // no town root on disk: nothing to have been a symlink
	}
	resolvedRoot, ok, err := crewResolve(root, eval)
	if err != nil {
		return "", fmt.Errorf("resolve the crew clone %s: %w", root, err)
	}
	if !ok {
		return root, nil
	}
	if _, _, ok := crewCloneParts(filepath.Clean(resolvedTown), resolvedRoot); !ok {
		return "", fmt.Errorf("%q resolves to %q, which is not a crew clone under %s", cwd, resolvedRoot, cleanTown)
	}
	return root, nil
}

// crewResolve resolves p through symlinks. ok is false, with no error, when p
// does not exist: a path that is not there has no target, so the caller keeps
// its lexical answer rather than refusing.
func crewResolve(p string, eval func(string) (string, error)) (resolved string, ok bool, err error) {
	resolved, err = eval(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return resolved, true, nil
}

// crewCloneParts cuts p's path below town into the rig and crew directory it
// names, and reports whether p sits in a crew clone at all:
// <town>/<rig>/crew/<dir>, and anything below <dir>.
func crewCloneParts(town, p string) (rig, dir string, ok bool) {
	prefix := town
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	rel, under := strings.CutPrefix(filepath.Clean(p), prefix)
	parts := strings.Split(rel, string(filepath.Separator))
	if !under || len(parts) < 3 || parts[1] != "crew" || parts[2] == "" {
		return "", "", false
	}
	return parts[0], parts[2], true
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
