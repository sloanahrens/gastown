package slot

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// fullSuitePool is the town's shape (yieldPool) with the whole-tree cap on:
// four slots, two reserved for the gate, one full-suite-class holder at a time.
func fullSuitePool() Pool {
	return Pool{Slots: 4, ReservedForGate: 2, YieldToGate: true, MaxFullSuites: 1}
}

// TestIsFullSuiteRole pins the class: a whole-tree test run's role. The tier
// and flake sweeps are the named ones; any other caller opts in with the
// /full-suite suffix. Gate roles are deliberately not in it — the landing gate
// runs the whole tree too, but it must never wait on this cap.
func TestIsFullSuiteRole(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		role string
		want bool
	}{
		{name: "the tier sweep", role: "gastown/tier-sweep", want: true},
		{name: "the flake sweep", role: "gastown/flake-sweep", want: true},
		{name: "a caller that names the class", role: "gastown/crew/sloan/full-suite", want: true},
		{name: "a rig-qualified class role", role: "gastown/full-suite", want: true},
		{name: "the landing gate is not full-suite class", role: "gastown/landing", want: false},
		{name: "the post-land runner is not full-suite class", role: "gastown/post-land", want: false},
		{name: "a package-scoped suite", role: "gastown/crew/sloan", want: false},
		{name: "a polecat seat", role: "gastown/polecats/agate", want: false},
		{name: "the unnamed placeholder", role: "pid-1234", want: false},
		{name: "a suffix that only looks like the class", role: "gastown/full-suite-x", want: false},
		{name: "a role merely mentioning the class", role: "gastown/full-suitery", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsFullSuiteRole(tt.role); got != tt.want {
				t.Errorf("IsFullSuiteRole(%q) = %v, want %v", tt.role, got, tt.want)
			}
			if tt.want && IsGateRole(tt.role) {
				t.Errorf("IsFullSuiteRole(%q) is true and IsGateRole(%q) is true; the classes must be disjoint", tt.role, tt.role)
			}
		})
	}
}

// admissionCase is one row of the admission matrix: the pool, the holders in
// place before the attempt, the role attempting, and the outcome it must get.
type admissionCase struct {
	name string
	pool Pool
	// hold is the roles acquired before the attempt, in order.
	hold []string
	// dead marks every holder's owner process as gone after it was acquired, so
	// its owner file outlives a hold the kernel has already dropped.
	dead bool
	// role is the role attempting the acquisition under test.
	role string
	// wantSlot is the slot the attempt must be granted. Negative means the
	// attempt must NOT be granted while the holders run: it waits for the cap.
	wantSlot int
	// wantWait is a substring the wait line must carry, for a negative
	// wantSlot.
	wantWait string
}

