// Package abuse meters a workspace's dispatches and gateway requests with a
// token bucket per workspace and per kind.
//
// A dispatch is refused while the workspace's spend stands at or over its cap,
// whatever the bucket holds; a gateway request is metered by tokens alone. The
// cap is the Level the spend-cap package reports, so a caller that never read
// the cap passes the zero Level and its dispatch is refused rather than let
// past (plan row S5, gt-29ca2).
//
// The clock is injected and no goroutine is started, so a Limiter is safe for
// one goroutine at a time; a caller sharing one across goroutines needs its own
// lock. Nothing wires the limiter yet: the dispatcher and the gateway do, in
// later beads.
package abuse

import (
	"math"
	"time"

	"github.com/steveyegge/gastown/internal/spendcap"
)

// Kind is the sort of request a Limiter meters.
type Kind string

const (
	// Dispatch is a request to start work in a workspace.
	Dispatch Kind = "dispatch"
	// GatewayRequest is a request through the gateway.
	GatewayRequest Kind = "gateway"
)

// The reasons a Verdict carries for a refused request.
const (
	reasonUnknownKind = "unknown kind"
	reasonSpendCap    = "spend cap"
	reasonRate        = "rate"
)

// tokenEpsilon is how far a token count may fall short of one and still spend,
// because a refill computed as elapsed seconds times a rate can land a few
// billionths under the token it should have reached.
const tokenEpsilon = 1e-9

// Config gives each kind its fill rate in tokens per second and the most tokens
// a bucket may hold.
type Config struct {
	// DispatchRate is dispatch tokens added per second, and DispatchBurst the
	// most a workspace's dispatch bucket may hold.
	DispatchRate  float64
	DispatchBurst int
	// GatewayRate and GatewayBurst do the same for gateway requests.
	GatewayRate  float64
	GatewayBurst int
	// IdleTTL is how long a bucket must sit idle holding its burst before the
	// sweeper may drop it; zero or less keeps buckets for good.
	IdleTTL time.Duration
}

// Clock supplies the current time.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
}

// Verdict is Allow's answer: whether the request may proceed, and which limit
// refused it when it may not.
type Verdict struct {
	// Allowed reports whether the request may proceed.
	Allowed bool
	// Reason names the limit that refused the request, and is empty when
	// Allowed is true.
	Reason string
}

// Limiter holds one token bucket per workspace and kind.
type Limiter struct {
	cfg       Config
	clock     Clock
	buckets   map[bucketKey]*bucket
	lastSweep time.Time
}

// bucketKey keeps one workspace's dispatch tokens from paying for another's
// gateway request.
type bucketKey struct {
	kind      Kind
	workspace string
}

// bucket is the tokens a workspace and kind have left and when they were last
// asked for.
type bucket struct {
	tokens   float64
	lastSeen time.Time
}

// New returns a Limiter with no tokens spent, reading time from clock.
func New(cfg Config, clock Clock) *Limiter {
	if clock == nil {
		panic("abuse: New called with a nil Clock")
	}
	return &Limiter{
		cfg:       cfg,
		clock:     clock,
		buckets:   make(map[bucketKey]*bucket),
		lastSweep: clock.Now(),
	}
}

// Allow spends one token for a request of kind from workspace's bucket, and
// refuses a dispatch whose spend-cap level is not Under.
func (l *Limiter) Allow(kind Kind, workspace string, capLevel spendcap.Level) Verdict {
	rate, burst, ok := l.limits(kind)
	if !ok {
		// A kind with no configured rate would otherwise meter nothing, which
		// fails open on a typo at the call site.
		return Verdict{Reason: reasonUnknownKind}
	}
	// The cap check precedes the bucket so a refused dispatch spends no token:
	// lifting the cap then finds the allowance the refusal never used.
	if kind == Dispatch && capLevel != spendcap.Under {
		return Verdict{Reason: reasonSpendCap}
	}
	now := l.clock.Now()
	l.evict(now)
	key := bucketKey{kind: kind, workspace: workspace}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(burst), lastSeen: now}
		l.buckets[key] = b
	} else {
		b.refill(now, rate, float64(burst))
	}
	if b.tokens < 1-tokenEpsilon {
		return Verdict{Reason: reasonRate}
	}
	b.tokens = math.Max(0, b.tokens-1)
	return Verdict{Allowed: true}
}

// limits returns kind's fill rate and burst, and false for a kind the Config
// does not know.
func (l *Limiter) limits(kind Kind) (rate float64, burst int, ok bool) {
	switch kind {
	case Dispatch:
		return l.cfg.DispatchRate, l.cfg.DispatchBurst, true
	case GatewayRequest:
		return l.cfg.GatewayRate, l.cfg.GatewayBurst, true
	}
	return 0, 0, false
}

// evict drops buckets that sat idle past IdleTTL and would hold their full
// burst by now. Such a bucket is what a fresh one would be, so dropping it
// changes nothing a caller could spend; dropping one that has not refilled
// would hand back tokens the rate withheld, so the burst check is what keeps
// eviction from being a bypass.
//
// The scan runs at most once per IdleTTL because no bucket can pass the idle
// age sooner, so a busy workspace never pays a full scan per request.
func (l *Limiter) evict(now time.Time) {
	ttl := l.cfg.IdleTTL
	if ttl <= 0 || now.Sub(l.lastSweep) < ttl {
		return
	}
	l.lastSweep = now
	for key, b := range l.buckets {
		rate, burst, ok := l.limits(key.kind)
		if !ok {
			continue
		}
		if now.Sub(b.lastSeen) > ttl && b.tokensAt(now, rate, float64(burst)) >= float64(burst) {
			delete(l.buckets, key)
		}
	}
}

// tokensAt is the tokens the bucket would hold at now without moving it: the
// stored count plus what elapsed at rate, never past burst. A clock that has
// not moved forward adds nothing, so a clock stepping back cannot drive the
// count negative or replay an interval.
func (b *bucket) tokensAt(now time.Time, rate, burst float64) float64 {
	if !now.After(b.lastSeen) {
		return b.tokens
	}
	return math.Min(burst, b.tokens+now.Sub(b.lastSeen).Seconds()*rate)
}

// refill is tokensAt plus the lastSeen advance, so two requests at one instant
// cannot spend the same interval twice.
func (b *bucket) refill(now time.Time, rate, burst float64) {
	b.tokens = b.tokensAt(now, rate, burst)
	if now.After(b.lastSeen) {
		b.lastSeen = now
	}
}
