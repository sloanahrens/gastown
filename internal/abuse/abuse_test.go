package abuse

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/spendcap"
)

// No case calls t.Parallel: the Limiter is documented safe for one goroutine at
// a time, and subtests sharing one would race on its bucket map.

// fakeClock is the injected time the tests move by hand, so no case sleeps.
type fakeClock struct{ now time.Time }

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time { return c.now }

// Advance moves the clock by d; a negative d stands in for a clock that stepped
// back.
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

// spend asks for n dispatches in one workspace under an open cap, returning
// how many were allowed.
func spend(l *Limiter, workspace string, n int) (allowed int) {
	for i := 0; i < n; i++ {
		if l.Allow(Dispatch, workspace, spendcap.Under).Allowed {
			allowed++
		}
	}
	return allowed
}

func TestBurstThenRefillByClock(t *testing.T) {
	clock := newFakeClock()
	l := New(Config{DispatchRate: 1, DispatchBurst: 3}, clock)

	if got := spend(l, "acme", 3); got != 3 {
		t.Fatalf("the burst: %d of 3 allowed, want 3", got)
	}
	if v := l.Allow(Dispatch, "acme", spendcap.Under); v.Allowed {
		t.Fatalf("past the burst: allowed, want refused")
	} else if v.Reason != reasonRate {
		t.Fatalf("past the burst: reason %q, want %q", v.Reason, reasonRate)
	}

	clock.Advance(time.Second)
	if got := spend(l, "acme", 1); got != 1 {
		t.Fatalf("one refill interval later: %d of 1 allowed, want 1", got)
	}
	if v := l.Allow(Dispatch, "acme", spendcap.Under); v.Allowed {
		t.Fatalf("a second request on one refilled token: allowed, want refused")
	}
}

func TestRefillStopsAtBurst(t *testing.T) {
	clock := newFakeClock()
	l := New(Config{DispatchRate: 1, DispatchBurst: 2}, clock)

	if got := spend(l, "acme", 2); got != 2 {
		t.Fatalf("the burst: %d of 2 allowed, want 2", got)
	}
	// Far past the two seconds the burst takes to fill: an uncapped refill
	// would bank the surplus and allow more than the burst.
	clock.Advance(100 * time.Second)
	if got := spend(l, "acme", 3); got != 2 {
		t.Fatalf("after a long idle: %d of 3 allowed, want 2: the bucket holds no more than the burst", got)
	}
}

func TestZeroIdleTTLKeepsBuckets(t *testing.T) {
	clock := newFakeClock()
	l := New(Config{DispatchRate: 1, DispatchBurst: 1}, clock)

	if v := l.Allow(Dispatch, "acme", spendcap.Under); !v.Allowed {
		t.Fatalf("the first request: refused (%s), want allowed", v.Reason)
	}
	clock.Advance(100 * time.Second)
	// A second workspace asks now, which is what would trigger a sweep; with
	// IdleTTL 0 there is none, so acme's bucket is still there.
	if v := l.Allow(Dispatch, "other", spendcap.Under); !v.Allowed {
		t.Fatalf("the second workspace's request: refused (%s), want allowed", v.Reason)
	}
	if _, ok := l.buckets[bucketKey{kind: Dispatch, workspace: "acme"}]; !ok {
		t.Fatalf("acme's bucket is gone with IdleTTL 0: want eviction disabled so the zero Config keeps buckets")
	}
}

func TestWorkspacesDoNotShareTokens(t *testing.T) {
	clock := newFakeClock()
	l := New(Config{DispatchRate: 0, DispatchBurst: 1}, clock)

	if got := spend(l, "acme", 2); got != 1 {
		t.Fatalf("acme: %d of 2 allowed, want 1", got)
	}
	if v := l.Allow(Dispatch, "other", spendcap.Under); !v.Allowed {
		t.Fatalf("a second workspace: refused (%s), want allowed: it must not spend acme's bucket", v.Reason)
	}
}

func TestBucketsArePerKind(t *testing.T) {
	clock := newFakeClock()
	l := New(Config{DispatchBurst: 1, GatewayBurst: 1}, clock)

	if got := spend(l, "acme", 2); got != 1 {
		t.Fatalf("dispatch: %d of 2 allowed, want 1", got)
	}
	if v := l.Allow(GatewayRequest, "acme", spendcap.Under); !v.Allowed {
		t.Fatalf("a gateway request: refused (%s), want allowed: it must not spend the dispatch bucket", v.Reason)
	}
}

func TestCapAtOrOverRefusesDispatchOnly(t *testing.T) {
	cases := []struct {
		name         string
		level        spendcap.Level
		wantDispatch bool
	}{
		{name: "under the cap", level: spendcap.Under, wantDispatch: true},
		{name: "at the cap", level: spendcap.At},
		{name: "over the cap", level: spendcap.Over},
		{name: "cap never read", level: spendcap.Level("")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clock := newFakeClock()
			l := New(Config{DispatchBurst: 1, GatewayBurst: 1}, clock)

			v := l.Allow(Dispatch, "acme", c.level)
			if v.Allowed != c.wantDispatch {
				t.Fatalf("dispatch: allowed=%v, want %v", v.Allowed, c.wantDispatch)
			}
			if !v.Allowed && v.Reason != reasonSpendCap {
				t.Fatalf("dispatch: reason %q, want %q", v.Reason, reasonSpendCap)
			}
			if v := l.Allow(GatewayRequest, "acme", c.level); !v.Allowed {
				t.Fatalf("gateway request: refused (%s), want allowed: it is metered by tokens alone", v.Reason)
			}
		})
	}
}

