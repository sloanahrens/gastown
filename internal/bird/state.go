package bird

import (
	"fmt"
	"time"
)

// Agent is one running bird: the process that holds a name.
type Agent struct {
	Name string `json:"name"`
	PID  int    `json:"pid"`
	// Start is the kernel's start token for PID (internal/procid). A pid the
	// kernel reused carries a different token, so the pair names one process
	// for good.
	Start string `json:"start"`
	// Bead is the bead the agent was launched against, when it was launched
	// against one.
	Bead string `json:"bead,omitempty"`
	Cwd  string `json:"cwd,omitempty"`
	// Started is when the record was written, from the registry's clock.
	Started time.Time `json:"started"`
}

// Binding records that a name ran a bead, or was chosen for it.
type Binding struct {
	Name string    `json:"name"`
	Bead string    `json:"bead"`
	Time time.Time `json:"time"`
}

// Reservation is a name Pick handed out that nothing has bound yet, held until
// it ages past the registry's TTL, so a launcher that dies between Pick and
// Bind does not hold a name for good.
type Reservation struct {
	Name string    `json:"name"`
	Time time.Time `json:"time"`
}

// Expired reports whether the reservation was made longer than ttl before now.
func (res Reservation) Expired(now time.Time, ttl time.Duration) bool {
	return now.Sub(res.Time) > ttl
}

// State is the registry file: the agents holding names, the names bound to
// beads, and the names Pick has handed out that nothing bound yet.
type State struct {
	Agents       []Agent       `json:"agents"`
	Bindings     []Binding     `json:"bindings"`
	Reservations []Reservation `json:"reservations,omitempty"`
}

// Bound returns the newest name bound to bead.
func (s *State) Bound(bead string) (string, bool) {
	best := -1
	for i, b := range s.Bindings {
		if b.Bead != bead {
			continue
		}
		if best < 0 || !b.Time.Before(s.Bindings[best].Time) {
			best = i
		}
	}
	if best < 0 {
		return "", false
	}
	return s.Bindings[best].Name, true
}

// Bind records name bound to bead, dropping the reservation Pick may have made
// for it, and reports whether it added a pair that was not bound already.
func (s *State) Bind(name, bead string, at time.Time) bool {
	s.DropReservation(name)
	for _, b := range s.Bindings {
		if b.Name == name && b.Bead == bead {
			return false
		}
	}
	s.Bindings = append(s.Bindings, Binding{Name: name, Bead: bead, Time: at})
	return true
}

// SetAgent records a as the agent of its name, replacing the earlier record
// for that name, since one name is one process.
func (s *State) SetAgent(a Agent) {
	for i, old := range s.Agents {
		if old.Name == a.Name {
			s.Agents[i] = a
			return
		}
	}
	s.Agents = append(s.Agents, a)
}

// Reserve records that Pick handed out name at now, replacing any reservation
// the name already had.
func (s *State) Reserve(name string, now time.Time) {
	s.DropReservation(name)
	s.Reservations = append(s.Reservations, Reservation{Name: name, Time: now})
}

// DropReservation removes every reservation for name, if any.
func (s *State) DropReservation(name string) {
	kept := s.Reservations[:0]
	for _, res := range s.Reservations {
		if res.Name != name {
			kept = append(kept, res)
		}
	}
	s.Reservations = kept
}

// PruneReservations drops every reservation older than ttl at now and reports
// how many it dropped.
func (s *State) PruneReservations(now time.Time, ttl time.Duration) int {
	kept := make([]Reservation, 0, len(s.Reservations))
	for _, res := range s.Reservations {
		if !res.Expired(now, ttl) {
			kept = append(kept, res)
		}
	}
	dropped := len(s.Reservations) - len(kept)
	s.Reservations = kept
	return dropped
}

// PruneDead drops every agent isLive rejects and reports how many it dropped,
// leaving the state as it was when any liveness cannot be read, since an
// unreadable process is not a dead one.
func (s *State) PruneDead(isLive func(Agent) (bool, error)) (int, error) {
	kept := make([]Agent, 0, len(s.Agents))
	for _, a := range s.Agents {
		live, err := isLive(a)
		if err != nil {
			return 0, fmt.Errorf("pruning the agents: %w", err)
		}
		if live {
			kept = append(kept, a)
		}
	}
	dropped := len(s.Agents) - len(kept)
	s.Agents = kept
	return dropped, nil
}

// Held returns every name the state holds at now with every recorded agent
// counted, which is what Pick reads once PruneDead has dropped the dead ones.
func (s *State) Held(now time.Time, ttl time.Duration) map[string]bool {
	taken := s.heldByState(now, ttl)
	for _, a := range s.Agents {
		taken[a.Name] = true
	}
	return taken
}

// Taken returns every name the state holds at now — bound, reserved for less
// than ttl, or held by an agent isLive accepts — and fails rather than guess
// when a liveness cannot be read, since a name it cannot judge may be in use.
func (s *State) Taken(now time.Time, ttl time.Duration, isLive func(Agent) (bool, error)) (map[string]bool, error) {
	taken := s.heldByState(now, ttl)
	for _, a := range s.Agents {
		live, err := isLive(a)
		if err != nil {
			return nil, fmt.Errorf("collecting the taken names: %w", err)
		}
		if live {
			taken[a.Name] = true
		}
	}
	return taken, nil
}

// heldByState is the part of the taken set no process table decides.
func (s *State) heldByState(now time.Time, ttl time.Duration) map[string]bool {
	taken := make(map[string]bool, len(s.Bindings)+len(s.Agents)+len(s.Reservations))
	for _, b := range s.Bindings {
		taken[b.Name] = true
	}
	for _, res := range s.Reservations {
		if !res.Expired(now, ttl) {
			taken[res.Name] = true
		}
	}
	return taken
}
