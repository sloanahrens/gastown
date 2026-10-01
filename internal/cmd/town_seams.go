package cmd

import (
	"sync"

	"github.com/steveyegge/gastown/internal/beads"
)

// townBDRunners maps a town root to the bd runner its rig-config reads use
// instead of the bd on PATH (same per-town-root keying as townSessionListers).
var townBDRunners sync.Map // townRoot -> beads.BDRunner

func townBDRunner(townRoot string) beads.BDRunner {
	if run, ok := townBDRunners.Load(townRoot); ok {
		return run.(beads.BDRunner)
	}
	return nil
}

// townBeadsNew is beads.New(dir) answered by townRoot's registered bd runner,
// when a test registered one.
func townBeadsNew(townRoot, dir string) *beads.Beads {
	if run := townBDRunner(townRoot); run != nil {
		return beads.NewWithBeadsDirAndRunner(dir, beads.ResolveBeadsDir(dir), run)
	}
	return beads.New(dir)
}