// TestFullSuite_AdmissionMatrix covers acceptance 1 and 2 as a table: which
// whole-tree starts a pool admits, which it holds at the cap, and what the
// gate, a package-scoped suite and a dead holder do to that decision.
func TestFullSuite_AdmissionMatrix(t *testing.T) {
	t.Parallel()
	fiveSlots := Pool{Slots: 5, ReservedForGate: 2, YieldToGate: true, MaxFullSuites: 2}
	uncapped := fullSuitePool()
	uncapped.MaxFullSuites = 0
	noYield := fullSuitePool()
	noYield.YieldToGate = false

	tests := []admissionCase{
		{
			name: "a whole-tree start with no holder is admitted",
			pool: fullSuitePool(), role: "gastown/tier-sweep", wantSlot: 2,
		},
		{
			name: "a whole-tree start at the cap waits for the running one",
			pool: fullSuitePool(), hold: []string{"gastown/tier-sweep"},
			role: "gastown/flake-sweep", wantSlot: -1, wantWait: "full-suite cap",
		},
		{
			name: "a third whole-tree start waits at a cap of 2",
			pool: fiveSlots, hold: []string{"gastown/tier-sweep", "gastown/flake-sweep"},
			role: "gastown/crew/sloan/full-suite", wantSlot: -1, wantWait: "full-suite cap",
		},
		{
			name: "one below the cap is admitted",
			pool: fiveSlots, hold: []string{"gastown/tier-sweep"},
			role: "gastown/flake-sweep", wantSlot: 3,
		},
		{
			name: "max_full_suites 0 is no cap",
			pool: uncapped, hold: []string{"gastown/tier-sweep"},
			role: "gastown/flake-sweep", wantSlot: 3,
		},
		{
			name: "a package-scoped suite is not capped",
			pool: fullSuitePool(), hold: []string{"gastown/tier-sweep"},
			role: "gastown/crew/sloan", wantSlot: 3,
		},
		{
			name: "the landing gate is not capped and does not queue behind one",
			pool: fullSuitePool(), hold: []string{"gastown/tier-sweep"},
			role: "gastown/landing", wantSlot: 0,
		},
		{
			name: "a second gate takes the next reserved slot",
			pool: fullSuitePool(), hold: []string{"gastown/tier-sweep", "gastown/landing"},
			role: "hm/landing", wantSlot: 1,
		},
		{
			name: "a gate does not consume the cap",
			pool: noYield, hold: []string{"gastown/landing"},
			role: "gastown/tier-sweep", wantSlot: 2,
		},
		{
			name: "a dead whole-tree holder does not hold the cap",
			pool: fullSuitePool(), hold: []string{"gastown/tier-sweep"}, dead: true,
			role: "gastown/flake-sweep", wantSlot: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tg := newTestGate(t)
			town := t.TempDir()

			var held []*Handle
			for _, role := range tt.hold {
				held = append(held, tg.mustAcquirePool(t, town, role, tt.pool))
			}
			if tt.dead {
				tg.gone[tg.pid] = true
			}

			done := goAcquire(func() (*Handle, error) { return tg.AcquirePool(town, tt.role, time.Hour, tt.pool) })
			if tt.wantSlot < 0 {
				waitBlocked(t, tg.clk)
				select {
				case got := <-done:
					t.Fatalf("acquire returned while it should have waited at the cap: h=%+v err=%v", got.h, got.err)
				default:
				}
				if out := tg.probe.String(); !strings.Contains(out, tt.wantWait) {
					t.Errorf("wait line = %q, want it to contain %q", out, tt.wantWait)
				}
				// The cap is not a slot: while the attempt waits, it holds
				// nothing of its own.
				rep, err := tg.StatusPoolLocksOnly(town, tt.pool)
				if err != nil {
					t.Fatal(err)
				}
				if rep.HeldCount != len(held) {
					t.Fatalf("held slots while the attempt waited = %d, want only the %d pre-existing holds", rep.HeldCount, len(held))
				}
				// The cap lifts when the holder releases.
				for _, h := range held {
					release(t, h)
				}
			}

			got := driveClock(t, tg.clk, tg.pollInterval, done)
			if got.err != nil {
				t.Fatalf("AcquirePool(%q): %v", tt.role, got.err)
			}
			defer release(t, got.h)
			if tt.wantSlot >= 0 {
				if got.h.Index != tt.wantSlot {
					t.Errorf("%s got slot %d, want %d", tt.role, got.h.Index, tt.wantSlot)
				}
				if got.h.WaitedFor > 0 {
					t.Errorf("%s waited %s for an admitted slot, want no wait", tt.role, got.h.WaitedFor)
				}
				// The holders are still in place: assert the index against the
				// slots they occupy, then let them go.
				for _, h := range held {
					release(t, h)
				}
			}
		})
	}
}

