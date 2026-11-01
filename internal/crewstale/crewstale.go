// Package crewstale reads the crew clones under a town: one checkout per crew
// member in each rig's crew directory, when each last fetched and how far each
// is behind origin/main. Nothing here fetches or moves a clone.
//
// gt crew stale and the overseer report both read through it, so they agree on
// what a crew clone is and when it was last fetched. The package imports
// neither internal/cmd nor internal/daemon, which is what lets both call it.
package crewstale

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/crewfresh"
)

// OriginMain is the ref every crew clone tracks, so it is the one a clone's
// commits-behind count is taken against.
const OriginMain = "origin/main"

// Git is the read-only git surface one checkout is asked about. Every method
// reads, so a checkout the walk examines is left exactly as it found it.
type Git interface {
	// CurrentBranch is the checked-out branch, "HEAD" for a detached HEAD.
	CurrentBranch() (string, error)
	// CountCommitsBehind counts the commits ref has that HEAD lacks.
	CountCommitsBehind(ref string) (int, error)
	// CommonDir is the git directory the checkout shares with its worktrees.
	CommonDir() (string, error)
}

// FS is the filesystem surface the walk reads, so a test describes a town in a
// table instead of building one on disk. It has no write method: the walk
// cannot change a clone even by accident.
type FS interface {
	// ReadDir lists the entries of an existing directory.
	ReadDir(name string) ([]fs.DirEntry, error)
	// Stat is a file's information, whose ModTime is when it was last written.
	Stat(name string) (fs.FileInfo, error)
}

// Disk is FS on the real filesystem.
type Disk struct{}

func (Disk) ReadDir(name string) ([]fs.DirEntry, error) { return os.ReadDir(name) }

func (Disk) Stat(name string) (fs.FileInfo, error) { return os.Stat(name) }

// Reading is one checkout as the walk read it: the clone it reports, and which
// of its fields the reads returned. A field that was not read is false, since
// the clone's zero value would pass as a clone that is on main, fetched now and
// up to date.
type Reading struct {
	Clone    crewfresh.Clone
	BranchOK bool
	FetchOK  bool
	BehindOK bool
}

// Readings reads every checkout under <town>/<rig>/crew/. That path is the only
// one read: mayor/rig and polecat worktrees are not crew clones, and they keep
// their state under mayor/ and polecats/ instead.
//
// A rig without a crew directory is skipped, since most rigs have none. A crew
// directory that exists but cannot be listed is an error: a permission failure
// there would otherwise empty part of the report.
func Readings(townRoot string, fsys FS, repos func(root string) Git, now time.Time) ([]Reading, error) {
	rigs, err := fsys.ReadDir(townRoot)
	if err != nil {
		return nil, fmt.Errorf("read the town root %s: %w", townRoot, err)
	}

	var readings []Reading
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
			readings = append(readings, readingOf(root, rig.Name(), dir.Name(), fsys, now, repos(root)))
		}
	}
	return readings, nil
}

// Clones is Readings as the clones a report judges: each checkout's clone, with
// the reason in Err where one of its reads failed. A clone that could not be
// read is still returned, since it cannot be shown to be fresh.
func Clones(townRoot string, fsys FS, repos func(root string) Git, now time.Time) ([]crewfresh.Clone, error) {
	readings, err := Readings(townRoot, fsys, repos, now)
	if err != nil {
		return nil, err
	}
	clones := make([]crewfresh.Clone, len(readings))
	for i, r := range readings {
		clones[i] = r.Clone
	}
	return clones, nil
}

// readingOf reads one checkout: its branch, when it last fetched, and how far
// behind origin/main it is. A failed read names the question, never git's own
// output, which can quote a remote URL carrying a credential.
func readingOf(root, rig, dir string, fsys FS, now time.Time, g Git) Reading {
	r := Reading{Clone: crewfresh.Clone{Path: root, Rig: rig, Dir: dir}}
	var failed []string

	branch, err := g.CurrentBranch()
	switch {
	case err != nil:
		failed = append(failed, "the branch could not be read")
	case branch == "HEAD":
		// rev-parse --abbrev-ref prints HEAD on a detached HEAD, which a report
		// shows as detached rather than as a branch named HEAD.
		r.BranchOK = true
	default:
		r.Clone.Branch = branch
		r.BranchOK = true
	}

	fetched, err := fetchHead(g, fsys)
	switch {
	case err != nil:
		failed = append(failed, "FETCH_HEAD could not be read")
	case fetched.After(now):
		// A fetch time after this run is a clock that disagreed with this one or
		// a copied timestamp. Its age would print negative, and Stale would read
		// the clone as freshly fetched and leave it out of the report, so the
		// time is not trusted: the clone is listed with the reason.
		failed = append(failed, "FETCH_HEAD is dated in the future")
	default:
		r.Clone.LastFetch = fetched
		r.FetchOK = true
	}

	behind, err := g.CountCommitsBehind(OriginMain)
	if err != nil {
		failed = append(failed, "the commits behind origin/main could not be counted")
	} else {
		r.Clone.Behind = behind
		r.BehindOK = true
	}

	r.Clone.Err = strings.Join(failed, "; ")
	return r
}

// fetchHead is when the checkout last fetched, read from the modification time
// git gives FETCH_HEAD.
//
// The file is looked up in the checkout's common directory, not its own .git: a
// linked worktree keeps no FETCH_HEAD of its own, and the fetch it shares with
// its clone is the one that says when its tree was last brought down. A
// checkout with no FETCH_HEAD has never fetched, which is the zero time
// crewfresh.Stale counts as stale.
func fetchHead(g Git, fsys FS) (time.Time, error) {
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
