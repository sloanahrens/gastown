package slot

import (
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// shortWait is a timed-acquire budget for tests that expect a timeout. The
// poll loop sleeps DefaultPollInterval between attempts, so the call returns
// after roughly one poll, not after shortWait itself.
const shortWait = 200 * time.Millisecond

func mustAcquirePool(t *testing.T, town, role string, pool Pool) *Handle {
	t.Helper()
	h, err := AcquirePool(town, role, 5*time.Second, pool)
	if err != nil {
		t.Fatalf("AcquirePool(%q): %v", role, err)
	}
	return h
}

// TestPool_ThreeSlotsHoldConcurrently: a 3-slot pool hands out exactly three
// slots (indices 0,1,2 in order), refuses a fourth, reports all held, and
// hands a released slot straight back out.
func TestPool_ThreeSlotsHoldConcurrently(t *testing.T) {
	stubNoContainers(t)
	town := t.TempDir()
	pool := Pool{Slots: 3}

	var held []*Handle
	for i := 0; i < 3; i++ {
		h := mustAcquirePool(t, town, "gastown/polecat-"+strconv.Itoa(i), pool)
		if h.Index != i {
			t.Fatalf("acquire #%d got slot %d, want %d", i, h.Index, i)
		}
		held = append(held, h)
	}

	if _, err := AcquirePool(town, "gastown/polecat-3", shortWait, pool); err == nil {
		t.Fatal("fourth AcquirePool succeeded on a fully held 3-slot pool")
	}

	rep, err := StatusPool(town, pool)
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
	rep, _ = StatusPool(town, pool)
	if rep.HeldCount != 2 || rep.Busy() || rep.Slots[1].Held {
		t.Fatalf("status after releasing slot 1: %+v", rep)
	}
	if _, err := os.Stat(SlotOwnerPath(town, 1)); !os.IsNotExist(err) {
		t.Fatalf("slot 1 owner file should be gone after Release, stat err=%v", err)
	}
	h := mustAcquirePool(t, town, "gastown/polecat-4", pool)
	if h.Index != 1 {
		t.Fatalf("re-acquire after release got slot %d, want 1", h.Index)
	}
}

// TestPool_HeldSlotNamesTheWaitOnce covers gt-78b8: a caller that cannot be
// granted because another process holds every candidate slot says who holds it
// at the top of the wait, and says it once for the whole wait rather than on
// every poll.
func TestPool_HeldSlotNamesTheWaitOnce(t *testing.T) {
	stubNoContainers(t)
	town := t.TempDir()
	pool := Pool{Slots: 1}

	held := mustAcquirePool(t, town, "gastown/refinery", pool)
	defer func() { _ = held.Release() }()

	prev := probeWriter
	var out strings.Builder
	probeWriter = &out
	t.Cleanup(func() { probeWriter = prev })

	if _, err := AcquirePool(town, "gastown/amber", shortWait, pool); err == nil {
		t.Fatal("AcquirePool was granted a slot while the only slot was held")
	}

	if n := strings.Count(out.String(), "waiting for container-gate slot"); n != 1 {
		t.Errorf("wait lines = %d, want one line for the whole wait: %q", n, out.String())
	}
	if !strings.Contains(out.String(), "token held by gastown/refinery") {
		t.Errorf("wait line = %q, want the holder named", out.String())
	}
	if !strings.Contains(out.String(), "cap ") {
		t.Errorf("wait line = %q, want the caller's timeout cap", out.String())
	}
}

// TestPool_ReservedSlotOnlyForGateRoles: with one slot reserved, polecats
// never take slot 0 even when it is the only free slot; gate roles do.
func TestPool_ReservedSlotOnlyForGateRoles(t *testing.T) {
	stubNoContainers(t)
	town := t.TempDir()
	pool := Pool{Slots: 3, ReservedForGate: 1}

	a := mustAcquirePool(t, town, "gastown/amber", pool)
	b := mustAcquirePool(t, town, "gastown/basalt", pool)
	if a.Index != 1 || b.Index != 2 {
		t.Fatalf("polecats got slots %d and %d, want 1 and 2 (slot 0 is reserved)", a.Index, b.Index)
	}
	if _, err := AcquirePool(town, "gastown/coral", shortWait, pool); err == nil {
		t.Fatal("third polecat got a slot while only the reserved slot 0 was free")
	}

	rep, _ := StatusPool(town, pool)
	if rep.Busy() {
		t.Fatalf("pool must not report busy while the reserved slot is free: %+v", rep)
	}

	r := mustAcquirePool(t, town, "gastown/refinery", pool)
	if r.Index != 0 {
		t.Fatalf("refinery got slot %d, want the reserved slot 0", r.Index)
	}
	if _, err := AcquirePool(town, "gastown/refinery-batch", shortWait, pool); err == nil {
		t.Fatal("batch gate got a slot with every slot held")
	}

	_ = a.Release()
	c := mustAcquirePool(t, town, "gastown/coral", pool)
	if c.Index != 1 {
		t.Fatalf("polecat after release got slot %d, want 1", c.Index)
	}
}

// TestPool_GateRoleFallsBackToSharedSlot: gate roles prefer reserved slots
// but may take a shared one when the reserved ones are held.
func TestPool_GateRoleFallsBackToSharedSlot(t *testing.T) {
	stubNoContainers(t)
	town := t.TempDir()
	pool := Pool{Slots: 2, ReservedForGate: 1}

	r := mustAcquirePool(t, town, "gastown/refinery", pool)
	m := mustAcquirePool(t, town, "gastown/main-branch-test", pool)
	if r.Index != 0 || m.Index != 1 {
		t.Fatalf("gate roles got slots %d and %d, want 0 then 1", r.Index, m.Index)
	}
}

// TestPool_UnwrappedCheckSkippedWhenAnotherSlotHeld: running gate
// containers block a grant only when nobody holds a slot (gt-tuiy). Once any
// slot is held, containers are the holder's and a second slot is granted.
func TestPool_UnwrappedCheckSkippedWhenAnotherSlotHeld(t *testing.T) {
	var containersUp atomic.Bool
	orig := runningGateContainers
	runningGateContainers = func() ([]string, error) {
		if containersUp.Load() {
			return []string{"dolt/dolt-sql-server:2.2.0 suite"}, nil
		}
		return nil, nil
	}
	t.Cleanup(func() { runningGateContainers = orig })

	town := t.TempDir()
	pool := Pool{Slots: 2}

	h0 := mustAcquirePool(t, town, "gastown/refinery", pool)
	containersUp.Store(true)

	// Slot 0 is held, so the containers are legitimately the refinery's:
	// slot 1 must be granted without waiting for them to clear.
	start := time.Now()
	h1, err := AcquirePool(town, "gastown/amber", 5*time.Second, pool)
	if err != nil {
		t.Fatalf("AcquirePool with another slot held and containers up: %v", err)
	}
	if h1.Index != 1 {
		t.Fatalf("got slot %d, want 1", h1.Index)
	}
	if time.Since(start) > DefaultPollInterval {
		t.Fatalf("grant took %s; it should not have waited on the container check", time.Since(start))
	}

	// Control: with NO slot held and containers up, the same acquire is an
	// unwrapped-suite case and must time out.
	_ = h0.Release()
	_ = h1.Release()
	if _, err := AcquirePool(town, "gastown/amber", shortWait, pool); err == nil {
		t.Fatal("AcquirePool succeeded with unwrapped containers up and no slot held")
	}
}

// TestStatus_DiscoversPoolSlots: a single-slot Status (an older caller or a
// config that shrank) still sees slots created by a larger pool.
func TestStatus_DiscoversPoolSlots(t *testing.T) {
	stubNoContainers(t)
	town := t.TempDir()
	pool := Pool{Slots: 3}

	h0 := mustAcquirePool(t, town, "gastown/a", pool)
	h1 := mustAcquirePool(t, town, "gastown/b", pool)
	h2 := mustAcquirePool(t, town, "gastown/c", pool)
	_ = h0.Release()
	_ = h1.Release()

	rep, err := Status(town)
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
	town := t.TempDir()
	pool := Pool{Slots: 2}

	// Acquiring legitimately probes docker: it decides whether a suite may
	// start. Only the reads under test are held to the flock alone.
	stubNoContainers(t)
	h := mustAcquirePool(t, town, "gastown/amber", pool)

	var calls int
	restore := SetContainerListerForTest(func() ([]string, error) {
		calls++
		return []string{"dolt/dolt-sql-server:2.2.0 unwrapped-suite"}, nil
	})
	t.Cleanup(restore)

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
	held, err := StatusPool(town, pool)
	if err != nil {
		t.Fatalf("StatusPool (held): %v", err)
	}
	if calls != 0 {
		t.Fatalf("StatusPool probed docker while a slot was held: %d call(s)", calls)
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
	if calls != 0 {
		t.Fatalf("StatusPoolLocksOnly probed docker %d time(s); it must read the flocks alone", calls)
	}

	// The full report still probes — that is the half callers ask it for.
	full, err := StatusPool(town, pool)
	if err != nil {
		t.Fatalf("StatusPool: %v", err)
	}
	if calls != 1 {
		t.Fatalf("StatusPool made %d docker probes, want exactly 1", calls)
	}
	if len(full.UnwrappedContainers) != 1 {
		t.Fatalf("StatusPool UnwrappedContainers = %v, want the one container up with no slot held", full.UnwrappedContainers)
	}
}

// TestPool_ReentrantMarkerOnAnySlot: a descendant of a holder of slot N
// (not just slot 0) takes the reentrant fast path, even when every slot is
// held.
func TestPool_ReentrantMarkerOnAnySlot(t *testing.T) {
	stubNoContainers(t)
	town := t.TempDir()
	pool := Pool{Slots: 2}

	h0 := mustAcquirePool(t, town, "gastown/a", pool)
	h1 := mustAcquirePool(t, town, "gastown/b", pool)
	defer h0.Release()
	defer h1.Release()

	// Slot 1, a different PID than ours as a real child would see it, and
	// the child's own role — the marker only reaches that role's work
	// (gt-off9, see reentrantMark.grants).
	t.Setenv(ReentrantEnvVar, reentrantEnvValue(town, 1, "gastown/b-child", os.Getpid()+100000))
	start := time.Now()
	h, err := AcquirePool(town, "gastown/b-child", shortWait, pool)
	if err != nil {
		t.Fatalf("reentrant AcquirePool: %v", err)
	}
	if !h.reentrant || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("expected an immediate reentrant handle, got reentrant=%v after %s", h.reentrant, time.Since(start))
	}
	if err := h.Release(); err != nil {
		t.Fatal(err)
	}
	// The real holders are untouched: a reentrant handle holds nothing of
	// its own, and its Release must not clear an ancestor's marker.
	rep, _ := StatusPool(town, pool)
	if rep.HeldCount != 2 {
		t.Fatalf("reentrant Release must not release a real slot: %+v", rep)
	}
}

// TestPool_RealAcquireNeverRidesTheMarker is the gt-off9 test for the
// daemon's side of the fix: AcquirePoolReal must take a real, visible hold
// even when the process carries a marker naming its own role — the shape a
// marker inherited from a predecessor daemon process has, whose flock the
// kernel already dropped.
func TestPool_RealAcquireNeverRidesTheMarker(t *testing.T) {
	stubNoContainers(t)
	town := t.TempDir()
	const role = "gastown/main-branch-test"

	t.Setenv(ReentrantEnvVar, reentrantEnvValue(town, 0, role, os.Getpid()+100000))

	// Sanity check on the hole this closes: with this marker, the ordinary
	// AcquirePool hands out a reentrant handle that holds nothing at all.
	reentrant, err := AcquirePool(town, role, shortWait, DefaultPool)
	if err != nil {
		t.Fatalf("AcquirePool under a same-role marker: %v", err)
	}
	if !reentrant.reentrant {
		t.Fatalf("AcquirePool under a same-role marker returned a real handle, want the reentrant fast path")
	}

	h, err := AcquirePoolReal(town, role, 5*time.Second, DefaultPool)
	if err != nil {
		t.Fatalf("AcquirePoolReal: %v", err)
	}
	if h.reentrant {
		t.Fatalf("AcquirePoolReal returned a reentrant handle; the daemon's suite would be invisible to every other caller")
	}

	rep, err := Status(town)
	if err != nil {
		t.Fatalf("Status while the real hold is outstanding: %v", err)
	}
	if !rep.Held {
		t.Fatalf("AcquirePoolReal took no flock — the hold is invisible: %+v", rep)
	}
	if rep.Owner == nil || rep.Owner.Role != role || rep.Owner.PID != os.Getpid() {
		t.Fatalf("owner file should name the real holder: %+v", rep.Owner)
	}

	// It arms the marker for its own descendants like any other holder, but
	// names its own role — the part that keeps the marker harmless in the
	// unrelated processes the daemon spawns while holding (reentrantMark.grants).
	if got := os.Getenv(ReentrantEnvVar); got != reentrantEnvValue(town, h.Index, role, os.Getpid()) {
		t.Fatalf("marker after AcquirePoolReal = %q, want this hold named by its own role", got)
	}

	if err := h.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if rep, _ = Status(town); rep.Held {
		t.Fatalf("the hold outlived Release: %+v", rep)
	}
}

func TestPool_NormalizedAndCandidates(t *testing.T) {
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
		if got := c.in.candidates("gastown/refinery"); !eq(got, c.gate) {
			t.Errorf("candidates gate %+v = %v, want %v", c.in, got, c.gate)
		}
	}
	for role, want := range map[string]bool{
		"gastown/refinery": true, "gastown/refinery-batch": true, "hm/main-branch-test": true,
		"gastown/amber": false, "gastown/refinery-impostor-polecat": true, "pid-1234": false,
	} {
		if IsGateRole(role) != want {
			t.Errorf("IsGateRole(%q) = %v, want %v", role, !want, want)
		}
	}
}