// TestFullSuite_SecondWholeTreeRunWaitsForTheFirst is acceptance 1's end-to-end
// shape: two whole-tree runs through the slot serialize, and the second reports
// a measured wait (WaitedFor, which `gt slot run` prints as "waited Ns" —
// TestAcquiredFormat pins that line).
func TestFullSuite_SecondWholeTreeRunWaitsForTheFirst(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := fullSuitePool()

	first := tg.mustAcquirePool(t, town, "gastown/tier-sweep", pool)
	if first.Index != 2 {
		t.Fatalf("the first whole-tree run got slot %d, want the first shared slot 2", first.Index)
	}

	done := goAcquire(func() (*Handle, error) { return tg.AcquirePool(town, "gastown/flake-sweep", time.Hour, pool) })
	for i := 0; i < 3; i++ {
		waitBlocked(t, tg.clk)
		select {
		case got := <-done:
			t.Fatalf("the second whole-tree run started while the first held the cap: h=%+v err=%v", got.h, got.err)
		default:
		}
		tg.clk.Advance(tg.pollInterval)
	}
	if out := tg.probe.String(); !strings.Contains(out, "full-suite cap") || !strings.Contains(out, "gastown/tier-sweep") {
		t.Errorf("wait line = %q, want it to name the cap and the run it cedes to", out)
	}

	// Ceding to the cap is not competing for a free slot: slot 3 stays empty
	// while the second run waits.
	rep, err := tg.StatusPoolLocksOnly(town, pool)
	if err != nil {
		t.Fatal(err)
	}
	if rep.HeldCount != 1 {
		t.Fatalf("held slots while the second run waited = %d, want only the first run's", rep.HeldCount)
	}

	release(t, first)
	got := driveClock(t, tg.clk, tg.pollInterval, done)
	if got.err != nil {
		t.Fatalf("the second whole-tree run after the first released: %v", got.err)
	}
	defer release(t, got.h)
	if got.h.WaitedFor == 0 {
		t.Error("the second whole-tree run acquired with no measured wait")
	}
	// What `gt slot run` prints for this handle (TestAcquiredFormat pins the
	// line's shape).
	line := fmt.Sprintf(acquiredFormat, "gastown/flake-sweep", got.h.WaitedFor.Round(time.Second), got.h.Index, pool.Slots)
	if strings.Contains(line, "waited 0s") {
		t.Errorf("the acquire line reports no wait: %q", line)
	}

	hist, err := History(town)
	if err != nil {
		t.Fatal(err)
	}
	last := hist[len(hist)-1]
	if last.Reason != WaitReasonFullSuiteHeld || last.HolderRole != "gastown/tier-sweep" || last.WaitedS <= 0 {
		t.Fatalf("history entry = %+v, want the wait attributed to the running whole-tree suite", last)
	}
}

// TestFullSuite_LandingGateDoesNotQueueBehindAWholeTreeRun is acceptance 2: a
// gate holds priority. A landing gate started while a whole-tree run holds a
// shared slot takes its gate-reserved slot at once, and the running suite is
// not preempted.
func TestFullSuite_LandingGateDoesNotQueueBehindAWholeTreeRun(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := fullSuitePool()

	run := tg.mustAcquirePool(t, town, "gastown/tier-sweep", pool)
	defer release(t, run)

	for i, role := range []string{"gastown/landing", "hm/landing"} {
		h, err, elapsed := tg.run(t, func() (*Handle, error) { return tg.AcquirePool(town, role, time.Minute, pool) })
		if err != nil {
			t.Fatalf("%s acquiring beside a whole-tree run: %v", role, err)
		}
		defer release(t, h)
		if elapsed != 0 || h.Index != i {
			t.Fatalf("%s got slot %d after %s, want gate-reserved slot %d with no wait", role, h.Index, elapsed, i)
		}
	}

	rep, err := tg.StatusPoolLocksOnly(town, pool)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Slots[run.Index].Held || rep.Slots[run.Index].Owner.Role != "gastown/tier-sweep" {
		t.Fatalf("the whole-tree run's slot after the gates started: %+v", rep.Slots[run.Index])
	}
}

