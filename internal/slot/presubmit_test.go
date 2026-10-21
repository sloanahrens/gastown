package slot

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A third presubmit waits while two are held, and is granted the slot the
// moment one of the holders releases it.
func TestAcquirePresubmitThirdWaitsForRelease(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	const role = "gastown/polecats/alpha"

	first, err := tg.AcquirePresubmit(town, role, time.Minute, 2)
	if err != nil {
		t.Fatalf("first presubmit: %v", err)
	}
	second, err := tg.AcquirePresubmit(town, role, time.Minute, 2)
	if err != nil {
		t.Fatalf("second presubmit: %v", err)
	}

	third := goAcquire(func() (*Handle, error) {
		return tg.AcquirePresubmit(town, "gastown/polecats/beta", 10*time.Minute, 2)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := tg.clk.BlockUntilContext(ctx, 1); err != nil {
		t.Fatalf("third presubmit never blocked: %v", err)
	}
	select {
	case got := <-third:
		t.Fatalf("third presubmit granted while both slots were held: %+v", got)
	default:
	}

	if err := first.Release(); err != nil {
		t.Fatalf("release first: %v", err)
	}
	got := driveClock(t, tg.clk, tg.pollInterval, third)
	if got.err != nil {
		t.Fatalf("third presubmit: %v", got.err)
	}
	defer release(t, got.h)
	defer release(t, second)
	if got.h.Index != first.Index {
		t.Errorf("third presubmit got slot %d, want the released slot %d", got.h.Index, first.Index)
	}
	if got.h.WaitedFor <= 0 {
		t.Errorf("third presubmit WaitedFor = %s, want the time it waited", got.h.WaitedFor)
	}
}

// A wait past PresubmitWaitLogAfter is logged, and only once.
func TestAcquirePresubmitLogsLongWaitOnce(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()

	held, err := tg.AcquirePresubmit(town, "gastown/polecats/alpha", time.Minute, 1)
	if err != nil {
		t.Fatalf("holder: %v", err)
	}
	third := goAcquire(func() (*Handle, error) {
		return tg.AcquirePresubmit(town, "gastown/polecats/beta", 10*time.Minute, 1)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := tg.clk.BlockUntilContext(ctx, 1); err != nil {
		t.Fatalf("waiter never blocked: %v", err)
	}
	tg.clk.Advance(PresubmitWaitLogAfter + time.Second)
	if err := held.Release(); err != nil {
		t.Fatalf("release holder: %v", err)
	}
	got := driveClock(t, tg.clk, tg.pollInterval, third)
	if got.err != nil {
		t.Fatalf("waiter: %v", got.err)
	}
	defer release(t, got.h)

	if n := strings.Count(tg.probe.String(), "for a presubmit slot"); n != 1 {
		t.Errorf("long-wait line logged %d times, want once; output:\n%s", n, tg.probe.String())
	}
}

// A wait counts against the caller's timeout: a held pool times out at the
// cap the caller gave, not after some other default.
func TestAcquirePresubmitTimesOutAtCap(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()

	held, err := tg.AcquirePresubmit(town, "gastown/polecats/alpha", time.Minute, 1)
	if err != nil {
		t.Fatalf("holder: %v", err)
	}
	defer release(t, held)

	const cap = 10 * time.Second
	got, err, elapsed := tg.run(t, func() (*Handle, error) {
		return tg.AcquirePresubmit(town, "gastown/polecats/beta", cap, 1)
	})
	if err == nil {
		got.Release()
		t.Fatalf("presubmit granted a held pool: %+v", got)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error %q does not name the timeout", err)
	}
	if elapsed < cap || elapsed > cap+tg.pollInterval {
		t.Errorf("waited %s, want about the %s cap", elapsed, cap)
	}
}

// A presubmit holds its own pool: it never takes a container-gate slot, and a
// container-gate acquire is not blocked by presubmits holding every presubmit
// slot.
func TestPresubmitDoesNotTakeContainerGateSlot(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()

	held, err := tg.AcquirePresubmit(town, "gastown/polecats/alpha", time.Minute, 2)
	if err != nil {
		t.Fatalf("presubmit: %v", err)
	}
	defer release(t, held)

	rep, err := StatusPoolLocksOnly(town, Pool{Slots: 1})
	if err != nil {
		t.Fatalf("StatusPoolLocksOnly: %v", err)
	}
	if rep.HeldCount != 0 {
		t.Errorf("container-gate pool reports %d held while only presubmits run", rep.HeldCount)
	}

	landing := tg.mustAcquirePool(t, town, "gastown/landing", Pool{Slots: 1})
	release(t, landing)
}

// StatusPresubmit names each held slot's holder and frees the slot on release.
func TestStatusPresubmitReportsHolders(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	const role = "gastown/polecats/alpha"

	rep, err := StatusPresubmit(town, 2)
	if err != nil {
		t.Fatalf("StatusPresubmit: %v", err)
	}
	if rep.HeldCount != 0 || rep.Total != 2 {
		t.Fatalf("idle pool: held=%d total=%d, want 0 of 2", rep.HeldCount, rep.Total)
	}

	h, err := tg.AcquirePresubmit(town, role, time.Minute, 2)
	if err != nil {
		t.Fatalf("presubmit: %v", err)
	}
	rep, err = StatusPresubmit(town, 2)
	if err != nil {
		t.Fatalf("StatusPresubmit while held: %v", err)
	}
	if rep.HeldCount != 1 || !rep.Held {
		t.Fatalf("one held: held=%d, want 1", rep.HeldCount)
	}
	if rep.Owner == nil || rep.Owner.Role != role {
		t.Errorf("owner = %+v, want role %q", rep.Owner, role)
	}
	if !rep.Slots[h.Index].Held || rep.Slots[h.Index].Owner == nil {
		t.Errorf("slot %d row = %+v, want held with an owner", h.Index, rep.Slots[h.Index])
	}

	release(t, h)
	rep, err = StatusPresubmit(town, 2)
	if err != nil {
		t.Fatalf("StatusPresubmit after release: %v", err)
	}
	if rep.HeldCount != 0 {
		t.Errorf("after release held=%d, want 0", rep.HeldCount)
	}
}
