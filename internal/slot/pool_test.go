package slot

import (
	"errors"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/lock"
)

// shortWait is a timed-acquire budget for tests that expect a timeout: under
// one poll interval, so the call gives up after its second blocked pass.
const shortWait = 200 * time.Millisecond

// TestPool_ThreeSlotsHoldConcurrently: a 3-slot pool hands out exactly three
// slots (indices 0,1,2 in order), refuses a fourth, reports all held, and
// hands a released slot straight back out.
func TestPool_ThreeSlotsHoldConcurrently(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := Pool{Slots: 3}

	var held []*Handle
	for i := 0; i < 3; i++ {
		h := tg.mustAcquirePool(t, town, "gastown/polecat-"+strconv.Itoa(i), pool)
		if h.Index != i {
			t.Fatalf("acquire #%d got slot %d, want %d", i, h.Index, i)
		}
		held = append(held, h)
	}

	if _, err, _ := tg.run(t, func() (*Handle, error) { return tg.AcquirePool(town, "gastown/polecat-3", shortWait, pool) }); err == nil {
		t.Fatal("fourth AcquirePool succeeded on a fully held 3-slot pool")
	}

	rep, err := tg.StatusPool(town, pool)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total != 3 || rep.HeldCount != 3 || !rep.AllHeld() || !rep.Busy() {
		t.Fatalf("status after 3 acquires: %+v", rep)
	}
	if !rep.Held || rep.Owner == nil || rep.Owner.Slot != 0 || rep.Owner.Role != "gastown/polecat-0" {
		t.Fatalf("Report.Held/Owner should describe the lowest held slot: %+v", rep.Owner)
	}

	if err := held[1].Release(); err != nil {
		t.Fatal(err)
	}
	rep, _ = tg.StatusPool(town, pool)
	if rep.HeldCount != 2 || rep.Busy() || rep.Slots[1].Held {
		t.Fatalf("status after releasing slot 1: %+v", rep)
	}
	if _, err := os.Stat(SlotOwnerPath(town, 1)); !os.IsNotExist(err) {
		t.Fatalf("slot 1 owner file should be gone after Release, stat err=%v", err)
	}
	h := tg.mustAcquirePool(t, town, "gastown/polecat-4", pool)
	if h.Index != 1 {
		t.Fatalf("re-acquire after release got slot %d, want 1", h.Index)
	}
}

// TestPool_HeldSlotNamesTheWaitOnce covers gt-78b8: a caller that cannot be
// granted because another process holds every candidate slot says who holds it
// at the top of the wait, and says it once for the whole wait rather than on
// every poll.
func TestPool_HeldSlotNamesTheWaitOnce(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := Pool{Slots: 1}

	held := tg.mustAcquirePool(t, town, "gastown/landing", pool)
	defer func() { _ = held.Release() }()

	// Several blocked passes, so "once" is a claim about a whole wait.
	timeout := 3 * tg.pollInterval
	if _, err, _ := tg.run(t, func() (*Handle, error) { return tg.AcquirePool(town, "gastown/amber", timeout, pool) }); err == nil {
		t.Fatal("AcquirePool was granted a slot while the only slot was held")
	} else if !strings.Contains(err.Error(), "token held by gastown/landing") {
		t.Errorf("timeout error = %q, want the holder the caller queued behind (gt-18zj)", err)
	}

	out := tg.probe
	if n := strings.Count(out.String(), "waiting for container-gate slot"); n != 1 {
		t.Errorf("wait lines = %d, want one line for the whole wait: %q", n, out.String())
	}
	if !strings.Contains(out.String(), "token held by gastown/landing") {
		t.Errorf("wait line = %q, want the holder named", out.String())
	}
	if !strings.Contains(out.String(), "cap ") {
		t.Errorf("wait line = %q, want the caller's timeout cap", out.String())
	}
}

