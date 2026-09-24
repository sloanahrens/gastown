package beads

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// useTestContainerInitSlots swaps this process's init pool for one of
// capacity n for the length of one test and returns it. The pool is process
// state, so a caller must not be t.Parallel.
func useTestContainerInitSlots(t *testing.T, n int) *initSlots {
	t.Helper()
	s := newInitSlots(n)
	prev := testContainerInitSlots.Swap(s)
	t.Cleanup(func() { testContainerInitSlots.Store(prev) })
	return s
}

// TestProcessInitPoolStartsFull pins the property both failed gt-elvf4 patches
// lacked: the channel holds its tokens before anyone acquires, so the first
// acquire returns instead of blocking forever.
func TestProcessInitPoolStartsFull(t *testing.T) {
	s := testContainerInitSlots.Load()
	if s == nil {
		t.Fatal("process init pool is nil; package init did not build it")
	}
	if cap(s.tokens) < 1 {
		t.Fatalf("process init pool capacity = %d, want >= 1", cap(s.tokens))
	}
	if len(s.tokens) != cap(s.tokens) {
		t.Fatalf("process init pool holds %d of %d tokens at rest, want all of them", len(s.tokens), cap(s.tokens))
	}
}

func TestParseTestDoltInitConcurrency(t *testing.T) {
	tests := []struct {
		raw      string
		want     int
		wantWarn bool
	}{
		{raw: "", want: 4},
		{raw: "1", want: 1},
		{raw: "8", want: 8},
		{raw: " 2 ", want: 2},
		{raw: "64", want: 64}, // no upper clamp
		{raw: "0", want: 4, wantWarn: true},
		{raw: "-3", want: 4, wantWarn: true},
		{raw: "abc", want: 4, wantWarn: true},
		{raw: "2.5", want: 4, wantWarn: true},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%q", tt.raw), func(t *testing.T) {
			got, warning := parseTestDoltInitConcurrency(tt.raw)
			if got != tt.want {
				t.Errorf("parseTestDoltInitConcurrency(%q) = %d, want %d", tt.raw, got, tt.want)
			}
			if !tt.wantWarn {
				if warning != "" {
					t.Errorf("parseTestDoltInitConcurrency(%q) warned %q, want no warning", tt.raw, warning)
				}
				return
			}
			if !strings.Contains(warning, testDoltInitConcurrencyEnv) || !strings.Contains(warning, "using 4") {
				t.Errorf("warning %q should name %s and the fallback", warning, testDoltInitConcurrencyEnv)
			}
			if strings.Contains(warning, "\n") {
				t.Errorf("warning %q should be one line", warning)
			}
		})
	}
}

func TestInitSlotsAdmitCapacityThenBlock(t *testing.T) {
	s := newInitSlots(2)
	var held []func()
	for i := 0; i < 2; i++ {
		release, err := s.acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire %d of 2: %v", i+1, err)
		}
		held = append(held, release)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := s.acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("third acquire on a full pool = %v, want a deadline error", err)
	}

	got := make(chan error, 1)
	go func() {
		release, err := s.acquire(context.Background())
		if err == nil {
			release()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("a waiter got through a full pool (err=%v)", err)
	case <-time.After(100 * time.Millisecond):
	}
	held[0]()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("waiter after a release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never received the released slot")
	}
	held[1]()
	if len(s.tokens) != 2 {
		t.Fatalf("pool holds %d tokens after every release, want 2", len(s.tokens))
	}
}

