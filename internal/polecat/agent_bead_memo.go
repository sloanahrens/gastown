package polecat

import (
	"sync"

	"github.com/steveyegge/gastown/internal/beads"
)

// agentBeadMemo holds the agent beads one sling's reuse step has read, so the
// seat listing and the reuse of the seat it picks read each bead from bd once.
// It exists only between ShareAgentBeadReads and its end func, and belongs to
// one Manager, so it never outlives the sling that opened it.
type agentBeadMemo struct {
	mu    sync.Mutex
	reads map[string]agentBeadRead
}

// agentBeadRead is one successful read of an agent bead: the bead and its
// parsed fields, both as GetAgentBead returned them.
type agentBeadRead struct {
	issue  *beads.Issue
	fields *beads.AgentFields
}

func newAgentBeadMemo() *agentBeadMemo {
	return &agentBeadMemo{reads: map[string]agentBeadRead{}}
}

func (c *agentBeadMemo) get(agentID string) (agentBeadRead, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	read, ok := c.reads[agentID]
	return read, ok
}

func (c *agentBeadMemo) put(agentID string, read agentBeadRead) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reads[agentID] = read
}

func (c *agentBeadMemo) forget(agentID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.reads, agentID)
}

// ShareAgentBeadReads makes this Manager read each agent bead from bd at most
// once until the returned end func is called. The sling's reuse step opens it
// around its seat decision and reuse. A successful read is kept; an error is
// not, so the next asker reads again. A reuse forgets its seat's bead when it
// returns, so a read after the reuse sees what the reuse wrote. With no memo
// open, every read goes to bd as before.
func (m *Manager) ShareAgentBeadReads() (end func()) {
	m.agentMemo = newAgentBeadMemo()
	return func() { m.agentMemo = nil }
}

// readAgentBead is beads.GetAgentBead through the open memo, if any.
func (m *Manager) readAgentBead(agentID string) (*beads.Issue, *beads.AgentFields, error) {
	memo := m.agentMemo
	if memo == nil {
		return beads.GetAgentBead(m.agentBeads(), agentID)
	}
	if read, ok := memo.get(agentID); ok {
		return read.issue, read.fields, nil
	}
	issue, fields, err := beads.GetAgentBead(m.agentBeads(), agentID)
	if err != nil {
		return nil, nil, err
	}
	memo.put(agentID, agentBeadRead{issue: issue, fields: fields})
	return issue, fields, nil
}

// forgetAgentBead drops agentID from the open memo, so the next read goes to bd.
func (m *Manager) forgetAgentBead(agentID string) {
	if m.agentMemo != nil {
		m.agentMemo.forget(agentID)
	}
}
