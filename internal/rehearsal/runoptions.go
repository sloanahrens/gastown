package rehearsal

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// NewRunOptions turns a parsed run command line into the Options Drive takes,
// refusing a run that cannot work. root is the absolute town root the command runs
// from. It checks the inputs first and makes the run's cache root last, so a
// refused run creates nothing.
func NewRunOptions(a RunArgs, root string, now func() time.Time) (Options, error) {
	tasks, err := checkRunInputs(a, root, now)
	if err != nil {
		return Options{}, err
	}
	cacheRoot := runInputPath(root, a.Record+".gocache")
	if err := os.Mkdir(cacheRoot, 0o700); err != nil {
		return Options{}, fmt.Errorf("run cache root %s: %w", cacheRoot, err)
	}
	start := now()
	return Options{
		Town: Town{Rig: a.Rig, Run: ExecRun}, Agent: a.Agent, CacheRoot: cacheRoot, SideRun: ExecSideRun,
		Worktrees: []string{runInputPath(root, a.Worktrees[0]), runInputPath(root, a.Worktrees[1]), runInputPath(root, a.Worktrees[2])},
		Now:       now, Tick: time.NewTicker(30 * time.Second).C, Deadline: time.After, Tag: a.Tag, ChangeSet: a.ChangeSet,
		RunID: "rehearsal-" + start.UTC().Format("20060102T150405Z"), Tasks: tasks, Path: runInputPath(root, a.Record),
		CacheBytes: a.CacheBytes, Interventions: a.Interventions, HostLoad: HostLoadReader(filepath.Join(root, ".runtime", "host-load"), start, now),
	}, nil
}
