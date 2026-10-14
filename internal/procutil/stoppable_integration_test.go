//go:build integration && !windows

package procutil

import (
	"context"
	"io"
	"os/exec"
	"sync"
	"testing"
)

// The proof a restart leaves no child behind is the process table: a child that
// was not waited on stays there as stat Z. Each test names the children it
// started and looks each pid up after Quiesce returns, which is directly after
// it: Quiesce returns only once the run has, and the run returns only after its
// Waits have, so there is nothing to sleep for. The tests do not run in
// parallel, since KillChildren kills every child of the test process.

// inFlightPolls stands in for the dashboard hub's poll workers at the instant of
// a restart: n workers, each with one child that reads stdin until it closes
// and a goroutine that Waits for it, as every call site of the dashboard does.
type inFlightPolls struct {
	mu       sync.Mutex
	children []*exec.Cmd
	stdins   []io.WriteCloser
	started  sync.WaitGroup
}

// run is the Stoppable's run: it starts the children, then returns once every
// poll has Waited, which is when the real hub's Run returns.
func (p *inFlightPolls) run(n int) func(context.Context) {
	return func(context.Context) {
		var polls sync.WaitGroup
		for range n {
			polls.Add(1)
			go func() {
				defer polls.Done()
				cmd := exec.Command("sh", "-c", "read x")
				stdin, err := cmd.StdinPipe()
				if err == nil {
					err = cmd.Start()
				}
				p.mu.Lock()
				if err == nil {
					p.children = append(p.children, cmd)
					p.stdins = append(p.stdins, stdin)
				}
				p.mu.Unlock()
				p.started.Done()
				if err == nil {
					_ = cmd.Wait() // the poll's own Wait is what reaps the child
				}
			}()
		}
		polls.Wait()
	}
}

// pids is the pids of the children started so far.
func (p *inFlightPolls) pids() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []int
	for _, c := range p.children {
		out = append(out, c.Process.Pid)
	}
	return out
}

func (p *inFlightPolls) releaseAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.stdins {
		_ = s.Close()
	}
}

func assertGone(t *testing.T, pids []int) {
	t.Helper()
	for _, pid := range pids {
		if inProcessTable(t, pid) {
			t.Errorf("child %d is still in the process table (defunct or alive) after the restart quiesce", pid)
		}
	}
}

// Polls that finish inside the drain bound are waited for, not killed, and
// every child is gone from the table when Quiesce returns.
func TestIntegrationQuiesce_ReapsInFlightChildrenWithoutKilling(t *testing.T) {
	const n = 10 // about what the hub has in flight (one events tail per store)
	var polls inFlightPolls
	polls.started.Add(n)
	s := StartStoppable(context.Background(), polls.run(n))
	polls.started.Wait()
	pids := polls.pids()
	if len(pids) != n {
		t.Fatalf("%d children in flight, want %d", len(pids), n)
	}

	killed := false
	go polls.releaseAll() // the polls finish on their own, in whichever order
	s.Quiesce(QuiesceBounds{Drain: eventually, Kill: eventually}, func() int { killed = true; return 0 }, t.Logf)

	if killed {
		t.Fatal("killed children of polls that were about to finish")
	}
	assertGone(t, pids)
}

// Polls that outlive the drain bound have their children killed with the real
// KillChildren, and the polls' Waits reap them: none is left defunct.
func TestIntegrationQuiesce_KillsAndReapsStragglerChildren(t *testing.T) {
	const n = 10
	var polls inFlightPolls
	polls.started.Add(n)
	s := StartStoppable(context.Background(), polls.run(n))
	polls.started.Wait()
	pids := polls.pids()

	killed := 0
	s.Quiesce(QuiesceBounds{Drain: 0, Kill: eventually}, func() int {
		killed = KillChildren()
		return killed
	}, t.Logf)

	if killed < n {
		t.Fatalf("KillChildren signaled %d, want at least the %d children in flight", killed, n)
	}
	assertGone(t, pids)
}
