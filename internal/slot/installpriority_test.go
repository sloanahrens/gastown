package slot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/installpending"
)

// installPriorityPool is a four-slot pool with two slots reserved for the gate
// class, the shape a landing town runs, so a post-landing suite has a shared
// slot to fall back on once it stops yielding.
func installPriorityPool() Pool {
	return Pool{Slots: 4, ReservedForGate: 2}
}

// writePendingInstall records the pending-install marker the daemon writes
// while an install it concluded is due is in flight (gt-vc34c).
func writePendingInstall(t *testing.T, tg *testGate, town string) installpending.Store {
	t.Helper()
	store := installpending.StoreIn(filepath.Join(town, "daemon", installpending.FileName))
	if err := store.Write(tg.clk.Now(), os.Getpid()); err != nil {
		t.Fatalf("writing the pending-install marker: %v", err)
	}
	return store
}

// TestInstallPriority_PostLandYieldsToAPendingInstall: while an install is in
// flight, a post-landing suite does not walk the slots at all — it waits, even
// with every slot free — and goes the moment the marker clears (gt-t135z).
func TestInstallPriority_PostLandYieldsToAPendingInstall(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := installPriorityPool()
	store := writePendingInstall(t, tg, town)

	done := goAcquire(func() (*Handle, error) { return tg.AcquirePool(town, "gastown/post-land", time.Hour, pool) })
	for i := 0; i < 3; i++ {
		waitBlocked(t, tg.clk)
		select {
		case got := <-done:
			t.Fatalf("post-land acquire returned while an install was pending: h=%+v err=%v", got.h, got.err)
		default:
		}
		tg.clk.Advance(tg.pollInterval)
	}
	out := tg.probe.String()
	if !strings.Contains(out, "install pending") {
		t.Errorf("wait line = %q, want it to say an install is pending", out)
	}
	if n := strings.Count(out, "install pending"); n != 1 {
		t.Errorf("wait line printed %d times across polls, want once:\n%s", n, out)
	}

	if err := store.Clear(); err != nil {
		t.Fatalf("clearing the pending-install marker: %v", err)
	}
	got := driveClock(t, tg.clk, tg.pollInterval, done)
	if got.err != nil {
		t.Fatalf("post-land acquire after the install cleared: %v", got.err)
	}
	defer release(t, got.h)
	if got.h.Index != 0 {
		t.Fatalf("post-land suite got slot %d, want the reserved slot 0", got.h.Index)
	}

	hist, err := History(town)
	if err != nil {
		t.Fatal(err)
	}
	last := hist[len(hist)-1]
	if last.Role != "gastown/post-land" || last.Reason != WaitReasonInstallPending {
		t.Fatalf("history entry = %+v, want the wait attributed to the pending install", last)
	}
	if last.InstallPID != os.Getpid() {
		t.Errorf("history install pid = %d, want the marker's writer %d", last.InstallPID, os.Getpid())
	}
}

// TestInstallPriority_LandingIsNeverHeldBack: the merge gate is not in the
// post-landing class, so a live marker does not delay a landing (gt-t135z).
func TestInstallPriority_LandingIsNeverHeldBack(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := installPriorityPool()
	writePendingInstall(t, tg, town)

	h, err, elapsed := tg.run(t, func() (*Handle, error) { return tg.AcquirePool(town, "gastown/landing", time.Minute, pool) })
	if err != nil {
		t.Fatal(err)
	}
	defer release(t, h)
	if elapsed != 0 {
		t.Fatalf("landing waited %s behind a pending install", elapsed)
	}
}

