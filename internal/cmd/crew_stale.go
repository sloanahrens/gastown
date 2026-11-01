package cmd

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/crewfresh"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/workspace"
)

// crewStaleMaxAge is how long a crew clone may go unfetched before gt crew
// stale lists it: the day the overseer report wants named (gt-f1qng).
const crewStaleMaxAge = 24 * time.Hour

// crewStaleOriginMain is the ref every crew clone tracks, so it is the one a
// clone's commits-behind count is taken against.
const crewStaleOriginMain = "origin/main"

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

// crewStaleGit is the read-only git surface gt crew stale asks about one
// checkout. Every method reads, so a checkout the command examines is left
// exactly as it found it.
type crewStaleGit interface {
	// CurrentBranch is the checked-out branch, "HEAD" for a detached HEAD.
	CurrentBranch() (string, error)
	// CountCommitsBehind counts the commits ref has that HEAD lacks.
	CountCommitsBehind(ref string) (int, error)
	// CommonDir is the git directory the checkout shares with its worktrees.
	CommonDir() (string, error)
}

// crewStaleFS is the filesystem surface the walk reads, so a test describes a
// town in a table instead of building one on disk. It has no write method: the
// command cannot change a clone even by accident.
type crewStaleFS interface {
	// ReadDir lists the entries of an existing directory.
	ReadDir(name string) ([]fs.DirEntry, error)
	// Stat is a file's information, whose ModTime is when it was last written.
	Stat(name string) (fs.FileInfo, error)
}

// crewStaleDisk is crewStaleFS on the real filesystem.
type crewStaleDisk struct{}

func (crewStaleDisk) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(name) }

func (crewStaleDisk) Stat(name string) (fs.FileInfo, error) { return os.Stat(name) }

// crewStaleDeps are the seams gt crew stale runs through, so the town, the
// clock, the filesystem and git are all a test's own.
type crewStaleDeps struct {
	findTown func() (string, error)
	now      func() time.Time
	fs       crewStaleFS
	repos    func(root string) crewStaleGit
}

