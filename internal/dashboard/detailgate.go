package dashboard

import (
	"errors"
	"sync"
)

// errDetailQuiescing is what a bead-detail read gets while the dashboard is
// restarting in place. The page's detail fetch fails and the row can be opened
// again; the restarted dashboard (or this one, if the exec fails) answers it.
var errDetailQuiescing = errors.New("dashboard restarting; retry")

// detailGate lets the hub's run own the bead-detail reads. A read starts a bd
// child on an HTTP-server goroutine that the hub's run does not wait for, so a
// restart that only waited for the polls could exec while that child was still
// running and strand it as defunct (gt-fekq1). While the gate is open a read
// enters and leaves it; once the run's context ends the gate closes, refusing
// new reads, and the run waits for the ones that entered.
//
// The zero value is open, so a hub that is never Run serves reads as before.
type detailGate struct {
	mu       sync.Mutex
	idle     sync.Cond
	closed   bool
	inFlight int
}

func (g *detailGate) cond() *sync.Cond {
	if g.idle.L == nil {
		g.idle.L = &g.mu
	}
	return &g.idle
}

// enter admits one read, or reports false while the gate is closed. Every true
// must be paired with a leave.
func (g *detailGate) enter() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.closed {
		return false
	}
	g.inFlight++
	return true
}

func (g *detailGate) leave() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.inFlight--
	if g.inFlight == 0 {
		g.cond().Broadcast()
	}
}

// isClosed reports whether new reads are being refused. A read already inside
// the gate checks it again before it starts a child.
func (g *detailGate) isClosed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closed
}

// open admits reads again, for a run that starts or restarts.
func (g *detailGate) open() {
	g.mu.Lock()
	g.closed = false
	g.mu.Unlock()
}

// closeAndDrain refuses new reads and returns once the ones inside have left.
func (g *detailGate) closeAndDrain() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
	c := g.cond()
	for g.inFlight > 0 {
		c.Wait()
	}
}
