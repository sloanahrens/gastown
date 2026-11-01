package cmd

import (
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/crewfresh"
	"github.com/steveyegge/gastown/internal/crewstale"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/workspace"
)

// crewStaleMaxAge is how long a crew clone may go unfetched before gt crew
// stale lists it: the day the overseer report wants named (gt-f1qng).
const crewStaleMaxAge = 24 * time.Hour

var crewStaleCmd = &cobra.Command{
	Use:   "stale",
	Short: "List crew clones not fetched in the last day",
	Long: `List every crew clone under <town>/<rig>/crew/ whose last fetch is older
than a day, oldest fetch first, one line per clone: its rig and directory, its
branch, the hours since its last fetch, and the commits origin/main has that
it lacks.

Empty output means every crew clone was fetched within a day.

The command only reads. It never fetches and never moves a clone, so it is
safe to run while a crew member is working in one.

  gt crew stale`,
	Args: cobra.NoArgs,
	RunE: runCrewStale,
}

// crewStaleDeps are the seams gt crew stale runs through, so the town, the
// clock, the filesystem and git are all a test's own.
type crewStaleDeps struct {
	findTown func() (string, error)
	now      func() time.Time
	fs       crewstale.FS
	repos    func(root string) crewstale.Git
}

// crewStaleRealDeps is the production wiring, kept in one place so a test that
// means to exercise the real filesystem and git asks for it here instead of
// rebuilding it.
func crewStaleRealDeps() crewStaleDeps {
	return crewStaleDeps{
		findTown: workspace.FindFromCwdOrError,
		now:      time.Now,
		fs:       crewstale.Disk{},
		repos:    func(root string) crewstale.Git { return git.NewGit(root) },
	}
}

func runCrewStale(cmd *cobra.Command, args []string) error {
	return crewStale(cmd.OutOrStdout(), crewStaleRealDeps())
}

// crewStale writes one line for each crew checkout not fetched within
// crewStaleMaxAge, ordered by crewfresh.Stale.
//
// A checkout that could not be read is listed too, with the failed fields
// shown as "?" and the reason after the line: an unreadable clone cannot be
// shown fresh, and hiding it would report a town as fresher than it is. Only a
// town or crew directory that cannot be listed is an error, since then the
// command has no set of clones to report on at all.
func crewStale(out io.Writer, deps crewStaleDeps) error {
	townRoot, err := deps.findTown()
	if err != nil {
		return fmt.Errorf("find the town root: %w", err)
	}
	// The clock is read once, before the walk, so every checkout is judged
	// against the same instant.
	now := deps.now()
	readings, err := crewstale.Readings(townRoot, deps.fs, deps.repos, now)
	if err != nil {
		return err
	}

	clones := make([]crewfresh.Clone, len(readings))
	byPath := make(map[string]crewstale.Reading, len(readings))
	for i, r := range readings {
		clones[i] = r.Clone
		byPath[r.Clone.Path] = r
	}

	for _, stale := range crewfresh.Stale(clones, now, crewStaleMaxAge) {
		fmt.Fprintln(out, crewStaleLine(byPath[stale.Path], now))
	}
	return nil
}

// crewStaleLine is one clone's report line: rig/dir, branch, hours since its
// last fetch, and the commits it is behind origin/main. A field whose read
// failed shows as "?".
func crewStaleLine(r crewstale.Reading, now time.Time) string {
	c := r.Clone

	branch := "?"
	if r.BranchOK {
		branch = c.Branch
		if branch == "" {
			branch = "detached"
		}
	}

	age := "?"
	if r.FetchOK {
		age = "never"
		if !c.LastFetch.IsZero() {
			age = strconv.FormatInt(int64(now.Sub(c.LastFetch).Hours()), 10) + "h"
		}
	}

	behind := "?"
	if r.BehindOK {
		behind = strconv.Itoa(c.Behind)
	}

	line := fmt.Sprintf("%s/%s  %s  %s  %s", c.Rig, c.Dir, branch, age, behind)
	if c.Err != "" {
		line += "  " + c.Err
	}
	return line
}