// crewStaleRealDeps is the production wiring, kept in one place so a test that
// means to exercise the real filesystem and git asks for it here instead of
// rebuilding it.
func crewStaleRealDeps() crewStaleDeps {
	return crewStaleDeps{
		findTown: workspace.FindFromCwdOrError,
		now:      time.Now,
		fs:       crewStaleDisk{},
		repos:    func(root string) crewStaleGit { return git.NewGit(root) },
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
	readings, err := crewStaleReadings(townRoot, deps.fs, deps.repos, now)
	if err != nil {
		return err
	}

	clones := make([]crewfresh.Clone, len(readings))
	byPath := make(map[string]crewStaleReading, len(readings))
	for i, r := range readings {
		clones[i] = r.clone
		byPath[r.clone.Path] = r
	}

	for _, stale := range crewfresh.Stale(clones, now, crewStaleMaxAge) {
		fmt.Fprintln(out, crewStaleLine(byPath[stale.Path], now))
	}
	return nil
}

// crewStaleReading is one checkout as the command read it: the clone the
// report lists, and which of its fields the reads returned. A field that was
// not read shows as "?"; the clone's zero value would pass as a clone that is
// on main, fetched now and up to date.
type crewStaleReading struct {
	clone    crewfresh.Clone
	branchOK bool
	fetchOK  bool
	behindOK bool
}

// crewStaleReadings reads every checkout under <town>/<rig>/crew/. That path
// is the only one read: mayor/rig and polecat worktrees are not crew clones,
// and they keep their state under mayor/ and polecats/ instead.
//
// A rig without a crew directory is skipped, since most rigs have none. A crew
// directory that exists but cannot be listed is an error: a permission
// failure there would otherwise empty part of the report.
func crewStaleReadings(townRoot string, fsys crewStaleFS, repos func(root string) crewStaleGit, now time.Time) ([]crewStaleReading, error) {
	rigs, err := fsys.ReadDir(townRoot)
	if err != nil {
		return nil, fmt.Errorf("read the town root %s: %w", townRoot, err)
	}

	var readings []crewStaleReading
	for _, rig := range rigs {
		if !rig.IsDir() {
			continue
		}
		crewDir := filepath.Join(townRoot, rig.Name(), "crew")
		dirs, err := fsys.ReadDir(crewDir)
		if errors.Is(err, fs.ErrNotExist) {
			// A rig with no crew directory has no clones to report.
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read the crew directory %s: %w", crewDir, err)
		}
		for _, dir := range dirs {
			if !dir.IsDir() {
				continue
			}
			root := filepath.Join(crewDir, dir.Name())
			readings = append(readings, crewStaleReadingOf(root, rig.Name(), dir.Name(), fsys, now, repos(root)))
		}
	}
	return readings, nil
}

// crewStaleReadingOf reads one checkout: its branch, when it last fetched, and
// how far behind origin/main it is. A failed read names the question, never
// git's own output, which can quote a remote URL carrying a credential.
func crewStaleReadingOf(root, rig, dir string, fsys crewStaleFS, now time.Time, g crewStaleGit) crewStaleReading {
	r := crewStaleReading{clone: crewfresh.Clone{Path: root, Rig: rig, Dir: dir}}
	var failed []string

	branch, err := g.CurrentBranch()
	switch {
	case err != nil:
		failed = append(failed, "the branch could not be read")
	case branch == "HEAD":
		// rev-parse --abbrev-ref prints HEAD on a detached HEAD, which the
		// report shows as detached rather than as a branch named HEAD.
		r.branchOK = true
	default:
		r.clone.Branch = branch
		r.branchOK = true
	}

	fetched, err := crewStaleFetchHead(g, fsys)
	switch {
	case err != nil:
		failed = append(failed, "FETCH_HEAD could not be read")
	case fetched.After(now):
		// A fetch time after this run is a clock that disagreed with this one
		// or a copied timestamp. Its age would print negative, and Stale would
		// read the clone as freshly fetched and leave it out of the report, so
		// the time is not trusted: the clone is listed with the reason.
		failed = append(failed, "FETCH_HEAD is dated in the future")
	default:
		r.clone.LastFetch = fetched
		r.fetchOK = true
	}

	behind, err := g.CountCommitsBehind(crewStaleOriginMain)
	if err != nil {
		failed = append(failed, "the commits behind origin/main could not be counted")
	} else {
		r.clone.Behind = behind
		r.behindOK = true
	}

	r.clone.Err = strings.Join(failed, "; ")
	return r
}

// crewStaleFetchHead is when the checkout last fetched, read from the
// modification time git gives FETCH_HEAD.
//
// The file is looked up in the checkout's common directory, not its own .git:
// a linked worktree keeps no FETCH_HEAD of its own, and the fetch it shares
// with its clone is the one that says when its tree was last brought down. A
// checkout with no FETCH_HEAD has never fetched, which is the zero time
// crewfresh.Stale counts as stale.
func crewStaleFetchHead(g crewStaleGit, fsys crewStaleFS) (time.Time, error) {
	dir, err := g.CommonDir()
	if err != nil {
		return time.Time{}, err
	}
	info, err := fsys.Stat(filepath.Join(dir, "FETCH_HEAD"))
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime(), nil
}

// crewStaleLine is one clone's report line: rig/dir, branch, hours since its
// last fetch, and the commits it is behind origin/main.
func crewStaleLine(r crewStaleReading, now time.Time) string {
	c := r.clone

	branch := "?"
	if r.branchOK {
		branch = c.Branch
		if branch == "" {
			branch = "detached"
		}
	}

	age := "?"
	if r.fetchOK {
		age = "never"
		if !c.LastFetch.IsZero() {
			age = strconv.FormatInt(int64(now.Sub(c.LastFetch).Hours()), 10) + "h"
		}
	}

	behind := "?"
	if r.behindOK {
		behind = strconv.Itoa(c.Behind)
	}

	line := fmt.Sprintf("%s/%s  %s  %s  %s", c.Rig, c.Dir, branch, age, behind)
	if c.Err != "" {
		line += "  " + c.Err
	}
	return line
}
