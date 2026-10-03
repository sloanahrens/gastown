package cmd

import (
	"os"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/rig"
)

// TestPolecatListInventoryEnvProbe: the listing's probe fidelity is the fix for
// gt-8q0s, and it lives in one field of one struct literal. Without this the
// only thing standing between the dashboard and a per-seat ls-remote is nobody
// flipping a boolean — the probe itself is tested, but the listing's use of it
// is the part the dashboard actually feels.
func TestPolecatListInventoryEnvProbe(t *testing.T) {
	t.Parallel()
	env := polecatListInventoryEnv(
		"/town/gastown", "gastown", "zircon",
		polecatMRIndex{}, nil, polecatSpawnFacts{},
	)
	if !env.GitProbeLocalOnly {
		t.Fatalf("the listing's inventory env does not set GitProbeLocalOnly; " +
			"`gt polecat list --all` would run an ls-remote per seat again (gt-8q0s)")
	}
}

// TestResolvePolecatSeatsPreservesOutputOrder: the pool may run seats in any
// order, but the list output must not depend on which goroutine finished
// first. Rows are indexed, never appended.
func TestResolvePolecatSeatsPreservesOutputOrder(t *testing.T) {
	t.Parallel()
	// Deliberately not sorted: an implementation that appended results as they
	// completed, or that sorted them, would produce a different order.
	names := []string{"zircon", "alpha", "quartz", "basalt", "topaz", "cobalt", "flint", "marble"}
	rigs := []string{"gastown", "beads", "gastown", "beads", "gastown", "beads", "gastown", "beads"}

	seats := make([]polecatSeat, len(names))
	for i, name := range names {
		seats[i] = polecatSeat{
			rigName: rigs[i],
			name:    name,
			// No worktree and no MR source: the item build is pure, so this
			// exercises the pool's ordering without depending on git or Dolt.
			sessions: polecatSessionSet{},
			env:      polecatInventoryEnv{},
		}
	}

	items := resolvePolecatSeats(seats)
	if len(items) != len(seats) {
		t.Fatalf("resolvePolecatSeats returned %d rows for %d seats", len(items), len(seats))
	}
	for i := range seats {
		if items[i].Name != names[i] || items[i].Rig != rigs[i] {
			t.Fatalf("row %d = %s/%s, want %s/%s — output order must not depend on completion order",
				i, items[i].Rig, items[i].Name, rigs[i], names[i])
		}
	}
}

// TestResolvePolecatSeatsPassesDecidedRowsThrough: orphan sessions are
// classified before the pool and take their place in the output unchanged.
// They carry no probe inputs, so a pool that tried to rebuild them would lose
// the foreign/zombie distinction.
func TestResolvePolecatSeatsPassesDecidedRowsThrough(t *testing.T) {
	t.Parallel()
	foreign := PolecatListItem{
		Rig: "gastown", Name: "stray", State: "foreign", Foreign: true, SessionRunning: true,
	}
	seat := polecatSeat{decided: &foreign}

	items := resolvePolecatSeats([]polecatSeat{seat})
	if len(items) != 1 || items[0].State != "foreign" || !items[0].Foreign {
		t.Fatalf("decided row was not passed through: %+v", items)
	}
}

// TestBuildAllRigSeatsPreservesOutputOrder: the rig-level pool may fetch rigs
// in any order, but `gt polecat list --all` output must not depend on which
// rig's Dolt round trips finish first. Rows are indexed by rig slot, never
// appended. This is the rig-level analogue of
// TestResolvePolecatSeatsPreservesOutputOrder, and it exists because the pool
// is a genuinely new concurrent path in a command whose output order
// downstream code depends on (gt-92zx).
func TestBuildAllRigSeatsPreservesOutputOrder(t *testing.T) {
	t.Parallel()
	// Deliberately not sorted: an implementation that sorted the rows, or
	// appended them as the concurrent builds landed, would produce a
	// different list.
	names := []string{"gastown", "beads", "quartz", "basalt", "topaz", "cobalt", "flint", "marble"}
	rigs := make([]*rig.Rig, len(names))
	for i, name := range names {
		rigs[i] = &rig.Rig{Name: name}
	}

	rigSeats := buildAllRigSeats(rigs, func(r *rig.Rig) []polecatSeat {
		return []polecatSeat{{rigName: r.Name, name: "seat-" + r.Name}}
	})

	if len(rigSeats) != len(rigs) {
		t.Fatalf("buildAllRigSeats returned %d rig slots for %d rigs", len(rigSeats), len(rigs))
	}
	for i, seats := range rigSeats {
		if len(seats) != 1 || seats[0].rigName != names[i] || seats[0].name != "seat-"+names[i] {
			t.Fatalf("rig slot %d = %+v, want one seat for %s — output order must not depend on completion order",
				i, seats, names[i])
		}
	}
}

