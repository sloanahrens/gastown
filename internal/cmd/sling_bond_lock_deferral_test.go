package cmd

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/steveyegge/gastown/internal/polecat"
)

// TestSlingBondLockTimeoutGivesBackTheRespawn guards gt-8d38p: a sling that
// fails to bond because the town bond lock did not free is deferred by the
// spec dispatcher, not failed, so the count its spawn charged must go back.
// Otherwise a town queueing bonds past the bound spends a bead's respawn
// budget on its own load, and the sling after the limit is refused as a failed
// dispatch. Every other sling failure keeps its count.
//
// The sling under test is run's own rollback, with the spawn carrying what a
// fresh allocation hands it (the bead and the mark recordRespawn took) and the
// bond failing where the sling bonds.
func TestSlingBondLockTimeoutGivesBackTheRespawn(t *testing.T) {
	t.Parallel()
	const bead = slingBead
	bondLock := fmt.Errorf("instantiating the formula: %w",
		fmt.Errorf("bonding: spawning and attaching proto: %w", ErrBondLockTimeout))
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{name: "a bond-lock deferral", err: bondLock, want: 1},
		{name: "another formula failure", err: errors.New("instantiating the formula: unknown var"), want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			// One count already on the bead, so a give-back that removes the
			// wrong increment reads as a count of 0.
			if _, err := polecat.RecordBeadRespawn(townRoot, bead); err != nil {
				t.Fatalf("RecordBeadRespawn: %v", err)
			}
			mark, err := polecat.RecordBeadRespawnMark(townRoot, bead)
			if err != nil {
				t.Fatalf("RecordBeadRespawnMark: %v", err)
			}

			h := newSlingHarness(t)
			h.addBead(bead, beadInfo{})
			h.formulas["mol-polecat-work"] = true
			// The spawn carries what a fresh allocation hands the sling: the
			// bead it will hook, its town, and the count it charged.
			h.run.spawnPolecat = func(rig string, _ SlingSpawnOptions) (*SpawnedPolecatInfo, error) {
				s := h.newSpawn(rig)
				s.HookBead = bead
				s.townRoot = townRoot
				s.respawnMark = mark
				return s, nil
			}
			h.run.instantiateFormula = func(context.Context, string, string, string, string, string, []string) (*FormulaOnBeadResult, error) {
				return nil, tc.err
			}

			if err := h.sling(bead, "gastown"); !errors.Is(err, tc.err) {
				t.Fatalf("sling error = %v, want the formula failure", err)
			}
			if got := respawnCountOf(t, townRoot, bead); got != tc.want {
				t.Errorf("respawn count after the failed sling = %d, want %d", got, tc.want)
			}
		})
	}
}