// TestPool_ReservedSlotOnlyForGateRoles: with one slot reserved, polecats
// never take slot 0 even when it is the only free slot; gate roles do.
func TestPool_ReservedSlotOnlyForGateRoles(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := Pool{Slots: 3, ReservedForGate: 1}

	a := tg.mustAcquirePool(t, town, "gastown/amber", pool)
	b := tg.mustAcquirePool(t, town, "gastown/basalt", pool)
	if a.Index != 1 || b.Index != 2 {
		t.Fatalf("polecats got slots %d and %d, want 1 and 2 (slot 0 is reserved)", a.Index, b.Index)
	}
	if _, err, _ := tg.run(t, func() (*Handle, error) { return tg.AcquirePool(town, "gastown/coral", shortWait, pool) }); err == nil {
		t.Fatal("third polecat got a slot while only the reserved slot 0 was free")
	}

	rep, _ := tg.StatusPool(town, pool)
	if rep.Busy() {
		t.Fatalf("pool must not report busy while the reserved slot is free: %+v", rep)
	}

	r := tg.mustAcquirePool(t, town, "gastown/landing", pool)
	if r.Index != 0 {
		t.Fatalf("landing gate got slot %d, want the reserved slot 0", r.Index)
	}
	if _, err, _ := tg.run(t, func() (*Handle, error) { return tg.AcquirePool(town, "hm/landing", shortWait, pool) }); err == nil {
		t.Fatal("batch gate got a slot with every slot held")
	}

	_ = a.Release()
	c := tg.mustAcquirePool(t, town, "gastown/coral", pool)
	if c.Index != 1 {
		t.Fatalf("polecat after release got slot %d, want 1", c.Index)
	}
}

// TestPool_GateRoleFallsBackToSharedSlot: gate roles prefer reserved slots
// but may take a shared one when the reserved ones are held.
func TestPool_GateRoleFallsBackToSharedSlot(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := Pool{Slots: 2, ReservedForGate: 1}

	r := tg.mustAcquirePool(t, town, "gastown/landing", pool)
	m := tg.mustAcquirePool(t, town, "mango/landing", pool)
	if r.Index != 0 || m.Index != 1 {
		t.Fatalf("gate roles got slots %d and %d, want 0 then 1", r.Index, m.Index)
	}
}

// TestPool_UnwrappedCheckSkippedWhenAnotherSlotHeld: running gate
// containers block a grant only when nobody holds a slot (gt-tuiy). Once any
// slot is held, containers are the holder's and a second slot is granted.
func TestPool_UnwrappedCheckSkippedWhenAnotherSlotHeld(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := Pool{Slots: 2}

	h0 := tg.mustAcquirePool(t, town, "gastown/landing", pool)
	tg.rt.setLines("dolt/dolt-sql-server:2.2.0 suite")

	// Slot 0 is held, so the containers are legitimately the landing worker's:
	// slot 1 must be granted without waiting for them to clear.
	h1, err, elapsed := tg.run(t, func() (*Handle, error) { return tg.AcquirePool(town, "gastown/amber", 5*time.Second, pool) })
	if err != nil {
		t.Fatalf("AcquirePool with another slot held and containers up: %v", err)
	}
	if h1.Index != 1 {
		t.Fatalf("got slot %d, want 1", h1.Index)
	}
	if elapsed != 0 {
		t.Fatalf("grant waited %s; it should not have waited on the container check", elapsed)
	}

	// Control: with NO slot held and containers up, the same acquire is an
	// unwrapped-suite case and must time out.
	_ = h0.Release()
	_ = h1.Release()
	if _, err, _ := tg.run(t, func() (*Handle, error) { return tg.AcquirePool(town, "gastown/amber", shortWait, pool) }); err == nil {
		t.Fatal("AcquirePool succeeded with unwrapped containers up and no slot held")
	}
}

// TestStatus_DiscoversPoolSlots: a single-slot Status (an older caller or a
// config that shrank) still sees slots created by a larger pool.
func TestStatus_DiscoversPoolSlots(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := Pool{Slots: 3}

	h0 := tg.mustAcquirePool(t, town, "gastown/a", pool)
	h1 := tg.mustAcquirePool(t, town, "gastown/b", pool)
	h2 := tg.mustAcquirePool(t, town, "gastown/c", pool)
	_ = h0.Release()
	_ = h1.Release()

	rep, err := tg.Status(town)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Total != 3 || rep.HeldCount != 1 || !rep.Held || rep.Owner == nil || rep.Owner.Slot != 2 {
		t.Fatalf("Status should discover 3 slots with only slot 2 held: %+v owner=%+v", rep, rep.Owner)
	}
	if rep.Busy() {
		t.Fatalf("one of three held must not be busy: %+v", rep)
	}
	if got := rep.HeldBy("gastown/c"); len(got) != 1 || got[0].Index != 2 {
		t.Fatalf("HeldBy(gastown/c) = %+v, want slot 2", got)
	}
	_ = h2.Release()
}

