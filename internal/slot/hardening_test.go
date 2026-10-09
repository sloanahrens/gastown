package slot

import (
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/lock"
)

// claimJSON is a slot owner file as another process writes it: the wire format
// these readers parse, with the fields under test spelled out. extra carries
// the optional keys (settled, start).
func claimJSON(role string, pid int, extra string) []byte {
	return []byte(fmt.Sprintf(`{"role":%q,"pid":%d,"acquired_at":"2026-09-27T12:00:00Z"%s}`, role, pid, extra))
}

// holdSlotClaim publishes slot i's owner file as this process's own claim and
// takes the slot's flock, returning the unlock. It is the state AcquirePool
// sits in twice: between writeSlotOwner and the grant (settled false, the
// claimant still probing) and between the grant and the release (settled true,
// a holder running its work).
func holdSlotClaim(t *testing.T, townRoot string, i int, role string, settled bool) func() {
	t.Helper()
	if err := os.MkdirAll(LockDir(townRoot), 0755); err != nil {
		t.Fatal(err)
	}
	unlock, ok, err := lock.FlockTryAcquire(SlotLockPath(townRoot, i))
	if err != nil || !ok {
		t.Fatalf("holding slot %d: ok=%v err=%v", i, ok, err)
	}
	extra := ""
	if settled {
		extra = `,"settled":true`
	}
	if err := os.WriteFile(SlotOwnerPath(townRoot, i), claimJSON(role, os.Getpid(), extra), 0644); err != nil {
		unlock()
		t.Fatalf("writing slot %d's owner file: %v", i, err)
	}
	return unlock
}

// TestPool_UnwrappedCheckNotSkippedForAMidClaimHolder: running containers are
// taken to be a held slot's suite, so a second acquiror skips the
// unwrapped-container probe once another slot is held. A slot whose holder is
// still in its acquisition probe is not a holder yet — it may release — and
// treating it as one is how two simultaneous acquirors each skipped the probe
// and let an unwrapped suite run beside them (gt-u0zq0).
func TestPool_UnwrappedCheckNotSkippedForAMidClaimHolder(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := Pool{Slots: 2}

	unlock := holdSlotClaim(t, town, 0, "gastown/amber", false)
	defer unlock()
	tg.rt.setLines("dolt/dolt-sql-server:2.2.0 suite")

	if _, err, _ := tg.run(t, func() (*Handle, error) { return tg.AcquirePool(town, "gastown/landing", shortWait, pool) }); err == nil {
		t.Fatal("AcquirePool granted beside a mid-claim holder without probing for an unwrapped suite")
	}
}

// TestPool_UnwrappedCheckSkippedForASettledHolder is the other side of the same
// rule: a holder past its probe is exactly what the skip is for, so the second
// slot is granted without waiting on the container check the holder owns.
func TestPool_UnwrappedCheckSkippedForASettledHolder(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := Pool{Slots: 2}

	unlock := holdSlotClaim(t, town, 0, "gastown/amber", true)
	defer unlock()
	tg.rt.setLines("dolt/dolt-sql-server:2.2.0 suite")

	h, err, elapsed := tg.run(t, func() (*Handle, error) { return tg.AcquirePool(town, "gastown/landing", 5*time.Second, pool) })
	if err != nil {
		t.Fatalf("AcquirePool beside a settled holder: %v", err)
	}
	if h.Index != 1 {
		t.Fatalf("got slot %d, want 1", h.Index)
	}
	if elapsed != 0 {
		t.Fatalf("grant waited %s; a held slot's suite must not be probed for", elapsed)
	}
	if probes := tg.rt.calls(); probes != 0 {
		t.Fatalf("the container probe ran %d time(s) while another slot was held", probes)
	}
}

// TestOwnerGoneWhenThePIDWasReused: kill -0 reports that some process has the
// owner's pid, and pids come back around. An owner file naming a dead holder's
// pid keeps a full-suite holder (and a running gate) on the books until the
// reaper runs, unless the holder's process start token is compared with the
// token of the process at that pid now (gt-u0zq0).
func TestOwnerGoneWhenThePIDWasReused(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	townRoot := t.TempDir()
	pool := fullSuitePool()
	if err := os.MkdirAll(LockDir(townRoot), 0755); err != nil {
		t.Fatal(err)
	}

	const reused = 4242
	tg.gone[reused] = false              // a process with this pid exists...
	tg.starts[reused] = "1790000000.200" // ...but it started at a different time
	seed := func(slot int, role, start string) {
		t.Helper()
		if err := os.WriteFile(SlotOwnerPath(townRoot, slot), claimJSON(role, reused, `,"start":"`+start+`"`), 0644); err != nil {
			t.Fatalf("writing slot %d's owner file: %v", slot, err)
		}
	}

	seed(1, "gastown/landing", "1790000000.100")
	seed(3, "gastown/tier-sweep", "1790000000.100")
	if holders := tg.liveFullSuiteHolders(townRoot, pool); len(holders) != 0 {
		t.Fatalf("liveFullSuiteHolders = %+v, want none: the pid carried a different start token", holders)
	}
	if owner, ok := tg.runningGate(townRoot, pool); ok {
		t.Fatalf("runningGate = %+v, want no gate: the pid carried a different start token", owner)
	}

	// Control: the same pid with the start token the file recorded is the
	// holder those readers exist to see.
	tg.starts[reused] = "1790000000.100"
	if holders := tg.liveFullSuiteHolders(townRoot, pool); len(holders) != 1 {
		t.Fatalf("liveFullSuiteHolders = %+v, want the holder whose start token matches", holders)
	}
	if owner, ok := tg.runningGate(townRoot, pool); !ok || owner == nil {
		t.Fatalf("runningGate = %+v, %v; want the gate whose start token matches", owner, ok)
	}
}
