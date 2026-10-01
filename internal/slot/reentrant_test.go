package slot

import (
	"maps"
	"testing"
)

// TestReentrant_NestedGateWrapRidesTheHold is the gt-tuiy/gt-off9 nesting
// end to end through forks: a landing gate holds a slot, the verify suite it
// spawns runs `gt slot run` with no --role, and that wrap's own child
// acquires again. Each level resolves the ancestor's role from what it
// inherited, takes the reentrant fast path without waiting, and leaves the
// real hold in force; the holder's own process still contends.
func TestReentrant_NestedGateWrapRidesTheHold(t *testing.T) {
	t.Parallel()
	tg := newTestGate(t)
	town := t.TempDir()
	pool := yieldPool()
	const role = "gastown/landing"

	gate := tg.mustAcquirePool(t, town, role, pool)
	defer release(t, gate)

	verify := tg.child()
	inherited, ok := verify.InheritedRole(town)
	if !ok || inherited != role {
		t.Fatalf("InheritedRole under the gate = (%q, %v), want (%q, true)", inherited, ok, role)
	}
	if !verify.underGateHold(town) {
		t.Fatalf("a child of a live gate hold must see itself under that hold")
	}
	wrap, err, elapsed := tg.run(t, func() (*Handle, error) { return verify.AcquirePool(town, inherited, shortWait, pool) })
	if err != nil {
		t.Fatalf("nested wrap: %v", err)
	}
	if !wrap.reentrant || elapsed != 0 {
		t.Fatalf("nested wrap: reentrant=%v after %s, want the immediate fast path", wrap.reentrant, elapsed)
	}

	// The wrap's child inherits the same marker, unchanged by the wrap's
	// reentrant hold.
	suite := *verify
	suite.env = verify.env.(*mapEnv).clone()
	suite.pid = verify.pid + 1
	inner, err, elapsed := tg.run(t, func() (*Handle, error) { return suite.AcquirePool(town, role, shortWait, pool) })
	if err != nil {
		t.Fatalf("suite under the wrap: %v", err)
	}
	if !inner.reentrant || elapsed != 0 {
		t.Fatalf("suite under the wrap: reentrant=%v after %s, want the immediate fast path", inner.reentrant, elapsed)
	}
	release(t, inner)
	release(t, wrap)

	// The holder's own process is not its own descendant.
	sibling := tg.mustAcquirePool(t, town, role, pool)
	defer release(t, sibling)
	if sibling.reentrant || sibling.Index == gate.Index {
		t.Fatalf("a second acquire in the holder's process rode its own marker: %+v", sibling)
	}
	if rep, _ := tg.StatusPool(town, pool); rep.HeldCount != 2 {
		t.Fatalf("nested reentrant releases must leave the real holds: %+v", rep)
	}
}

// TestReentrant_MarkerTestsReadIsStable (gt-22hdp.56): `go test` keys a
// cached result on the value of every variable the test binary read. A test
// binary run under `gt slot run` works in a sandbox town, so what it reads of
// the inherited marker must be the same for every hold of the real town,
// whatever its pid, role or slot; otherwise every run is a cache miss.
func TestReentrant_MarkerTestsReadIsStable(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	pool := Pool{Slots: 2}

	first := newTestGate(t)
	h := first.mustAcquirePool(t, town, "pid-111", pool)
	firstVal := first.env.Getenv(ReentrantEnvVar)
	release(t, h)

	second := newTestGate(t)
	second.pid = foreignPID()
	other := second.mustAcquirePool(t, town, "gastown/a", pool)
	defer release(t, other)
	h = second.mustAcquirePool(t, town, "pid-222", pool)
	defer release(t, h)
	if h.Index == 0 {
		t.Fatalf("second hold landed on slot 0; the test needs a different slot")
	}
	if got := second.env.Getenv(ReentrantEnvVar); got != firstVal || got == "" {
		t.Fatalf("%s differs between holds: %q then %q", ReentrantEnvVar, firstVal, got)
	}

	test := second.child()
	sandbox := t.TempDir()
	th, err, _ := second.run(t, func() (*Handle, error) { return test.AcquirePool(sandbox, "gastown/crew", shortWait, pool) })
	if err != nil {
		t.Fatalf("acquire in the sandbox town: %v", err)
	}
	release(t, th)
	if got, want := test.env.(*mapEnv).readKeys(), map[string]bool{ReentrantEnvVar: true}; !maps.Equal(got, want) {
		t.Fatalf("a sandbox-town acquire read %v, want only the stable %s", got, ReentrantEnvVar)
	}
}
