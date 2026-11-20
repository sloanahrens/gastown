package daemon

import "sync"

// nodePruneSeen holds the run ids the node image prune already ran for in this
// process, so a roll whose record cannot be written is not pruned on every
// heartbeat (gt-otks5k). The zero value is ready to use.
type nodePruneSeen struct {
	mu  sync.Mutex
	ids map[string]bool
}

// Remember records runID; the empty id is ignored.
func (s *nodePruneSeen) Remember(runID string) {
	if runID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ids == nil {
		s.ids = make(map[string]bool)
	}
	s.ids[runID] = true
}

// Seen reports whether runID was remembered.
func (s *nodePruneSeen) Seen(runID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ids[runID]
}
