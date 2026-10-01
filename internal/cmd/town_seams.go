package cmd

import (
	"sync"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/rig"
)

// townBDDown marks town roots whose bd reads answer "unavailable" instead of
// starting bd: tests register a t.TempDir() town root, so parallel tests never
// share an entry. Production never registers one.
var townBDDown sync.Map // townRoot -> struct{}

func isTownBDDown(townRoot string) bool {
	_, ok := townBDDown.Load(townRoot)
	return ok
}

// townBeadsNew is beads.New(dir) for the town's bd reads: a wrapper whose
// every call fails when a test marked townRoot's bd down.
func townBeadsNew(townRoot, dir string) *beads.Beads {
	if isTownBDDown(townRoot) {
		return beads.NewWithBeadsDirAndRunner(dir, beads.ResolveBeadsDir(dir), beads.UnavailableBD)
	}
	return beads.New(dir)
}

// townRigBD points r's identity-bead reads at an unavailable bd when a test
// marked townRoot's bd down; otherwise r keeps the bd on PATH.
func townRigBD(townRoot string, r *rig.Rig) *rig.Rig {
	if isTownBDDown(townRoot) {
		downRigIdentityBeads(r)
	}
	return r
}

// downRigIdentityBeads gives r an identity-bead reader whose every call fails.
func downRigIdentityBeads(r *rig.Rig) {
	r.IdentityBeads = beads.NewWithBeadsDirAndRunner(r.Path, beads.ResolveBeadsDir(r.Path), beads.UnavailableBD)
}

// townBeadsWithDir is beads.NewWithBeadsDir(workDir, beadsDir) for the town's
// bd reads, failing every call when a test marked townRoot's bd down.
func townBeadsWithDir(townRoot, workDir, beadsDir string) *beads.Beads {
	if isTownBDDown(townRoot) {
		return beads.NewWithBeadsDirAndRunner(workDir, beadsDir, beads.UnavailableBD)
	}
	return beads.NewWithBeadsDir(workDir, beadsDir)
}
