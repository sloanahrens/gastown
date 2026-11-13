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
// the gateway lists as absent or not Ready. No tmux read is made for such a
// seat: its sandbox has no server to ask.
var ErrSeatNotLive = errors.New("sandbox seat is not live: its sandbox is absent from the gateway or not Ready")

// SandboxCensus is one sling process's read-once view of the sandbox seats. A
// sling asks the same questions of every seat in its seat decision and again in
// its admission, and each question was one openshell process per seat per read.
// The census asks the gateway for its sandbox list once per gateway, and answers
// a seat's read-only tmux question once per sling, so the later read reads the
// census.
//
// Only a Ready sandbox gets a tmux probe, and a read that succeeded is kept.
// A failed listing or a failed probe is not kept: the read falls back to the
// per-seat probe, and the next read asks again. A sling process installs one
// census (BeginSandboxCensus) and drops it when it ends; nothing carries from
// one sling into the next.
type SandboxCensus struct {
	townRoot string
	run      sandbox.Runner // nil runs the openshell binary

	mu sync.Mutex
	// phases holds, per gateway, the phase of each sandbox a listing that read
	// whole named. Only a whole listing is kept.
	phases map[string]map[string]string
	// answers holds the successful tmux reads, keyed by seatKey.
	answers map[string]any
}

// NewSandboxCensus returns an empty census for the town's sandbox registry.
// run runs the openshell calls; nil runs the binary itself.
func NewSandboxCensus(townRoot string, run sandbox.Runner) *SandboxCensus {
	return &SandboxCensus{
		townRoot: townRoot,
		run:      run,
		phases:   map[string]map[string]string{},
		answers:  map[string]any{},
	}
}

// seatKey names one read: the session, the verb and its arguments. The NUL
// separator cannot appear in a session name or a tmux argument.
func seatKey(session, verb string, args []string) string {
	return strings.Join(append([]string{session, verb}, args...), "\x00")
}

// seatPhase reports the gateway's phase for the sandbox name, and whether the
// listing names it. A listing that cannot be read, or that comes back empty
// while the registry holds a record for the seat, is an error: an unknown
// listing must not read as "no sandbox".
func (c *SandboxCensus) seatPhase(gateway, name string) (phase string, listed bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rows, ok := c.phases[gateway]
	if !ok {
		entries, err := (&sandbox.CLI{Gateway: gateway, Run: c.run}).List(context.Background())
		if err != nil {
			return "", false, err
		}
		if len(entries) == 0 {
			return "", false, fmt.Errorf("sandbox listing of gateway %q is empty while the registry holds %s", gateway, name)
		}
		rows = make(map[string]string, len(entries))
		for _, e := range entries {
			rows[e.Name] = e.Phase
		}
		c.phases[gateway] = rows
	}
	phase, listed = rows[name]
	return phase, listed, nil
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

// Forget drops the answers held for session and every gateway listing. Called
// when this process creates or kills the session: a new sandbox is not in the
// listing taken before it, so the listing is read again rather than read as
// "absent".
func (c *SandboxCensus) Forget(session string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	prefix := session + "\x00"
	for k := range c.answers {
		if strings.HasPrefix(k, prefix) {
			delete(c.answers, k)
		}
	}
	c.phases = map[string]map[string]string{}
}

// censusRead answers the tmux read verb(args) on session through c. A session
// with no sandbox record, or a census that cannot read its listing, takes the
// per-seat probe as it always did. A Ready sandbox takes the probe once, and a
// seat absent from the listing or not Ready is answered ErrSeatNotLive with no
// probe at all.
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
	if !listed || !strings.EqualFold(strings.TrimSpace(phase), "Ready") {
		return zero, ErrSeatNotLive
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
