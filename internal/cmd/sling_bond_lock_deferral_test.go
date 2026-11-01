package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
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

// TestExecuteSlingBondLockTimeoutGivesBackTheRespawn covers the callers that
// never pass through runRigTarget: the spec dispatcher (slingSpec) and the
// scheduler's capacity dispatch both call executeSling directly with
// FormulaFailFatal. The give-back lives on that shared method, so their spawn
// is charged and given back the same way gt sling's rig target is (gt-8d38p).
// Every other formula failure keeps its count.
func TestExecuteSlingBondLockTimeoutGivesBackTheRespawn(t *testing.T) {
	t.Parallel()
	const bead = slingBead
	bondLock := fmt.Errorf("instantiating formula mol-polecat-work: %w",
		fmt.Errorf("bonding: spawning and attaching proto: %w", ErrBondLockTimeout))
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{name: "a bond-lock deferral", err: bondLock, want: 1},
		{name: "another formula failure", err: errors.New("instantiating formula mol-polecat-work: unknown var"), want: 2},
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

			// The params the spec dispatcher and the capacity dispatch make: a
			// declared formula and a fatal formula failure.
			params := executeParams()
			params.FormulaName = "mol-polecat-work"
			if _, err := h.run.executeSling(params); !errors.Is(err, tc.err) {
				t.Fatalf("executeSling error = %v, want the formula failure", err)
			}
			if got := respawnCountOf(t, townRoot, bead); got != tc.want {
				t.Errorf("respawn count after the failed sling = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestBondLockGiveBackResolvesTheTown pins gt-ymw7d: a spawn that recorded no
// town — a reused seat, a forced sling — resolves it through the lookup the
// production path passes, and a lookup that fails leaves the count spent with
// a warning rather than passing as a give-back that had nothing to do.
func TestBondLockGiveBackResolvesTheTown(t *testing.T) {
	t.Parallel()
	const bead = "gt-bond-town"
	for _, tc := range []struct {
		name    string
		findErr error
		want    int
		// wantWarn is whether the failed lookup must be reported: a give-back
		// that could not be made is not a give-back that had nothing to do.
		wantWarn bool
	}{
		{name: "the town is found", want: 1},
		{name: "the town cannot be found", findErr: errors.New("no town"), want: 2, wantWarn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			// One death already on the bead: a give-back removes the spawn's
			// count and leaves that one, so a no-op reads as 2.
			if _, err := polecat.RecordBeadRespawn(townRoot, bead); err != nil {
				t.Fatalf("RecordBeadRespawn: %v", err)
			}
			mark, err := polecat.RecordBeadRespawnMark(townRoot, bead)
			if err != nil {
				t.Fatalf("RecordBeadRespawnMark: %v", err)
			}

			findTown := func() (string, error) {
				if tc.findErr != nil {
					return "", tc.findErr
				}
				return townRoot, nil
			}
			s := &SpawnedPolecatInfo{HookBead: bead, respawnMark: mark}
			var warn bytes.Buffer

			s.giveBackBondLockRespawnWith(findTown, &warn, fmt.Errorf("bonding: %w", ErrBondLockTimeout))

			if got := respawnCountOf(t, townRoot, bead); got != tc.want {
				t.Errorf("respawn count after the give-back = %d, want %d", got, tc.want)
			}
			if gotWarn := warn.Len() > 0; gotWarn != tc.wantWarn {
				t.Errorf("warning = %q, want printed %v", warn.String(), tc.wantWarn)
			} else if tc.wantWarn {
				for _, want := range []string{bead, "find the town root", tc.findErr.Error()} {
					if !strings.Contains(warn.String(), want) {
						t.Errorf("warning = %q, want it to name %q", warn.String(), want)
					}
				}
			}
		})
	}
}