func TestCapRefusalSpendsNoToken(t *testing.T) {
	clock := newFakeClock()
	l := New(Config{DispatchBurst: 5}, clock)

	for i := 0; i < 3; i++ {
		if v := l.Allow(Dispatch, "acme", spendcap.At); v.Allowed {
			t.Fatalf("dispatch %d at the cap: allowed, want refused", i+1)
		}
	}
	if got := spend(l, "acme", 5); got != 5 {
		t.Fatalf("under the cap: %d of 5 allowed, want 5: the refusals must spend no token", got)
	}
}

func TestIdleBucketsEvicted(t *testing.T) {
	clock := newFakeClock()
	l := New(Config{DispatchRate: 1, DispatchBurst: 1, IdleTTL: time.Second}, clock)

	if v := l.Allow(Dispatch, "idle", spendcap.Under); !v.Allowed {
		t.Fatalf("the idle workspace's request: refused (%s), want allowed", v.Reason)
	}
	clock.Advance(3 * time.Second)
	if v := l.Allow(Dispatch, "active", spendcap.Under); !v.Allowed {
		t.Fatalf("the active workspace's request: refused (%s), want allowed", v.Reason)
	}
	if _, ok := l.buckets[bucketKey{kind: Dispatch, workspace: "idle"}]; ok {
		t.Fatalf("the idle workspace's bucket is still in the map: want it evicted past IdleTTL")
	}
	if got := len(l.buckets); got != 1 {
		t.Fatalf("len(buckets) = %d, want 1: only the active workspace's bucket", got)
	}
}

func TestEvictionNeverGrantsMoreTokens(t *testing.T) {
	clock := newFakeClock()
	// The burst takes 100s to refill, far longer than IdleTTL: dropping the
	// bucket at IdleTTL would hand back ten tokens the rate has not granted.
	l := New(Config{DispatchRate: 0.1, DispatchBurst: 10, IdleTTL: time.Second}, clock)

	if got := spend(l, "acme", 10); got != 10 {
		t.Fatalf("the burst: %d of 10 allowed, want 10", got)
	}
	clock.Advance(15 * time.Second) // 1.5 tokens at 0.1/s, well short of the burst
	if got := spend(l, "acme", 1); got != 1 {
		t.Fatalf("after 1.5 refilled tokens: %d of 1 allowed, want 1", got)
	}
	if v := l.Allow(Dispatch, "acme", spendcap.Under); v.Allowed {
		t.Fatalf("a second request on the 1.5 tokens: allowed, want refused: the idle sweep must not refill the burst")
	}
}

func TestClockSteppingBackAddsNoTokens(t *testing.T) {
	const back = 100 * time.Second
	cases := []struct {
		name string
		// net is where the clock ends up, measured from the last request.
		net         time.Duration
		wantAllowed int
	}{
		{
			// Half a token has filled, so neither can be spent. A lastSeen
			// dragged back with the clock would count a hundred seconds that
			// never passed and allow the whole burst.
			name: "half an interval after the step back", net: 500 * time.Millisecond,
		},
		{
			// Eleven seconds have passed, enough for the burst of two. A count
			// driven negative by the step back would withhold it for another
			// eighty-nine seconds.
			name: "past the burst after the step back", net: 11 * time.Second, wantAllowed: 2,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clock := newFakeClock()
			l := New(Config{DispatchRate: 1, DispatchBurst: 2}, clock)

			if got := spend(l, "acme", 2); got != 2 {
				t.Fatalf("the burst: %d of 2 allowed, want 2", got)
			}
			clock.Advance(-back)
			if v := l.Allow(Dispatch, "acme", spendcap.Under); v.Allowed {
				t.Fatalf("a clock %v behind the last request: allowed, want refused: no interval elapsed", back)
			}
			clock.Advance(back + c.net)
			if got := spend(l, "acme", 3); got != c.wantAllowed {
				t.Fatalf("%v after the last request: %d of 3 allowed, want %d", c.net, got, c.wantAllowed)
			}
		})
	}
}

func TestSweepRunsOncePerIdleAge(t *testing.T) {
	clock := newFakeClock()
	l := New(Config{DispatchRate: 100, DispatchBurst: 100, IdleTTL: time.Second}, clock)

	first := clock.Now()
	if !l.lastSweep.Equal(first) {
		t.Fatalf("lastSweep = %v at construction, want %v", l.lastSweep, first)
	}
	for i := 0; i < 50; i++ {
		l.Allow(Dispatch, "acme", spendcap.Under)
	}
	if !l.lastSweep.Equal(first) {
		t.Fatalf("lastSweep moved to %v within the idle age, want it still %v: the sweep must not scan per request", l.lastSweep, first)
	}

	clock.Advance(time.Second)
	l.Allow(Dispatch, "other", spendcap.Under)
	swept := clock.Now()
	if !l.lastSweep.Equal(swept) {
		t.Fatalf("lastSweep = %v a full idle age after the last sweep, want %v", l.lastSweep, swept)
	}
	clock.Advance(time.Millisecond)
	for i := 0; i < 50; i++ {
		l.Allow(Dispatch, "acme", spendcap.Under)
	}
	if !l.lastSweep.Equal(swept) {
		t.Fatalf("lastSweep moved to %v within the idle age, want it still %v", l.lastSweep, swept)
	}
}

func TestUnknownKindRefused(t *testing.T) {
	clock := newFakeClock()
	l := New(Config{DispatchRate: 1, DispatchBurst: 1}, clock)

	for _, kind := range []Kind{"", "frobnicate"} {
		v := l.Allow(kind, "acme", spendcap.Under)
		if v.Allowed {
			t.Fatalf("kind %q: allowed, want refused", kind)
		}
		if v.Reason != reasonUnknownKind {
			t.Fatalf("kind %q: reason %q, want %q", kind, v.Reason, reasonUnknownKind)
		}
	}
}
