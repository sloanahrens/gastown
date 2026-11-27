package rehearsal

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// checkRunInputs refuses a run that cannot work, before anything is filed, and
// returns the bead set it files. It checks in order that the rig directory and
// the three worktrees can be read, that today's host load file can be read, and
// that the set loads. A relative worktree or set is joined to root. Every error
// names the path that failed. It creates and changes nothing.
func checkRunInputs(a RunArgs, root string, now func() time.Time) ([]Task, error) {
	dirs := []string{filepath.Join(root, a.Rig)}
	for _, w := range a.Worktrees {
		dirs = append(dirs, runInputPath(root, w))
	}
	for _, dir := range dirs {
		if _, err := os.ReadDir(dir); err != nil {
			return nil, fmt.Errorf("run input directory %s: %w", dir, err)
		}
	}
	load := filepath.Join(root, ".runtime", "host-load", now().Format("2006-01-02")+".log")
	if _, err := os.ReadFile(load); err != nil {
		return nil, fmt.Errorf("run input host load file %s: %w", load, err)
	}
	set := runInputPath(root, a.Set)
	tasks, err := LoadSet(set)
	if err != nil {
		return nil, fmt.Errorf("run input set %s: %w", set, err)
	}
	return tasks, nil
}

// runInputPath returns p, joined to root unless it is absolute.
func runInputPath(root, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(root, p)
}
