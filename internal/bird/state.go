package bird

import "time"

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

// State is the registry file: the agents holding names and the names bound to
// beads.
type State struct {
	Agents   []Agent   `json:"agents"`
	Bindings []Binding `json:"bindings"`
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

// Bind records name bound to bead, adding nothing when that pair is already
// bound, and reports whether it added the binding.
func (s *State) Bind(name, bead string, at time.Time) bool {
	for _, b := range s.Bindings {
		if b.Name == name && b.Bead == bead {
			return false
		}
	}
	s.Bindings = append(s.Bindings, Binding{Name: name, Bead: bead, Time: at})
	return true
}

// PruneDead drops every agent isLive rejects and reports how many it dropped.
func (s *State) PruneDead(isLive func(Agent) bool) int {
	kept := s.Agents[:0]
	for _, a := range s.Agents {
		if isLive(a) {
			kept = append(kept, a)
		}
	}
	dropped := len(s.Agents) - len(kept)
	s.Agents = kept
	return dropped
}

// Taken returns every name the state holds: every bound name and every agent
// isLive accepts.
func (s *State) Taken(isLive func(Agent) bool) map[string]bool {
	taken := make(map[string]bool, len(s.Bindings)+len(s.Agents))
	for _, b := range s.Bindings {
		taken[b.Name] = true
	}
	for _, a := range s.Agents {
		if isLive(a) {
			taken[a.Name] = true
		}
	}
	return taken
}