// TestStatusPoolLocksOnlySkipsDockerProbe pins gt-a8kx's first half: the hot
// readers that want only the held/owner picture must not shell out to
// `docker ps`. The stub counts calls and returns a matching container, so a
// stray probe shows up twice over — as a count, and as a container in the
// report, which is what a caller reading only Held/Owner would have paid for
// without ever looking.
func TestStatusPoolLocksOnlySkipsDockerProbe(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := Pool{Slots: 2}

	// Acquiring legitimately probes docker: it decides whether a suite may
	// start. Only the reads under test are held to the flock alone.
	h := tg.mustAcquirePool(t, town, "gastown/amber", pool)

	tg.rt.setLines("dolt/dolt-sql-server:2.2.0 unwrapped-suite")
	probesBefore := tg.rt.calls()
	calls := func() int { return tg.rt.calls() - probesBefore }

	rep, err := StatusPoolLocksOnly(town, pool)
	if err != nil {
		t.Fatalf("StatusPoolLocksOnly: %v", err)
	}
	if !rep.Held || rep.HeldCount != 1 || rep.Owner == nil || rep.Owner.Role != "gastown/amber" {
		t.Fatalf("locks-only report lost the hold or its owner: %+v owner=%+v", rep, rep.Owner)
	}
	if len(rep.UnwrappedContainers) != 0 || len(rep.DebrisContainers) != 0 || rep.DockerUnknown {
		t.Fatalf("locks-only report carries container fields it never looked for: %+v", rep)
	}

	// A full report while a slot IS held skips the probe too — that half is
	// vacuous, because containers up while a slot is held belong to that
	// holder.
	held, err := tg.StatusPool(town, pool)
	if err != nil {
		t.Fatalf("StatusPool (held): %v", err)
	}
	if calls() != 0 {
		t.Fatalf("StatusPool probed docker while a slot was held: %d call(s)", calls())
	}
	if len(held.UnwrappedContainers) != 0 {
		t.Fatalf("full report with a slot held listed unwrapped containers: %+v", held.UnwrappedContainers)
	}

	_ = h.Release()
	rep, err = StatusPoolLocksOnly(town, pool)
	if err != nil {
		t.Fatalf("StatusPoolLocksOnly (released): %v", err)
	}
	if rep.Held || rep.HeldCount != 0 {
		t.Fatalf("released slot still reads held: %+v", rep)
	}
	if calls() != 0 {
		t.Fatalf("StatusPoolLocksOnly probed docker %d time(s); it must read the flocks alone", calls())
	}

	// The full report still probes — that is the half callers ask it for.
	full, err := tg.StatusPool(town, pool)
	if err != nil {
		t.Fatalf("StatusPool: %v", err)
	}
	if calls() != 1 {
		t.Fatalf("StatusPool made %d docker probes, want exactly 1", calls())
	}
	if len(full.UnwrappedContainers) != 1 {
		t.Fatalf("StatusPool UnwrappedContainers = %v, want the one container up with no slot held", full.UnwrappedContainers)
	}
}

