package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/steveyegge/gastown/internal/sandbox"
)

// ErrSeatNotLive is what a census read answers for a sandbox seat whose sandbox
// the gateway lists as absent, Stopped, Error or Failed. No tmux read is made
// for such a seat: its sandbox has no server to ask.
var ErrSeatNotLive = errors.New("sandbox seat is not live: its sandbox is absent from the gateway or not Ready")

// SandboxCensus is one sling process's read-once view of the sandbox seats. A
// sling asks the same questions of every seat in its seat decision and again in
// its admission, and each question was one openshell process per seat per read.
// The census asks the gateway for its sandbox list once per gateway, and answers
// a seat's read-only tmux question once per sling, so the later read reads the
// census.
//
// Only a Ready sandbox gets a tmux probe, and a probe that succeeded is kept. A
// probe that failed is not kept, and the next read asks again. A listing that
// failed is kept for the rest of the sling as "listing unavailable": every seat
// of that gateway then takes its per-seat probe, and the gateway is not asked
// again within the sling. A sling process installs one census
// (BeginSandboxCensus) and drops it when it ends; nothing carries from one sling
// into the next.
type SandboxCensus struct {
	townRoot string
	run      sandbox.Runner // nil runs the openshell binary

	// mu guards the two maps only. It is never held across a gateway call.
	mu       sync.Mutex
	listings map[string]*gatewayListing
	// answers holds the successful tmux reads, keyed by seatKey.
	answers map[string]any
}

// gatewayListing is one gateway's sandbox listing for this sling. The read that
// creates it fetches the listing with no lock held, then sets rows or err and
// closes done. Every other read of that gateway waits on done and shares the
// result, so concurrent seats make one gateway call.
type gatewayListing struct {
	done chan struct{}
	// rows maps each sandbox name to its phase. Valid once done is closed and
	// err is nil.
	rows map[string]string
	// err is why the listing cannot answer for any seat: the call failed, or
	// the reply named no sandboxes at all.
	err error
}

// NewSandboxCensus returns an empty census for the town's sandbox registry.
// run runs the openshell calls; nil runs the binary itself.
func NewSandboxCensus(townRoot string, run sandbox.Runner) *SandboxCensus {
	return &SandboxCensus{
		townRoot: townRoot,
		run:      run,
		listings: map[string]*gatewayListing{},
		answers:  map[string]any{},
	}
}

// seatKey names one read: the session, the verb and its arguments. The NUL
// separator cannot appear in a session name or a tmux argument.
func seatKey(session, verb string, args []string) string {
	return strings.Join(append([]string{session, verb}, args...), "\x00")
}

// listingOf returns gateway's listing for this sling, reading it if no read has
// yet. The census lock covers only the lookup and the store; a listing in
// flight is waited on, not read again.
func (c *SandboxCensus) listingOf(gateway string) *gatewayListing {
	c.mu.Lock()
	if l, ok := c.listings[gateway]; ok {
		c.mu.Unlock()
		<-l.done
		return l
	}
	l := &gatewayListing{done: make(chan struct{})}
	c.listings[gateway] = l
	c.mu.Unlock()

	entries, err := (&sandbox.CLI{Gateway: gateway, Run: c.run}).List(context.Background())
	switch {
	case err != nil:
		l.err = err
	case len(entries) == 0:
		// An empty reply is not an answer that no seat exists: the registry
		// may hold records the gateway has lost track of, and reading absent
		// would skip their probes.
		l.err = fmt.Errorf("sandbox listing of gateway %q is empty", gateway)
	default:
		l.rows = make(map[string]string, len(entries))
		for _, e := range entries {
			l.rows[e.Name] = e.Phase
		}
	}
	close(l.done)
	return l
}

// seatPhase reports the gateway's phase for the sandbox name, and whether the
// listing names it. A listing that could not be read is an error for every seat
// of that gateway: an unknown listing must not read as "no sandbox".
func (c *SandboxCensus) seatPhase(gateway, name string) (phase string, listed bool, err error) {
	l := c.listingOf(gateway)
	if l.err != nil {
		return "", false, l.err
	}
	phase, listed = l.rows[name]
	return phase, listed, nil
}