// TestInstallPriority_InstallHoldingASlotEndsTheWait: once the install's build
// holds a slot, its reservation is in place, so post-landing work stops waiting
// on a build that may run long (gt-t135z).
func TestInstallPriority_InstallHoldingASlotEndsTheWait(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := installPriorityPool()
	writePendingInstall(t, tg, town)

	install := tg.mustAcquirePool(t, town, InstallRole, pool)
	defer release(t, install)

	h, err, elapsed := tg.run(t, func() (*Handle, error) { return tg.AcquirePool(town, "gastown/post-land", time.Minute, pool) })
	if err != nil {
		t.Fatal(err)
	}
	defer release(t, h)
	if elapsed != 0 {
		t.Fatalf("post-land suite waited %s while the install held slot %d", elapsed, install.Index)
	}
}

// TestInstallPriority_BoundedByMaxInstallYield: a marker no cycle clears parks
// a post-landing suite for at most MaxInstallYield; after that it competes for
// a slot as before, so a stuck install cannot starve the red-main detector
// (gt-t135z).
func TestInstallPriority_BoundedByMaxInstallYield(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := installPriorityPool()
	pool.MaxInstallYield = 4 * tg.pollInterval
	writePendingInstall(t, tg, town)

	h, err, elapsed := tg.run(t, func() (*Handle, error) { return tg.AcquirePool(town, "gastown/post-land", time.Hour, pool) })
	if err != nil {
		t.Fatalf("post-land acquire behind a marker that never clears: %v", err)
	}
	defer release(t, h)
	if elapsed != pool.MaxInstallYield {
		t.Fatalf("post-land waited %s, want exactly the yield cap %s", elapsed, pool.MaxInstallYield)
	}
	if h.Index != 0 {
		t.Fatalf("post-land suite got slot %d, want reserved slot 0", h.Index)
	}
	if !strings.Contains(tg.probe.String(), "yield cap") {
		t.Errorf("probe output = %q, want the capped yield announced", tg.probe.String())
	}
}

// TestInstallPriority_UnreadableMarkerIsAnInstallInFlight: a marker that cannot
// be read is not read as none — a caller that cannot tell must not start a
// suite beside an install — and its diagnosis reaches the pane once per wait
// (gt-t135z).
func TestInstallPriority_UnreadableMarkerIsAnInstallInFlight(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := installPriorityPool()
	dir := filepath.Join(town, "daemon")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	markerPath := filepath.Join(dir, installpending.FileName)
	if err := os.WriteFile(markerPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	done := goAcquire(func() (*Handle, error) { return tg.AcquirePool(town, "gastown/post-land", time.Hour, pool) })
	for i := 0; i < 2; i++ {
		waitBlocked(t, tg.clk)
		select {
		case got := <-done:
			t.Fatalf("post-land acquire returned past an unreadable marker: h=%+v err=%v", got.h, got.err)
		default:
		}
		tg.clk.Advance(tg.pollInterval)
	}
	out := tg.probe.String()
	if !strings.Contains(out, "install-pending marker unreadable") {
		t.Errorf("probe output = %q, want the unreadable marker reported", out)
	}
	if n := strings.Count(out, "install-pending marker unreadable"); n != 1 {
		t.Errorf("unreadable marker reported %d times, want once per wait:\n%s", n, out)
	}

	if err := os.Remove(markerPath); err != nil {
		t.Fatal(err)
	}
	got := driveClock(t, tg.clk, tg.pollInterval, done)
	if got.err != nil {
		t.Fatalf("post-land acquire after the unreadable marker went away: %v", got.err)
	}
	release(t, got.h)
}

// TestInstallPriority_DefaultCapApplies: MaxInstallYield <= 0 means
// DefaultMaxInstallYield, which is where the install job's own starvation
// escalation fires.
func TestInstallPriority_DefaultCapApplies(t *testing.T) {
	t.Parallel()
	if got := (Pool{Slots: 4}).normalized().MaxInstallYield; got != DefaultMaxInstallYield {
		t.Fatalf("normalized MaxInstallYield = %s, want %s", got, DefaultMaxInstallYield)
	}
	if DefaultMaxInstallYield != 30*time.Minute {
		t.Fatalf("DefaultMaxInstallYield = %s, want the install job's 30m starvation cliff", DefaultMaxInstallYield)
	}
}
