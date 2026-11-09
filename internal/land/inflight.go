package land

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"time"
)

// landDirPrefix names the directory a landing makes under its work root
// (addWorktree). Its removal at the landing's end is what ends the run.
const landDirPrefix = "land-"

// postDirPrefix and revertDirPrefix name the directories a post-landing run and
// a revert run make under the same work root (daemon's postLandRun and
// postLandRevert). They are listed here so one reader answers for every run
// that holds the root.
const (
	postDirPrefix   = "post-"
	revertDirPrefix = "revert-"
)

// WorkRun is one run directory under a rig's work root: its name and when it was
// last changed. A run removes its directory when it ends, so a directory that is
// still there, and how long ago it changed, tells a live run from a stale one.
type WorkRun struct {
	Name    string
	ModTime time.Time
}

// WorkInFlight counts the run directories under a rig's work root (WorkRuns).
// Each run makes one directory and removes it when it ends, so a directory
// present is a run in flight. A root that does not exist holds no run, and any
// other read failure is an error, never a zero count.
func WorkInFlight(workRoot string) (int, error) {
	runs, err := WorkRuns(workRoot)
	if err != nil {
		return 0, err
	}
	return len(runs), nil
}

// WorkRuns lists the landing, post-landing and revert directories under a rig's
// work root, each with its modification time. A root that does not exist holds
// no run; a read failure, or a directory whose time cannot be read, is an error.
func WorkRuns(workRoot string) ([]WorkRun, error) {
	entries, err := os.ReadDir(workRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var runs []WorkRun
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !isRunDir(name) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		runs = append(runs, WorkRun{Name: name, ModTime: info.ModTime()})
	}
	return runs, nil
}

// isRunDir reports whether a directory name is one a landing, post-landing or
// revert run makes under the work root.
func isRunDir(name string) bool {
	return strings.HasPrefix(name, landDirPrefix) || strings.HasPrefix(name, postDirPrefix) || strings.HasPrefix(name, revertDirPrefix)
}
