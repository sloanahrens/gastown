package polecat

import (
	"errors"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
)

// showCounter counts the Show calls each agent bead receives. failOnce names
// the beads whose next Show fails, as a dropped bd call does.
type showCounter struct {
	*polecatDB

	mu       sync.Mutex
	shows    map[string]int
	failOnce map[string]bool
}

func newShowCounter(db *polecatDB) *showCounter {
	return &showCounter{polecatDB: db, shows: map[string]int{}, failOnce: map[string]bool{}}
}

func (c *showCounter) Show(id string) (*beads.Issue, error) {
	c.mu.Lock()
	c.shows[id]++
	fail := c.failOnce[id]
	delete(c.failOnce, id)
	c.mu.Unlock()
	if fail {
		return nil, errors.New("bd show: connection reset")
	}
	return c.polecatDB.Show(id)
}

func (c *showCounter) count(id string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.shows[id]
}

// installShowCounter makes mgr read its beads through a counter over the same
// fake database canonicalWithPolecats built.
func installShowCounter(mgr *Manager, db *polecatDB) *showCounter {
	c := newShowCounter(db)
	mgr.beads = c
	mgr.agentBD = c
	return c
}

// idleSeatNames is the name of every seat IdlePolecats yields, in order.
func idleSeatNames(t *testing.T, mgr *Manager) []string {
	t.Helper()
	seats, err := mgr.IdlePolecats()
	if err != nil {
		t.Fatalf("IdlePolecats: %v", err)
	}
	var names []string
	for p := range seats {
		names = append(names, p.Name)
	}
	return names
}

