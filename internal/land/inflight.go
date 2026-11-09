package land

import (
	"errors"
	"io/fs"
	"os"
	"strings"
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

// WorkInFlight counts the landing, post-landing and revert directories under a
// rig's work root. Each run makes one directory and removes it when it ends, so
// a directory present is a run in flight. A root that does not exist holds no
// run, and any other read failure is an error, never a zero count.
func WorkInFlight(workRoot string) (int, error) {
	entries, err := os.ReadDir(workRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() {
			continue
		}
		if strings.HasPrefix(name, landDirPrefix) || strings.HasPrefix(name, postDirPrefix) || strings.HasPrefix(name, revertDirPrefix) {
			n++
		}
	}
	return n, nil
}
