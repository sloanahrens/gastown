package cmd

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/steveyegge/gastown/internal/polecat"
)

// The failure texts here are the ones the town produced (gt-hsrr45): a
// serialization abort on a mol bond, a hook write that lost the assignee lock,
// and a seat whose session was already running. The wiring is tested against
// what a sling actually returns, not a paraphrase of it.
const (
	slingSerializationText = `bd mol bond mol-polecat-work gt-x: Error 1213 (40001): serialization failure: this transaction conflicts with a committed transaction from another client, try restarting transaction.`
	slingHookLockText      = `serializing hook write for gastown/polecats/toast: timed out acquiring assignee sling lock after 10s (held by pid 10297 started 00:07:09Z; another sling may be stuck)`
	slingRunningText       = `starting polecat session: session already running: gt-cheedo`
)

// TestSlingGiveBackWireGivesBackEveryClassifiedFailure pins gt-wsf3ht: a sling
// that fails through the engine, on any failure the classifier names, leaves
// the bead's respawn count as it was before the spawn. The dispatcher defers
// such a bead and slings it again, so a count per attempt would reach the
// limit on the town's own contention. Every other failure keeps its count.
func TestSlingGiveBackWireGivesBackEveryClassifiedFailure(t *testing.T) {
	t.Parallel()
	const bead = slingBead
	cases := []struct {
		name string
		err  error
		// want is the count after the failed sling: the one death it already
		// had, or that plus this attempt's charge.
		want int
	}{
		{name: "a Dolt serialization failure", err: errors.New(slingSerializationText), want: 1},
		{name: "an assignee-lock timeout", err: errors.New(slingHookLockText), want: 1},
		{name: "a session that was already running", err: errors.New(slingRunningText), want: 1},
		{name: "a bond-lock deferral", err: fmt.Errorf("bonding: %w", ErrBondLockTimeout), want: 1},
		{name: "an unrelated failure", err: errors.New("instantiating the formula: unknown var"), want: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			// One death already on the bead, so a give-back that removes the
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

// TestSlingGiveBackWireCoversTheRigTarget pins the same give-back on the run's
// own rollback, the path a hand-run gt sling takes: the classifier is asked
// there too, not only on executeSling (gt-wsf3ht).
func TestSlingGiveBackWireCoversTheRigTarget(t *testing.T) {
	t.Parallel()
	const bead = slingBead
	formulaErr := errors.New(slingSerializationText)
	townRoot := t.TempDir()
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
		return nil, formulaErr
	}

	if err := h.sling(bead, "gastown"); !errors.Is(err, formulaErr) {
		t.Fatalf("sling error = %v, want the formula failure", err)
	}
	if got := respawnCountOf(t, townRoot, bead); got != 1 {
		t.Errorf("respawn count after the failed sling = %d, want the 1 it had before the spawn", got)
	}
}

// TestSlingGiveBackWirePoolFullLeavesTheCount pins the other half of gt-wsf3ht:
// a pool at its cap refuses the sling before any polecat is spawned, so the
// attempt recorded no count and the bead's count is untouched. There is no
// spawn to give back, and none is invented.
func TestSlingGiveBackWirePoolFullLeavesTheCount(t *testing.T) {
	t.Parallel()
	const bead = "gt-poolfull"
	townRoot := t.TempDir()
	if _, err := polecat.RecordBeadRespawn(townRoot, bead); err != nil {
		t.Fatalf("RecordBeadRespawn: %v", err)
	}
	const refusal = "pool: total full (2/2 across every seat) -> no seat for claude-sonnet"
	var prepared bool
	s := slingSeatSpawn{
		backpressure: func(string, string, SlingSpawnOptions) error { return nil },
		pinnedAgent:  func(string, string) (string, error) { return "", nil },
		resolvePool: func(string, string) (string, string, error) {
			return "", refusal, &poolBackpressureError{Reason: refusal, Raise: "polecat_pool.max_total"}
		},
		prepare: func(string, string, SlingSpawnOptions, slingStores) (*SpawnedPolecatInfo, error) {
			prepared = true
			return &SpawnedPolecatInfo{HookBead: bead}, nil
		},
	}

	info, err := s.spawn(townRoot, "gastown", SlingSpawnOptions{HookBead: bead})
	if !errors.Is(err, errPoolBackpressure) {
		t.Fatalf("spawn error = %v, want the pool's refusal", err)
	}
	if info != nil {
		t.Errorf("spawn returned %+v for a refused sling, want none", info)
	}
	if prepared {
		t.Error("a refused sling prepared a polecat, which records the count it would give back")
	}
	if got := respawnCountOf(t, townRoot, bead); got != 1 {
		t.Errorf("respawn count after the pool-full refusal = %d, want the 1 it had", got)
	}
}