// TestInitSlotsFourHoldersTwelveWaiters is the no-deadlock pin: run it under
// -race. Four holders fill the pool, twelve waiters queue, none gets through
// until a holder lets go, and then every one finishes with never more than
// four in flight.
func TestInitSlotsFourHoldersTwelveWaiters(t *testing.T) {
	const holders, waiters = 4, 12
	s := newInitSlots(holders)
	held := make([]func(), 0, holders)
	for i := 0; i < holders; i++ {
		release, err := s.acquire(context.Background())
		if err != nil {
			t.Fatalf("holder %d: %v", i+1, err)
		}
		held = append(held, release)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var admitted, inFlight, peak atomic.Int32
	var wg sync.WaitGroup
	errs := make(chan error, waiters)
	for i := 0; i < waiters; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := s.acquire(ctx)
			if err != nil {
				errs <- err
				return
			}
			defer release()
			admitted.Add(1)
			n := inFlight.Add(1)
			for {
				p := peak.Load()
				if n <= p || peak.CompareAndSwap(p, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			inFlight.Add(-1)
		}()
	}

	time.Sleep(100 * time.Millisecond)
	if n := admitted.Load(); n != 0 {
		t.Fatalf("%d waiters got through while all %d slots were held", n, holders)
	}
	for _, release := range held {
		release()
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("waiters did not all finish: the pool deadlocked")
	}
	close(errs)
	for err := range errs {
		t.Errorf("waiter failed: %v", err)
	}
	if n := admitted.Load(); n != waiters {
		t.Errorf("admitted %d waiters, want %d", n, waiters)
	}
	if p := peak.Load(); p > holders {
		t.Errorf("peak in flight = %d, want <= %d", p, holders)
	}
	if len(s.tokens) != holders {
		t.Errorf("pool holds %d tokens after every release, want %d", len(s.tokens), holders)
	}
}

func TestInitSlotsCancelledContextTakesNoToken(t *testing.T) {
	s := newInitSlots(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.acquire(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("acquire with a cancelled context = %v, want context.Canceled", err)
	}
	if err.Error() != "test Dolt init slot: context canceled" {
		t.Errorf("error = %q, want it to name the slot", err.Error())
	}
	if len(s.tokens) != 1 {
		t.Errorf("a failed acquire consumed a token: %d left, want 1", len(s.tokens))
	}
}

func TestInitSlotsReleaseIsIdempotent(t *testing.T) {
	s := newInitSlots(1)
	release, err := s.acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	release()
	release()
	if len(s.tokens) != 1 {
		t.Fatalf("pool holds %d tokens after a double release, want 1", len(s.tokens))
	}
	second, err := s.acquire(context.Background())
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	defer second()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a double release grew the pool: second concurrent acquire = %v, want a deadline error", err)
	}
}

func TestInitSlotsPanicReleases(t *testing.T) {
	s := newInitSlots(1)
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected the panic to propagate to this recover")
			}
		}()
		release, err := s.acquire(context.Background())
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		defer release()
		panic("bd init blew up")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := s.acquire(ctx)
	if err != nil {
		t.Fatalf("slot not returned after a panic: %v", err)
	}
	release()
}

func TestAcquireTestContainerInitSlotUsesProcessPool(t *testing.T) {
	s := useTestContainerInitSlots(t, 1)
	release, err := AcquireTestContainerInitSlot(context.Background())
	if err != nil {
		t.Fatalf("AcquireTestContainerInitSlot: %v", err)
	}
	if len(s.tokens) != 0 {
		t.Fatalf("process pool holds %d tokens while one is taken, want 0", len(s.tokens))
	}
	release()
	if len(s.tokens) != 1 {
		t.Fatalf("process pool holds %d tokens after release, want 1", len(s.tokens))
	}
}

func TestTestContainerEnv(t *testing.T) {
	got := testContainerEnv()
	if len(got) != 1 || got[0] != "BD_ALLOW_REMOTE_MIGRATE=1" {
		t.Fatalf("testContainerEnv() = %q, want [BD_ALLOW_REMOTE_MIGRATE=1]", got)
	}
}

// TestTestContainerEnvOnlyOnTestContainerCalls pins decision 2: the env
// escape hatch reaches bd only from a wrapper aimed at the test Dolt
// container, once, whatever the parent process had set.
func TestTestContainerEnvOnlyOnTestContainerCalls(t *testing.T) {
	t.Setenv(allowRemoteMigrateEnv, "0") // inherited: must be replaced, not duplicated, on container calls
	dir := t.TempDir()

	container := NewIsolatedWithPort(dir, 45678)
	for name, env := range map[string][]string{
		"run":     container.buildRunEnv(),
		"routing": container.buildRoutingEnv(),
	} {
		if got := countEnvPrefix(env, allowRemoteMigrateEnv+"="); got != 1 {
			t.Errorf("container %s env has %d %s entries, want 1", name, got, allowRemoteMigrateEnv)
		}
		if !containsEnv(env, allowRemoteMigrateEnv+"=1") {
			t.Errorf("container %s env lacks %s=1", name, allowRemoteMigrateEnv)
		}
	}

	for _, tc := range []struct {
		name string
		b    *Beads
	}{
		{name: "isolated without a port", b: NewIsolated(dir)},
		{name: "real town", b: New(dir)},
		{name: "real town with beads dir", b: NewWithBeadsDir(dir, filepath.Join(dir, ".beads"))},
	} {
		for name, env := range map[string][]string{
			"run":     tc.b.buildRunEnv(),
			"routing": tc.b.buildRoutingEnv(),
		} {
			if containsEnv(env, allowRemoteMigrateEnv+"=1") {
				t.Errorf("%s %s env carries %s=1; only test-container calls may", tc.name, name, allowRemoteMigrateEnv)
			}
		}
	}
}