// TestBuildAllRigSeatsRunsEveryRigExactlyOnce: the shared cursor hands out
// slots one at a time, so the pool must neither skip a rig (a slot left nil
// silently truncates the listing) nor build one twice.
func TestBuildAllRigSeatsRunsEveryRigExactlyOnce(t *testing.T) {
	t.Parallel()
	names := []string{"gastown", "beads", "quartz", "basalt", "topaz", "cobalt", "flint", "marble"}
	rigs := make([]*rig.Rig, len(names))
	for i, name := range names {
		rigs[i] = &rig.Rig{Name: name}
	}

	var mu sync.Mutex
	calls := make(map[string]int, len(names))
	buildAllRigSeats(rigs, func(r *rig.Rig) []polecatSeat {
		mu.Lock()
		defer mu.Unlock()
		calls[r.Name]++
		return []polecatSeat{{rigName: r.Name, name: "seat-" + r.Name}}
	})

	for _, name := range names {
		if calls[name] != 1 {
			t.Fatalf("build called %d times for rig %s, want exactly 1 (calls: %v)", calls[name], name, calls)
		}
	}
}

// TestBuildAllRigSeatsOverlapsRigsUpToThePoolSize is the regression test for the
// pool itself (gt-92zx). The order and exactly-once tests above pass just as
// well for a serial loop, and a serial loop is the very thing the pool
// replaced: `--all` paid every rig's Dolt round trips one after another and
// timed out on a many-rig town. So this asserts the mechanism, not a duration
// (a timing bound flakes on a loaded host): every build blocks at a
// rendezvous until as many builds as the pool has workers are in flight
// together. A serial implementation never gets there and hangs until go
// test's -timeout names this test; an unbounded one is caught by the
// peak-in-flight check.
func TestBuildAllRigSeatsOverlapsRigsUpToThePoolSize(t *testing.T) {
	t.Parallel()
	workers := polecatSeatPoolSize()
	rigs := make([]*rig.Rig, workers*3)
	for i := range rigs {
		rigs[i] = &rig.Rig{Name: "rig"}
	}

	var (
		mu       sync.Mutex
		inFlight int
		peak     int
		arrived  int
	)
	allArrived := make(chan struct{})

	buildAllRigSeats(rigs, func(r *rig.Rig) []polecatSeat {
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		arrived++
		if arrived == workers {
			close(allArrived)
		}
		mu.Unlock()

		<-allArrived

		mu.Lock()
		inFlight--
		mu.Unlock()
		return nil
	})

	if peak != workers {
		t.Fatalf("peak concurrent rig builds = %d, want exactly the pool size %d (bounded, and fully used)", peak, workers)
	}
}

// TestBuildAllRigSeatsSingleRigRunsInline: one rig has nothing to overlap with,
// so the pool must hand back its one slot without needing a second worker, and
// no rigs must build nothing.
func TestBuildAllRigSeatsSingleRigRunsInline(t *testing.T) {
	t.Parallel()
	rigSeats := buildAllRigSeats([]*rig.Rig{{Name: "gastown"}}, func(r *rig.Rig) []polecatSeat {
		return []polecatSeat{{rigName: r.Name, name: "seat"}}
	})
	if len(rigSeats) != 1 || len(rigSeats[0]) != 1 || rigSeats[0][0].rigName != "gastown" {
		t.Fatalf("single-rig result = %+v, want one slot holding gastown's seat", rigSeats)
	}
	got := buildAllRigSeats(nil, func(*rig.Rig) []polecatSeat {
		t.Error("build called with no rigs")
		return nil
	})
	if len(got) != 0 {
		t.Fatalf("no rigs produced %d slots, want 0", len(got))
	}
}

// TestPolecatSeatPoolSizeIsBounded: the pool exists to keep a large town from
// forking hundreds of git processes at once, so the bound has to hold on every
// host. Too small and a 47-seat listing serializes again; unbounded and the
// fan-out defeats the purpose. The ceiling is four because wider pools only
// bought kernel time (see polecatSeatPoolSize).
func TestPolecatSeatPoolSizeIsBounded(t *testing.T) {
	t.Parallel()
	size := polecatSeatPoolSize()
	if size < 2 {
		t.Fatalf("polecatSeatPoolSize() = %d, want at least 2 so a many-seat listing overlaps its probes", size)
	}
	if size > 4 {
		t.Fatalf("polecatSeatPoolSize() = %d, want at most 4: wider pools measured no faster and cost twice the system CPU", size)
	}
}

// TestBuildPolecatSeatItemCarriesTheActiveWorkFailure: the rig-wide active-work
// query is one call per rig but its failure is per seat, and a seat whose work
// could not be read must not be reported as idle. This is the wiring the pool
// has to keep intact when it moves the build off the serial loop.
func TestBuildPolecatSeatItemCarriesTheActiveWorkFailure(t *testing.T) {
	t.Parallel()
	item := buildPolecatSeatItem(
		"gastown", "zircon", nil, nil,
		os.ErrDeadlineExceeded,
		polecatSessionSet{},
		polecatInventoryEnv{},
	)
	if item.Name != "zircon" || item.Rig != "gastown" {
		t.Fatalf("item identity = %s/%s, want gastown/zircon", item.Rig, item.Name)
	}
	if item.Blockers == nil {
		t.Fatalf("a seat whose active work could not be read reported no blockers: %+v", item)
	}
}
