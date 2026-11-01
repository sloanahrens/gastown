package abuse

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/spendcap"
)

// Each case builds its own Limiter and clock, so cases run in parallel; the
// Limiter itself is safe for one goroutine at a time, so no case shares one.

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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	clock := newFakeClock()
	l := New(Config{DispatchRate: 1, DispatchBurst: 1}, clock)

	if v := l.Allow(Dispatch, "acme", spendcap.Under); !v.Allowed {
		t.Fatalf("the first request: refused (%s), want allowed", v.Reason)
	}
	clock.Advance(100 * time.Second)
	// A second workspace asks now, which is what would trigger a sweep; with
	// IdleTTL 0 there is none, so acme's bucket is still counted.
	if v := l.Allow(Dispatch, "other", spendcap.Under); !v.Allowed {
		t.Fatalf("the second workspace's request: refused (%s), want allowed", v.Reason)
	}
	if got := l.Len(); got != 2 {
		t.Fatalf("Len = %d with IdleTTL 0, want 2: eviction disabled so the zero Config keeps buckets", got)
	}
}

func TestWorkspacesDoNotShareTokens(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	clock := newFakeClock()
	l := New(Config{DispatchRate: 1, DispatchBurst: 1, IdleTTL: time.Second}, clock)

	if v := l.Allow(Dispatch, "idle", spendcap.Under); !v.Allowed {
		t.Fatalf("the idle workspace's request: refused (%s), want allowed", v.Reason)
	}
	clock.Advance(3 * time.Second)
	if v := l.Allow(Dispatch, "active", spendcap.Under); !v.Allowed {
		t.Fatalf("the active workspace's request: refused (%s), want allowed", v.Reason)
	}
	if got := l.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1: the idle workspace's bucket evicted past IdleTTL, only the active one held", got)
	}
}

func TestEvictionNeverGrantsMoreTokens(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// The sweep is observed through Len: a bucket idle past IdleTTL is still held
// until the next scan is due, and gone once it runs.
func TestSweepRunsAtMostOncePerIdleAge(t *testing.T) {
	t.Parallel()
	const ttl = 10 * time.Second
	clock := newFakeClock()
	l := New(Config{DispatchRate: 100, DispatchBurst: 100, IdleTTL: ttl}, clock)
	ask := func(workspace string) {
		t.Helper()
		if v := l.Allow(Dispatch, workspace, spendcap.Under); !v.Allowed {
			t.Fatalf("%s: refused (%s), want allowed", workspace, v.Reason)
		}
	}

	ask("a")
	clock.Advance(ttl)
	ask("b") // a scan is due and runs; a has been idle exactly ttl, not past it
	if got := l.Len(); got != 2 {
		t.Fatalf("after the first scan: Len = %d, want 2 (a not yet past the idle age)", got)
	}
	clock.Advance(ttl - time.Second)
	ask("c") // a is now past the idle age, but the next scan is a second away
	if got := l.Len(); got != 3 {
		t.Fatalf("a scan ran within one idle age of the last: Len = %d, want 3 (a still held)", got)
	}
	clock.Advance(time.Second)
	ask("d") // the next scan runs: a goes, b has been idle exactly ttl and stays
	if got := l.Len(); got != 3 {
		t.Fatalf("after the second scan: Len = %d, want 3 (a evicted; b, c, d held)", got)
	}
}

// A clock that steps back past the last scan must not hold eviction off until
// it catches up: the map would grow meanwhile.
func TestBackwardClockStepDoesNotSuspendEviction(t *testing.T) {
	t.Parallel()
	const ttl = 10 * time.Second
	clock := newFakeClock()
	l := New(Config{DispatchRate: 100, DispatchBurst: 100, IdleTTL: ttl}, clock)

	if v := l.Allow(Dispatch, "idle", spendcap.Under); !v.Allowed {
		t.Fatalf("the idle workspace's request: refused (%s)", v.Reason)
	}
	clock.Advance(-100 * time.Second)
	for _, step := range []time.Duration{0, 6 * time.Second} { // "other" stays active after the step back
		clock.Advance(step)
		if v := l.Allow(Dispatch, "other", spendcap.Under); !v.Allowed {
			t.Fatalf("a request after the step back: refused (%s)", v.Reason)
		}
	}
	clock.Advance(5 * time.Second) // an idle age and a second after the step back
	if v := l.Allow(Dispatch, "third", spendcap.Under); !v.Allowed {
		t.Fatalf("a request an idle age after the step back: refused (%s)", v.Reason)
	}
	if got := l.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2: the workspace idle since the step back evicted, not held until the clock catches up", got)
	}
}

func TestUnknownKindRefused(t *testing.T) {
	t.Parallel()
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