// installSucceedingBdStub is a fake bd that answers the --allow-stale probe,
// counts and records every other call, and succeeds.
func installSucceedingBdStub(t *testing.T) *flakyBdStub {
	t.Helper()
	return installBdStub(t, `#!/bin/sh
if [ "${1:-}" = "--allow-stale" ] && [ "${2:-}" = "version" ]; then
  echo "Error: unknown flag: --allow-stale" >&2
  exit 0
fi
count=0
[ -f __COUNT__ ] && count=$(cat __COUNT__)
count=$((count + 1))
echo "$count" > __COUNT__
echo "$*" >> __ARGS__
mkdir -p .beads
echo "initialized"
exit 0
`)
}

// readBdCallCount reads a stub's counter without failing on a torn read: the
// stub may be mid-write while a test polls it.
func readBdCallCount(stub *flakyBdStub) int {
	raw, err := os.ReadFile(stub.countFile)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0
	}
	return n
}

// shortenInitSlotWait collapses the slot-wait budget for one test.
func shortenInitSlotWait(t *testing.T, d time.Duration) {
	t.Helper()
	prev := testContainerInitSlotWait
	testContainerInitSlotWait = d
	t.Cleanup(func() { testContainerInitSlotWait = prev })
}

func TestInitWaitsForATestContainerInitSlot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	slots := useTestContainerInitSlots(t, 1)
	stub := installSucceedingBdStub(t)
	b := NewIsolatedWithPort(t.TempDir(), 45678)

	release, err := slots.acquire(context.Background())
	if err != nil {
		t.Fatalf("take the only slot: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- b.Init("gt") }()

	time.Sleep(300 * time.Millisecond)
	if n := readBdCallCount(stub); n != 0 {
		t.Fatalf("Init ran bd %d times while the only slot was held", n)
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Init after the slot freed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Init never proceeded after the slot freed")
	}
	if n := stub.calls(t); n != 1 {
		t.Fatalf("bd init calls = %d, want 1", n)
	}
	if len(slots.tokens) != 1 {
		t.Fatalf("Init kept its slot: pool holds %d tokens, want 1", len(slots.tokens))
	}
}

func TestInitSlotWaitIsBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	slots := useTestContainerInitSlots(t, 1)
	shortenInitSlotWait(t, 100*time.Millisecond)
	stub := installSucceedingBdStub(t)

	release, err := slots.acquire(context.Background())
	if err != nil {
		t.Fatalf("take the only slot: %v", err)
	}
	defer release()

	err = NewIsolatedWithPort(t.TempDir(), 45678).Init("gt")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Init with no free slot = %v, want a deadline error", err)
	}
	if !strings.Contains(err.Error(), "test Dolt init slot") {
		t.Errorf("error %q should name the slot", err)
	}
	if n := stub.calls(t); n != 0 {
		t.Errorf("bd ran %d times without a slot", n)
	}
}

// TestInitOutsideTheContainerTakesNoSlot pins that only test-container inits
// queue: the only slot is held and an isolated, port-less Init still runs.
func TestInitOutsideTheContainerTakesNoSlot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	slots := useTestContainerInitSlots(t, 1)
	shortenInitSlotWait(t, 100*time.Millisecond)
	stub := installSucceedingBdStub(t)
	release, err := slots.acquire(context.Background())
	if err != nil {
		t.Fatalf("take the only slot: %v", err)
	}
	defer release()

	if err := NewIsolated(t.TempDir()).Init("gt"); err != nil {
		t.Fatalf("port-less Init: %v", err)
	}
	if n := stub.calls(t); n != 1 {
		t.Fatalf("bd init calls = %d, want 1", n)
	}
}

