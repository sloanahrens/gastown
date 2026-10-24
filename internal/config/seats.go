package config

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

// The session backends a seat can run its polecats on.
const (
	BackendTmux      = "tmux"
	BackendOpenShell = "openshell"
)

// HasSeat reports whether seats holds one for agent.
func HasSeat(seats []Seat, agent string) bool {
	return slices.ContainsFunc(seats, func(s Seat) bool { return s.Agent == agent })
}

// SeatConfig is one entry of polecat_pool.seats (gt-mukgk).
type SeatConfig struct {
	Cap     int      `json:"cap"`
	Closed  string   `json:"closed,omitempty"` // "HH:MM-HH:MM" in polecat_pool.timezone
	Names   []string `json:"names,omitempty"`
	Backend string   `json:"backend,omitempty"` // "" or "tmux" or "openshell"
	Sandbox string   `json:"sandbox,omitempty"` // key in TownSettings.Sandboxes; required when Backend is openshell
	// PinnedOnly keeps the seat for beads labeled agent:<its agent>: the
	// dispatcher's random draw for unlabelled beads never lands here.
	PinnedOnly bool `json:"pinned_only,omitempty"`
}

// Seat is a validated seat, as every reader sees it.
type Seat struct {
	Agent      string
	Cap        int
	Window     *Window // nil: always open
	Names      []string
	Backend    string // BackendTmux or BackendOpenShell, never empty
	Sandbox    string // key in TownSettings.Sandboxes; set only for an openshell seat
	PinnedOnly bool   // only beads labeled agent:<Agent> may take this seat
}

// Window is a daily closed period in one timezone.
type Window struct {
	StartMin, EndMin int // minutes after local midnight
	Loc              *time.Location
}

// Seats returns the town's seats sorted by agent name; known reports whether an
// agent name resolves. A nil known accepts every name.
func Seats(ts *TownSettings, known func(agent string) bool) ([]Seat, error) {
	if ts == nil || ts.PolecatPool == nil {
		return nil, nil
	}
	if known == nil {
		known = func(string) bool { return true }
	}
	pool := ts.PolecatPool
	var loc *time.Location
	if pool.Timezone != "" {
		l, err := time.LoadLocation(pool.Timezone)
		if err != nil {
			return nil, fmt.Errorf("polecat_pool.timezone %q: %w", pool.Timezone, err)
		}
		loc = l
	}
	agents := make([]string, 0, len(pool.Seats))
	for a := range pool.Seats {
		agents = append(agents, a)
	}
	sort.Strings(agents)
	owner := map[string]string{}
	out := make([]Seat, 0, len(agents))
	for _, a := range agents {
		c := pool.Seats[a]
		if a == UnknownSeatAgent {
			return nil, fmt.Errorf("polecat_pool.seats.%s: that key counts polecats that record no agent, and names no seat", a)
		}
		if !known(a) {
			return nil, fmt.Errorf("polecat_pool.seats.%s: no agent of that name in agents or the built-in presets", a)
		}
		if c.Cap < 0 {
			return nil, fmt.Errorf("polecat_pool.seats.%s.cap %d: must be 0 or more", a, c.Cap)
		}
		backend := c.Backend
		switch backend {
		case "":
			backend = BackendTmux
		case BackendTmux, BackendOpenShell:
		default:
			return nil, fmt.Errorf("polecat_pool.seats.%s.backend %q: want tmux or openshell", a, c.Backend)
		}
		if backend == BackendOpenShell {
			if c.Sandbox == "" {
				return nil, fmt.Errorf("polecat_pool.seats.%s.sandbox: required when backend is openshell", a)
			}
			p, ok := ts.Sandboxes[c.Sandbox]
			if !ok {
				return nil, fmt.Errorf("polecat_pool.seats.%s.sandbox %q: no such entry in sandboxes", a, c.Sandbox)
			}
			if err := p.Validate(c.Sandbox); err != nil {
				return nil, err
			}
		} else if c.Sandbox != "" {
			return nil, fmt.Errorf("polecat_pool.seats.%s.sandbox: only an openshell seat names a sandbox", a)
		}
		s := Seat{Agent: a, Cap: c.Cap, Names: c.Names, Backend: backend, Sandbox: c.Sandbox, PinnedOnly: c.PinnedOnly}
		if c.Closed != "" {
			if loc == nil {
				return nil, fmt.Errorf("polecat_pool.seats.%s.closed needs polecat_pool.timezone", a)
			}
			w, err := SeatWindow(pool, c)
			if err != nil {
				return nil, fmt.Errorf("polecat_pool.seats.%s.closed: %w", a, err)
			}
			s.Window = w
		}
		for _, n := range c.Names {
			if strings.TrimSpace(n) == "" {
				return nil, fmt.Errorf("polecat_pool.seats.%s.names: a name is empty", a)
			}
			if prev, ok := owner[n]; ok {
				if prev == a {
					return nil, fmt.Errorf("polecat_pool.seats.%s.names: name %q is listed twice", a, n)
				}
				return nil, fmt.Errorf("polecat_pool.seats: name %q is in both %s and %s", n, prev, a)
			}
			owner[n] = a
		}
		out = append(out, s)
	}
	return out, nil
}

