package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// useFailingTownBD makes every bd call the town-keyed readers make for
// townRoot fail, as a town with no rig beads would, without starting bd.
func useFailingTownBD(t *testing.T, townRoot string) {
	t.Helper()
	townBDRunners.Store(townRoot, beads.BDRunner(func(context.Context, beads.BDCall) ([]byte, []byte, error) {
		return nil, nil, errors.New("no rig bead")
	}))
	t.Cleanup(func() { townBDRunners.Delete(townRoot) })
}

// failingTownBD returns a fresh town root whose bd calls all fail.
func failingTownBD(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	useFailingTownBD(t, townRoot)
	return townRoot
}
