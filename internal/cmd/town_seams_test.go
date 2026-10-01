package cmd

import "testing"

// useFailingTownBD makes every bd read the town-keyed readers make for
// townRoot fail, as a town with no rig beads would, without starting bd.
func useFailingTownBD(t *testing.T, townRoot string) {
	t.Helper()
	townBDDown.Store(townRoot, struct{}{})
	t.Cleanup(func() { townBDDown.Delete(townRoot) })
}

// failingTownBD returns a fresh town root whose bd reads all fail.
func failingTownBD(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	useFailingTownBD(t, townRoot)
	return townRoot
}