// TestIdleReuseReadsEachAgentBeadOnce: inside one sling's reuse step, each
// candidate seat's agent bead is read from bd once by the seat listing, and
// the reuse of the picked seat reads it from the memo, not bd. The picked
// seat's reuse writes its bead, so the one read after that write goes to bd.
func TestIdleReuseReadsEachAgentBeadOnce(t *testing.T) {
	t.Parallel()
	names := []string{"p1", "p2", "p3", "p4", "p5", "p6"}

	t.Run("each candidate is read once and the picked seat is reused from the memo", func(t *testing.T) {
		t.Parallel()
		mgr, _, db, _, _ := canonicalWithPolecats(t, true, names...)
		counter := installShowCounter(mgr, db)
		end := mgr.ShareAgentBeadReads()
		defer end()

		seats := idleSeatNames(t, mgr)
		if len(seats) != len(names) {
			t.Fatalf("IdlePolecats yielded %v, want all %d seats", seats, len(names))
		}
		for _, name := range seats {
			if got := counter.count(mgr.agentBeadID(name)); got != 1 {
				t.Errorf("listing read %s %d times, want 1", name, got)
			}
		}

		picked := seats[0]
		before := counter.count(mgr.agentBeadID(picked))
		if _, err := mgr.ReuseIdlePolecat(picked, AddOptions{HookBead: "gt-next"}); err != nil {
			t.Fatalf("ReuseIdlePolecat(%s): %v", picked, err)
		}
		// The reuse's lookup and its gate both answer from the memo. The two
		// bd reads left are CreateOrReopenAgentBead's own read-then-rewrite of
		// the bead the reuse writes (the write runs before the forget).
		if got := counter.count(mgr.agentBeadID(picked)) - before; got != 2 {
			t.Errorf("reuse of %s read bd %d times, want 2 (the write's own reads only)", picked, got)
		}
		for _, name := range seats[1:] {
			if got := counter.count(mgr.agentBeadID(name)); got != 1 {
				t.Errorf("after reuse, %s read %d times, want 1", name, got)
			}
		}
	})

	t.Run("an errored read is not kept, so the next asker reads again", func(t *testing.T) {
		t.Parallel()
		mgr, _, db, _, _ := canonicalWithPolecats(t, true, names...)
		counter := installShowCounter(mgr, db)
		end := mgr.ShareAgentBeadReads()
		defer end()

		failed := "p3"
		counter.mu.Lock()
		counter.failOnce[mgr.agentBeadID(failed)] = true
		counter.mu.Unlock()

		// The listing's read of the failed seat errors, so the seat is not
		// yielded and its error is not remembered.
		for _, name := range idleSeatNames(t, mgr) {
			if name == failed {
				t.Fatalf("listing yielded %s although its agent bead read failed", failed)
			}
		}
		if got := counter.count(mgr.agentBeadID(failed)); got != 1 {
			t.Fatalf("listing read %s %d times, want 1", failed, got)
		}

		// The next asker reads bd again. The read succeeds and is kept, so a
		// later read of the same seat in this step does not reach bd.
		if _, _, err := mgr.lookupAgentBead(mgr.agentBeadID(failed), nil); err != nil {
			t.Fatalf("lookupAgentBead(%s) after the failed read: %v", failed, err)
		}
		if got := counter.count(mgr.agentBeadID(failed)); got != 2 {
			t.Errorf("next asker read %s %d times in total, want 2 (the failed read was not kept)", failed, got)
		}
		if _, _, err := mgr.readAgentBead(mgr.agentBeadID(failed)); err != nil {
			t.Fatalf("readAgentBead(%s): %v", failed, err)
		}
		if got := counter.count(mgr.agentBeadID(failed)); got != 2 {
			t.Errorf("a third asker of %s reached bd: %d reads, want 2 (the successful read was not kept)", failed, got)
		}
	})

	t.Run("a read after the reuse's write goes to bd", func(t *testing.T) {
		t.Parallel()
		mgr, _, db, _, _ := canonicalWithPolecats(t, true, names...)
		counter := installShowCounter(mgr, db)
		end := mgr.ShareAgentBeadReads()
		defer end()

		picked := idleSeatNames(t, mgr)[0]
		if _, err := mgr.ReuseIdlePolecat(picked, AddOptions{HookBead: "gt-next"}); err != nil {
			t.Fatalf("ReuseIdlePolecat(%s): %v", picked, err)
		}
		// The reuse wrote the bead, so Get must read it from bd and see the
		// write: a read the memo answered would show the state from before it.
		afterReuse := counter.count(mgr.agentBeadID(picked))
		if _, err := mgr.Get(picked); err != nil {
			t.Fatalf("Get(%s) after reuse: %v", picked, err)
		}
		if got := counter.count(mgr.agentBeadID(picked)) - afterReuse; got != 1 {
			t.Errorf("Get(%s) after the reuse's write read bd %d times, want 1", picked, got)
		}
	})

	t.Run("with no memo open every read goes to bd", func(t *testing.T) {
		t.Parallel()
		mgr, _, db, _, _ := canonicalWithPolecats(t, true, names...)
		counter := installShowCounter(mgr, db)

		picked := idleSeatNames(t, mgr)[0]
		if _, err := mgr.ReuseIdlePolecat(picked, AddOptions{HookBead: "gt-next"}); err != nil {
			t.Fatalf("ReuseIdlePolecat(%s): %v", picked, err)
		}
		// The listing reads the picked seat once; the reuse reads it for its
		// lookup and its gate, then the bead write reads it twice more.
		if got := counter.count(mgr.agentBeadID(picked)); got != 5 {
			t.Errorf("without a memo, %s was read %d times, want 5 (listing, lookup, gate, write's two)", picked, got)
		}
	})
}

// TestAgentBeadMemoEndDropsReads: once the end func runs, the Manager reads bd
// again, so a memo never outlives the sling that opened it.
func TestAgentBeadMemoEndDropsReads(t *testing.T) {
	t.Parallel()
	mgr, _, db, _, _ := canonicalWithPolecats(t, true, "p1")
	counter := installShowCounter(mgr, db)
	id := mgr.agentBeadID("p1")

	end := mgr.ShareAgentBeadReads()
	if _, _, err := mgr.readAgentBead(id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := mgr.readAgentBead(id); err != nil {
		t.Fatal(err)
	}
	if got := counter.count(id); got != 1 {
		t.Fatalf("two reads under the memo reached bd %d times, want 1", got)
	}
	end()
	if _, _, err := mgr.readAgentBead(id); err != nil {
		t.Fatal(err)
	}
	if got := counter.count(id); got != 2 {
		t.Errorf("a read after end reached bd %d times in total, want 2", got)
	}
}