// TestInitRetriesInsideOneSlot pins that the slot wraps the whole gt-o8i9f
// retry loop. With one slot, an init that fails twice on the catalog race and
// then succeeds must finish: a per-attempt acquire nested inside Init's would
// deadlock here, and a released-between-attempts slot would show as tokens.
func TestInitRetriesInsideOneSlot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	slots := useTestContainerInitSlots(t, 1)
	shortenInitSlotWait(t, 2*time.Second)
	zeroRetryBackoff(t)
	stub := installFlakyCatalogRaceBDStub(t, 2)

	done := make(chan error, 1)
	go func() { done <- NewIsolatedWithPort(t.TempDir(), 45678).Init("gt") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Init with two retried failures: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Init did not finish: the retry loop re-acquired a slot it already held")
	}
	if n := stub.calls(t); n != 3 {
		t.Fatalf("bd init calls = %d, want 3 (two failures, one success)", n)
	}
	if len(slots.tokens) != 1 {
		t.Fatalf("pool holds %d tokens after Init, want 1", len(slots.tokens))
	}
}

// waitForBdCalls polls a stub's counter until it reaches want.
func waitForBdCalls(t *testing.T, stub *flakyBdStub, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if readBdCallCount(stub) >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("bd stub reached %d calls, want %d", readBdCallCount(stub), want)
}

func TestRunTestContainerInitHoldsASlotAndSetsEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	useTestContainerInitSlots(t, 1)
	gate := filepath.Join(t.TempDir(), "open")
	stub := installBdStub(t, `#!/bin/sh
count=0
[ -f __COUNT__ ] && count=$(cat __COUNT__)
count=$((count + 1))
echo "$count" > __COUNT__
echo "$* BD_ALLOW_REMOTE_MIGRATE=${BD_ALLOW_REMOTE_MIGRATE:-unset}" >> __ARGS__
while [ ! -f '`+gate+`' ]; do sleep 0.05; done
echo "initialized"
exit 0
`)
	args := []string{"init", "--quiet", "--prefix", "rt", "--server", "--server-port", "45678"}
	type result struct {
		out []byte
		err error
	}
	run := func(env []string) chan result {
		ch := make(chan result, 1)
		dir := t.TempDir()
		go func() {
			out, err := RunTestContainerInit(context.Background(), dir, args, env)
			ch <- result{out, err}
		}()
		return ch
	}

	first := run(nil)
	waitForBdCalls(t, stub, 1)
	// An inherited value must be replaced, not passed through.
	second := run(append(os.Environ(), allowRemoteMigrateEnv+"=0"))
	time.Sleep(300 * time.Millisecond)
	if n := readBdCallCount(stub); n != 1 {
		t.Fatalf("second init ran while the only slot was held: %d bd calls", n)
	}
	if err := os.WriteFile(gate, nil, 0o644); err != nil {
		t.Fatalf("open the gate: %v", err)
	}
	for i, ch := range []chan result{first, second} {
		select {
		case r := <-ch:
			if r.err != nil {
				t.Fatalf("init %d: %v\n%s", i+1, r.err, r.out)
			}
			if !strings.Contains(string(r.out), "initialized") {
				t.Errorf("init %d output = %q, want bd's combined output", i+1, r.out)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("init %d did not finish after the gate opened", i+1)
		}
	}
	invocations := stub.invocations(t)
	if len(invocations) != 2 {
		t.Fatalf("bd invocations = %d, want 2", len(invocations))
	}
	for i, inv := range invocations {
		if inv[0] != "init" {
			t.Errorf("invocation %d argv = %q, want bd init", i+1, inv)
		}
		if last := inv[len(inv)-1]; last != "BD_ALLOW_REMOTE_MIGRATE=1" {
			t.Errorf("invocation %d saw %s, want BD_ALLOW_REMOTE_MIGRATE=1", i+1, last)
		}
	}
}

func TestRunTestContainerInitWaitIsBounded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	slots := useTestContainerInitSlots(t, 1)
	shortenInitSlotWait(t, 100*time.Millisecond)
	stub := installSucceedingBdStub(t)
	release, err := slots.acquire(context.Background())
	if err != nil {
		t.Fatalf("take the only slot: %v", err)
	}
	defer release()

	_, err = RunTestContainerInit(context.Background(), t.TempDir(), []string{"init", "--quiet"}, nil)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "test Dolt init slot") {
		t.Fatalf("RunTestContainerInit with no free slot = %v, want a slot deadline error", err)
	}
	if n := stub.calls(t); n != 0 {
		t.Errorf("bd ran %d times without a slot", n)
	}
}

func TestRunTestContainerInitRejectsNonInit(t *testing.T) {
	useTestContainerInitSlots(t, 1)
	for _, args := range [][]string{nil, {"list", "--json"}} {
		if _, err := RunTestContainerInit(context.Background(), t.TempDir(), args, nil); err == nil {
			t.Errorf("RunTestContainerInit(%q) succeeded, want a refusal", args)
		}
	}
}