// TestFullSuite_AClaimIsVisibleWhileTheDockerProbeRuns pins the window that
// matters for a genuinely simultaneous start: a full-suite claimant publishes
// its claim when it takes the flock, before the `docker ps` cross-check that
// can park it for up to dockerPSTimeout. A start landing in that probe must see
// the claim and wait, not take a second slot — the probe is exactly where two
// simultaneous starts would otherwise both win (gt-dhcmp, acceptance 1).
func TestFullSuite_AClaimIsVisibleWhileTheDockerProbeRuns(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := fullSuitePool()

	inProbe := make(chan struct{})
	letProbeFinish := make(chan struct{})
	tg.rt.listFn = func(call int) ([]string, error) {
		if call == 1 {
			close(inProbe)
			<-letProbeFinish
		}
		return nil, nil
	}

	first := goAcquire(func() (*Handle, error) { return tg.AcquirePool(town, "gastown/tier-sweep", time.Hour, pool) })
	<-inProbe // the first whole-tree run holds its slot and is inside docker ps

	done := goAcquire(func() (*Handle, error) { return tg.AcquirePool(town, "gastown/flake-sweep", time.Hour, pool) })
	waitBlocked(t, tg.clk)
	select {
	case got := <-done:
		t.Fatalf("a second whole-tree run started while the first was mid-probe: h=%+v err=%v", got.h, got.err)
	default:
	}
	if out := tg.probe.String(); !strings.Contains(out, "full-suite cap") {
		t.Errorf("wait line = %q, want it to name the full-suite cap", out)
	}

	close(letProbeFinish)
	got := <-first
	if got.err != nil {
		t.Fatalf("the first whole-tree run: %v", got.err)
	}
	release(t, got.h)

	second := driveClock(t, tg.clk, tg.pollInterval, done)
	if second.err != nil {
		t.Fatalf("the second whole-tree run after the first released: %v", second.err)
	}
	release(t, second.h)
}

// TestFullSuite_DescendantUnderAWholeTreeHoldIsNotCapped: work nested inside a
// whole-tree hold that names a different whole-tree role does not cede to the
// very hold it runs under — that would stall the hold on itself forever.
func TestFullSuite_DescendantUnderAWholeTreeHoldIsNotCapped(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := fullSuitePool()

	run := tg.mustAcquirePool(t, town, "gastown/tier-sweep", pool)
	defer release(t, run)

	child := tg.child()
	h, err, elapsed := tg.run(t, func() (*Handle, error) { return child.AcquirePool(town, "gastown/flake-sweep", time.Minute, pool) })
	if err != nil {
		t.Fatal(err)
	}
	defer release(t, h)
	if elapsed != 0 || h.reentrant {
		t.Fatalf("nested acquire under a whole-tree hold: reentrant=%v waited %s, want a real shared slot with no wait", h.reentrant, elapsed)
	}
}

// TestFullSuite_StaleMarkerDoesNotExemptTheCap: a marker outlives its hold in
// every process spawned while it held (gt-off9), so a process carrying one is
// not nested under a hold that is running now and still cedes to the cap.
func TestFullSuite_StaleMarkerDoesNotExemptTheCap(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := fullSuitePool()

	// Inherited from a whole-tree hold on slot 1 that has since ended.
	child := tg.child()
	armReentrant(child.env, town, 1, "mango/flake-sweep", foreignPID()+7)

	run := tg.mustAcquirePool(t, town, "gastown/tier-sweep", pool)
	defer release(t, run)

	done := goAcquire(func() (*Handle, error) {
		return child.AcquirePool(town, "gastown/crew/sloan/full-suite", time.Hour, pool)
	})
	waitBlocked(t, tg.clk)
	select {
	case got := <-done:
		t.Fatalf("a stale-marker process bypassed the cap: h=%+v err=%v", got.h, got.err)
	default:
	}
	release(t, run)
	got := driveClock(t, tg.clk, tg.pollInterval, done)
	if got.err != nil {
		t.Fatalf("stale-marker acquire after the holder released: %v", got.err)
	}
	release(t, got.h)
}