// TestPoolBusyReadOnlyWritesNothing pins the daemon cutover's contract
// (gt-qpeei.22): the quiet gate asks whether the pool is busy and must leave
// the town exactly as it found it, so the dead holder's owner file survives the
// read. The ordinary status read is the one that sweeps it, and this test
// asserts both halves so a read-only path that starts cleaning is caught here.
func TestPoolBusyReadOnlyWritesNothing(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := Pool{Slots: 1}
	if err := os.MkdirAll(LockDir(town), 0o755); err != nil {
		t.Fatal(err)
	}
	// A lock file nobody holds, and the owner file a holder that died left
	// behind.
	if err := os.WriteFile(SlotLockPath(town, 0), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ownerPath := SlotOwnerPath(town, 0)
	if err := os.WriteFile(ownerPath, []byte(`{"role":"gastown/landing","pid":999999}`), 0o644); err != nil {
		t.Fatal(err)
	}

	busy, err := tg.PoolBusyReadOnly(town, pool)
	if err != nil {
		t.Fatalf("PoolBusyReadOnly: %v", err)
	}
	if busy {
		t.Fatalf("PoolBusyReadOnly = busy for a pool with its only slot free")
	}
	if _, err := os.Stat(ownerPath); err != nil {
		t.Fatalf("the read-only busy check removed the dead holder's owner file: %v", err)
	}

	rep, err := tg.StatusPoolLocksOnly(town, pool)
	if err != nil {
		t.Fatalf("StatusPoolLocksOnly: %v", err)
	}
	if rep.Held || rep.HeldCount != 0 {
		t.Fatalf("the free slot reads held: %+v", rep)
	}
	if _, err := os.Stat(ownerPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the ordinary status read left the dead holder's owner file in place (stat err %v); the contrast this test rests on is gone", err)
	}
}

// TestPoolBusyReadOnlyCreatesNoLockFile: a town with the gate's directory but
// no lock file in it must gain none from the read — a cutover that gave up
// would otherwise leave a lock file behind that no holder wrote.
func TestPoolBusyReadOnlyCreatesNoLockFile(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	if err := os.MkdirAll(LockDir(town), 0o755); err != nil {
		t.Fatal(err)
	}

	busy, err := tg.PoolBusyReadOnly(town, Pool{Slots: 2})
	if err != nil {
		t.Fatalf("PoolBusyReadOnly: %v", err)
	}
	if busy {
		t.Fatalf("PoolBusyReadOnly = busy for a town with no lock file and no container")
	}
	for i := 0; i < 2; i++ {
		if _, err := os.Stat(SlotLockPath(town, i)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the read-only busy check left slot %d's lock file behind (stat err %v)", i, err)
		}
	}
}

// The read-only verdict is Report.Busy's — every slot held, or an unwrapped
// suite — and the two checks that cannot be taken, the slot read and the docker
// listing, are errors rather than a silent free or a busy the gate cannot
// vouch for.
func TestPoolBusyReadOnly(t *testing.T) {
	t.Parallel()
	looking := errors.New("docker is not answering")
	cases := []struct {
		name    string
		slots   int
		held    []int
		lines   []string
		listErr error
		want    bool
		wantErr bool
	}{
		{name: "the pool is free", slots: 1},
		{name: "the only slot is held", slots: 1, held: []int{0}, want: true},
		{name: "one of two slots is held", slots: 2, held: []int{1}},
		{name: "an unwrapped suite holds the docker VM", slots: 1, lines: []string{"dolt/dolt-sql-server:2.2.0 unwrapped-suite"}, want: true},
		{name: "docker cannot be read", slots: 1, listErr: looking, wantErr: true},
		{name: "the lock cannot be opened", slots: 1, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tg := newTestGate(t)
			town := t.TempDir()
			pool := Pool{Slots: tc.slots}
			if err := os.MkdirAll(LockDir(town), 0o755); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < tc.slots; i++ {
				if err := os.WriteFile(SlotLockPath(town, i), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			for _, i := range tc.held {
				unlock, ok, err := lock.FlockTryAcquire(SlotLockPath(town, i))
				if err != nil || !ok {
					t.Fatalf("holding slot %d = (%v, %v)", i, ok, err)
				}
				t.Cleanup(unlock)
			}
			if tc.listErr != nil {
				tg.rt.failList(tc.listErr)
			} else {
				tg.rt.setLines(tc.lines...)
			}
			if tc.wantErr {
				// A directory where the lock file belongs cannot be opened for a
				// flock, which is how a slot read fails on the host.
				lockPath := SlotLockPath(town, 0)
				if err := os.Remove(lockPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(lockPath, 0o755); err != nil {
					t.Fatal(err)
				}
			}

			busy, err := tg.PoolBusyReadOnly(town, pool)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("PoolBusyReadOnly = (%v, nil); want the slot read's error, never a silent free", busy)
				}
				return
			}
			if err != nil {
				t.Fatalf("PoolBusyReadOnly: %v", err)
			}
			if busy != tc.want {
				t.Errorf("PoolBusyReadOnly = %v; want %v", busy, tc.want)
			}
			if len(tc.held) == 0 {
				// With no slot held the probe took the flock to read it, and a
				// probe that kept that hold would refuse the gate's next suite:
				// the second read must see the same free pool.
				again, err := tg.PoolBusyReadOnly(town, pool)
				if err != nil || again != busy {
					t.Errorf("the second read = (%v, %v); want %v: the probe must not keep a slot it read", again, err, busy)
				}
			}
		})
	}
}

// TestPool_ReentrantMarkerOnAnySlot: a descendant of a holder of slot N
// (not just slot 0) takes the reentrant fast path, even when every slot is
// held.
func TestPool_ReentrantMarkerOnAnySlot(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := Pool{Slots: 2}

	h0 := tg.mustAcquirePool(t, town, "gastown/a", pool)
	h1 := tg.mustAcquirePool(t, town, "gastown/b", pool)
	defer h0.Release()
	defer h1.Release()

	// A child of the slot 1 holder (it inherited that holder's marker, and
	// has a different PID than ours) doing the holder's role's work — the
	// marker only reaches that role's work (gt-off9, see reentrantMark.grants).
	child := tg.child()
	h, err, elapsed := tg.run(t, func() (*Handle, error) { return child.AcquirePool(town, "gastown/b", shortWait, pool) })
	if err != nil {
		t.Fatalf("reentrant AcquirePool: %v", err)
	}
	if !h.reentrant || elapsed != 0 {
		t.Fatalf("expected an immediate reentrant handle, got reentrant=%v after %s", h.reentrant, elapsed)
	}
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	// The real holders are untouched: a reentrant handle holds nothing of
	// its own, and its Release must not clear an ancestor's marker.
	rep, _ := tg.StatusPool(town, pool)
	if rep.HeldCount != 2 {
		t.Fatalf("reentrant Release must not release a real slot: %+v", rep)
	}
}

// TestPool_RealAcquireNeverRidesTheMarker is the gt-off9 test for the
// daemon's side of the fix: AcquirePoolReal must take a real, visible hold
// even when the process carries a live marker naming its own role — the shape
// a daemon spawned by a holder of that role has.
func TestPool_RealAcquireNeverRidesTheMarker(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	const role = "mango/landing"

	pool := Pool{Slots: 2}
	holder := tg.mustAcquirePool(t, town, role, pool)
	defer release(t, holder)
	daemon := tg.child()

	// Sanity check on the hole this closes: under the live hold, the ordinary
	// AcquirePool hands out a reentrant handle that holds nothing at all.
	reentrant, err, _ := tg.run(t, func() (*Handle, error) { return daemon.AcquirePool(town, role, shortWait, pool) })
	if err != nil {
		t.Fatalf("AcquirePool under a same-role marker: %v", err)
	}
	if !reentrant.reentrant {
		t.Fatalf("AcquirePool under a same-role marker returned a real handle, want the reentrant fast path")
	}

	h, err, _ := tg.run(t, func() (*Handle, error) { return daemon.AcquirePoolReal(town, role, 5*time.Second, pool) })
	if err != nil {
		t.Fatalf("AcquirePoolReal: %v", err)
	}
	if h.reentrant {
		t.Fatalf("AcquirePoolReal returned a reentrant handle; the daemon's suite would be invisible to every other caller")
	}

	rep, err := tg.StatusPool(town, pool)
	if err != nil {
		t.Fatalf("StatusPool while the real hold is outstanding: %v", err)
	}
	if rep.HeldCount != 2 || h.Index == holder.Index {
		t.Fatalf("AcquirePoolReal took no slot of its own — the hold is invisible: index %d beside the holder's %d, %+v", h.Index, holder.Index, rep)
	}
	if owner := rep.Slots[h.Index].Owner; owner == nil || owner.Role != role || owner.PID != daemon.pid {
		t.Fatalf("owner file should name the real holder: %+v", owner)
	}

	// It arms the marker for its own descendants like any other holder, but
	// names its own role — the part that keeps the marker harmless in the
	// unrelated processes the daemon spawns while holding (reentrantMark.grants).
	if got := daemon.env.Getenv(reentrantHolderEnvVar); got != reentrantEnvValue(town, h.Index, role, daemon.pid) {
		t.Fatalf("marker after AcquirePoolReal = %q, want this hold named by its own role", got)
	}

	if err := h.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if rep, _ = tg.StatusPool(town, pool); rep.HeldCount != 1 {
		t.Fatalf("the hold outlived Release: %+v", rep)
	}
}

func TestPool_NormalizedAndCandidates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in           Pool
		wantSlots    int
		wantReserved int
		polecat      []int
		gate         []int
	}{
		{Pool{}, 1, 0, []int{0}, []int{0}},
		{Pool{Slots: 1, ReservedForGate: 1}, 1, 0, []int{0}, []int{0}},
		{Pool{Slots: 3, ReservedForGate: 1}, 3, 1, []int{1, 2}, []int{0, 1, 2}},
		{Pool{Slots: 3, ReservedForGate: 5}, 3, 2, []int{2}, []int{0, 1, 2}},
		{Pool{Slots: 2, ReservedForGate: -1}, 2, 0, []int{0, 1}, []int{0, 1}},
	}
	eq := func(a, b []int) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	for _, c := range cases {
		n := c.in.normalized()
		if n.Slots != c.wantSlots || n.ReservedForGate != c.wantReserved {
			t.Errorf("normalized(%+v) = %+v, want slots=%d reserved=%d", c.in, n, c.wantSlots, c.wantReserved)
		}
		if got := c.in.candidates("gastown/amber"); !eq(got, c.polecat) {
			t.Errorf("candidates polecat %+v = %v, want %v", c.in, got, c.polecat)
		}
		if got := c.in.candidates("gastown/landing"); !eq(got, c.gate) {
			t.Errorf("candidates gate %+v = %v, want %v", c.in, got, c.gate)
		}
	}
	for role, want := range map[string]bool{
		"gastown/landing": true, "hm/landing": true, "gastown/post-land": true,
		"gastown/refinery": false, "hm/main-branch-test": false, "gastown/post-land-impostor": false,
		"gastown/amber": false, "gastown/landing-impostor-polecat": false, "pid-1234": false,
	} {
		if IsGateRole(role) != want {
			t.Errorf("IsGateRole(%q) = %v, want %v", role, !want, want)
		}
	}
}