// SeatsKnownTo returns the known callback for Seats under settings s: an agent
// resolves when s defines it in agents or it is a built-in preset.
func SeatsKnownTo(s *TownSettings) func(agent string) bool {
	return func(agent string) bool {
		if s != nil && s.Agents[agent] != nil {
			return true
		}
		return IsKnownPreset(agent)
	}
}

// ParseWindow parses "HH:MM-HH:MM" as a daily window in loc.
func ParseWindow(s string, loc *time.Location) (*Window, error) {
	parts := strings.Split(s, "-")
	if len(parts) != 2 {
		return nil, fmt.Errorf("window %q: want HH:MM-HH:MM", s)
	}
	start, err := parseClock(parts[0])
	if err != nil {
		return nil, fmt.Errorf("window %q: %w", s, err)
	}
	end, err := parseClock(parts[1])
	if err != nil {
		return nil, fmt.Errorf("window %q: %w", s, err)
	}
	if start == end {
		return nil, fmt.Errorf("window %q: start and end are the same", s)
	}
	return &Window{StartMin: start, EndMin: end, Loc: loc}, nil
}

// SeatWindow resolves seat's closed window in pool's timezone, or returns nil for
// a seat with no window. The loader and the sling router both read windows
// through it, so they cannot disagree about what a window means.
func SeatWindow(pool *PolecatPool, seat SeatConfig) (*Window, error) {
	if seat.Closed == "" {
		return nil, nil
	}
	if pool.Timezone == "" {
		return nil, fmt.Errorf("needs polecat_pool.timezone")
	}
	loc, err := time.LoadLocation(pool.Timezone)
	if err != nil {
		return nil, fmt.Errorf("polecat_pool.timezone %q: %w", pool.Timezone, err)
	}
	return ParseWindow(seat.Closed, loc)
}

// parseClock returns the minutes after midnight for an HH:MM clock time.
func parseClock(s string) (int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("%q is not HH:MM", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}

// minute is t as minutes after midnight in the window's location.
func (w *Window) minute(t time.Time) int {
	l := t.In(w.Loc)
	return l.Hour()*60 + l.Minute()
}

// Closed reports whether t falls inside the window.
func (w *Window) Closed(t time.Time) bool {
	m := w.minute(t)
	if w.StartMin < w.EndMin {
		return m >= w.StartMin && m < w.EndMin
	}
	return m >= w.StartMin || m < w.EndMin
}

// OpensAt returns the first instant at or after t when the window is open.
func (w *Window) OpensAt(t time.Time) time.Time {
	if !w.Closed(t) {
		return t
	}
	l := t.In(w.Loc)
	open := time.Date(l.Year(), l.Month(), l.Day(), w.EndMin/60, w.EndMin%60, 0, 0, w.Loc)
	if !open.After(l) {
		open = open.AddDate(0, 0, 1)
	}
	return open
}

// UnknownSeatAgent is the key a polecat is counted under when it records no
// agent and the seat map has no single seat to put it on. It is no seat, so it
// fills none, and it still counts toward max_total.
const UnknownSeatAgent = "(unrecorded)"

// UnrecordedAgent is the agent a polecat that records none is counted on: the
// seat map's only seat, UnknownSeatAgent when the map has several, and none
// for a pool with no seats. A count under another agent's name would read as
// that agent's seat taken (gt-2q283), so the ambiguous case gets a key of its
// own for the caller to report.
func (p *PolecatPool) UnrecordedAgent() string {
	if p == nil {
		return ""
	}
	switch len(p.Seats) {
	case 0:
		return ""
	case 1:
		for agent := range p.Seats {
			return agent
		}
	}
	return UnknownSeatAgent
}

// AgentResolver returns the known predicate Seats takes: an agent resolves when
// the launcher would find it, in the town's agents, settings/agents.json or the
// built-in presets. It is the loader's own lookup, so the seat map and a launch
// agree on what a name means. An empty townRoot skips settings/agents.json.
func AgentResolver(townRoot string, ts *TownSettings) func(agent string) bool {
	return agentResolverIn(processHost, townRoot, ts)
}

// agentResolverIn is AgentResolver resolving against h. A test names the host
// whose PATH and environment the lookup reads; the exported resolver reads the
// process's.
func agentResolverIn(h host, townRoot string, ts *TownSettings) func(agent string) bool {
	reg := agentRegistryFor(h, townRoot, "")
	return func(agent string) bool {
		return lookupAgentConfigIfExists(reg, agent, ts, nil) != nil
	}
}