// seatLiveness is how the census reads one phase the gateway lists.
type seatLiveness int

const (
	// seatUnknownPhase is any phase the census has not been taught. It is
	// neither live nor dead, so the seat is asked directly.
	seatUnknownPhase seatLiveness = iota
	// seatLive is Ready: the seat's tmux server can be asked.
	seatLive
	// seatNotLive is Stopped, Error or Failed: the seat has no server to ask.
	seatNotLive
)

// livenessOf reads phase, compared case-insensitively after trimming.
func livenessOf(phase string) seatLiveness {
	switch strings.ToLower(strings.TrimSpace(phase)) {
	case "ready":
		return seatLive
	case "stopped", "error", "failed":
		return seatNotLive
	}
	return seatUnknownPhase
}

// cached returns the answer kept for key, if one was.
func (c *SandboxCensus) cached(key string) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.answers[key]
	return v, ok
}

// keep stores a successful answer for key.
func (c *SandboxCensus) keep(key string, v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.answers[key] = v
}

// Forget drops the answers held for session and every gateway listing that
// read whole or is still in flight. Called when this process creates or kills
// the session: a new sandbox is not in a listing taken before it, so the
// listing is read again rather than read as "absent". A listing that failed is
// kept, as the sling's answer that the gateway cannot be read.
func (c *SandboxCensus) Forget(session string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prefix := session + "\x00"
	for k := range c.answers {
		if strings.HasPrefix(k, prefix) {
			delete(c.answers, k)
		}
	}
	for gateway, l := range c.listings {
		select {
		case <-l.done:
			if l.err == nil {
				delete(c.listings, gateway)
			}
		default:
			delete(c.listings, gateway)
		}
	}
}

// censusRead answers the tmux read verb(args) on session through c. A session
// with no sandbox record, or a census that cannot read its listing, takes the
// per-seat probe as it always did. A Ready sandbox takes the probe once, and a
// seat absent from the listing, Stopped, Error or Failed is answered
// ErrSeatNotLive with no probe at all. A phase the census does not know takes
// the probe every read and is never kept.
func censusRead[T any](c *SandboxCensus, session, verb string, args []string, probe func() (T, error)) (T, error) {
	var zero T
	if c == nil {
		return probe()
	}
	rec, ok, err := ReadSandboxRecord(c.townRoot, session)
	if err != nil || !ok {
		return probe()
	}
	phase, listed, err := c.seatPhase(rec.Gateway, session)
	if err != nil {
		return probe()
	}
	if !listed {
		return zero, ErrSeatNotLive
	}
	switch livenessOf(phase) {
	case seatNotLive:
		return zero, ErrSeatNotLive
	case seatUnknownPhase:
		return probe()
	}
	key := seatKey(session, verb, args)
	if v, ok := c.cached(key); ok {
		if t, ok := v.(T); ok {
			return t, nil
		}
	}
	out, err := probe()
	if err != nil {
		return out, err
	}
	c.keep(key, out)
	return out, nil
}

// BeginSandboxCensus starts the census of one sling process: a fresh
// SandboxCensus for the town, with run as its openshell runner (nil runs the
// binary), installed for every SandboxCensusRead until the returned end func
// runs. A process with no town installs nothing and returns a no-op end.
func BeginSandboxCensus(run sandbox.Runner) (end func()) {
	town := routing.town()
	if town == "" {
		return func() {}
	}
	setSandboxCensus(NewSandboxCensus(town, run))
	return func() { setSandboxCensus(nil) }
}

// SandboxCensusRead answers a read-only tmux question about session through the
// census BeginSandboxCensus installed. With none installed it is the probe.
// probe is the one question the per-seat path asks, and verb with args names it
// for the cache.
func SandboxCensusRead[T any](session, verb string, args []string, probe func() (T, error)) (T, error) {
	return censusRead(installedCensus(), session, verb, args, probe)
}