// TestTimeoutError_NamesTheCause covers gt-18zj: the failure a caller gets back
// after exhausting its cap carries the wait's cause, and a wait that nothing
// ever blocked still reads as one sentence.
func TestTimeoutError_NamesTheCause(t *testing.T) {
	t.Parallel()
	const bare = "timed out after 30s waiting for container-gate slot"
	cases := []struct {
		name string
		info waitInfo
		want string
	}{
		{
			"holder",
			waitInfo{Reason: WaitReasonTokenHeld, Holder: &Owner{Role: "gastown/polecats/mica", PID: 62965}},
			"token held by gastown/polecats/mica pid 62965",
		},
		{
			"unwrapped suite",
			waitInfo{Reason: WaitReasonUnwrappedContainers, Containers: []string{"dolt/dolt-sql-server:2.2.0 stuck-suite"}},
			"unwrapped container suite: dolt/dolt-sql-server:2.2.0 stuck-suite",
		},
		{
			"wedged docker probe",
			waitInfo{Reason: WaitReasonDaemonUnreachable, DockerErr: "docker ps did not respond within 5s"},
			"docker probe inconclusive: docker ps did not respond within 5s",
		},
		{
			"running gate",
			waitInfo{Reason: WaitReasonGateRunning, Holder: &Owner{Role: "gastown/landing", PID: 42, Slot: 0}},
			"gate running: gastown/landing pid 42 holds gate-reserved slot 0",
		},
		{
			"the full-suite cap",
			waitInfo{Reason: WaitReasonFullSuiteHeld, Holder: &Owner{Role: "gastown/tier-sweep", PID: 42, Slot: 2}},
			"full-suite cap: gastown/tier-sweep pid 42 is already running a whole-tree suite (slot 2)",
		},
		{"no blocker", waitInfo{}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := timeoutError(30*time.Second, c.info).Error()
			if !strings.Contains(got, bare) {
				t.Fatalf("timeoutError = %q, want it to contain %q", got, bare)
			}
			if c.want == "" {
				if got != bare {
					t.Errorf("timeoutError with no blocker = %q, want %q with no dangling cause", got, bare)
				}
				return
			}
			if !strings.Contains(got, c.want) {
				t.Errorf("timeoutError = %q, want it to contain %q", got, c.want)
			}
		})
	}
}
