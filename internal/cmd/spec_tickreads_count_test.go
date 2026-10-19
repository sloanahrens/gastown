package cmd

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// countingStore counts the reads made against a store, by kind, so a test can
// say how many bd calls a tick would have spent.
type countingStore struct {
	beads.Client
	shows    map[string]int
	multis   [][]string
	lists    int
	assigned int
	// failBatch makes the batched reads (ShowMultiple, List) fail.
	failBatch bool
}

func newCountingStore(db beads.Client) *countingStore {
	return &countingStore{Client: db, shows: map[string]int{}}
}

func (c *countingStore) Show(id string) (*beads.Issue, error) {
	c.shows[id]++
	return c.Client.Show(id)
}

func (c *countingStore) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	c.multis = append(c.multis, append([]string(nil), ids...))
	if c.failBatch {
		return nil, errors.New("bd show: boom")
	}
	return c.Client.ShowMultiple(ids)
}

func (c *countingStore) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	c.lists++
	if c.failBatch {
		return nil, errors.New("bd list: boom")
	}
	return c.Client.List(opts)
}

func (c *countingStore) GetAssignedIssue(assignee string) (*beads.Issue, error) {
	c.assigned++
	return c.Client.GetAssignedIssue(assignee)
}

// TestSpecDispatchTickReadsEachBeadOnce runs the golden's sixteen candidates
// through the tick's reads over a counting store: a bead the roster already read
// is not read again by the candidate loop, the held-work read costs one batched
// call, and the only beads read twice are the ones slung, whose last-moment
// re-read is meant to ask bd. The tick's decisions are the golden's (gt-u8rbh).
func TestSpecDispatchTickReadsEachBeadOnce(t *testing.T) {
	t.Parallel()
	f := specGoldenTown()
	db := beadsfake.New()
	for _, s := range f.specs {
		if over, ok := f.showOverride[s.ID]; ok {
			s = over
		}
		db.Seed(issueOfSpec(s))
	}
	db.Seed(beads.Issue{ID: "gt-t61ft", Status: beads.StatusHooked, Assignee: "gastown/polecats/capable",
		Description: "internal/daemon/overrun_guard.go"})
	db.Seed(beads.Issue{ID: "gt-other", Status: string(beads.StatusInProgress), Assignee: "gastown/polecats/other",
		Description: "no path"})
	store := newCountingStore(db)
	reads := newSpecTickReads()

	// The roster read gt-c4 as a polecat's hook reference before the loop began.
	if _, err := reads.client(store).Show("gt-c4"); err != nil {
		t.Fatalf("roster read: %v", err)
	}

	env := f.env()
	env.PerTick = 3
	env.ShapeGate = specdispatch.ShapeGateWarn
	read := func(id string) (*beads.Issue, error) { return store.Show(id) }
	env.Show = func(id string) (specdispatch.Spec, error) { return showSpecWith(reads, id, read) }
	env.ShowFresh = func(id string) (specdispatch.Spec, error) { return showSpecWith(nil, id, read) }
	env.Held = func(string) ([]specdispatch.HeldBead, error) {
		return heldBeadsFrom(reads.client(store), func(string, error) {}, t.TempDir())
	}
	r := runSpecDispatchCycle(env)

	if got, want := renderSpecTick(f.slung, r), readSpecTickGolden(t); got != want {
		t.Fatalf("tick differs from testdata/spec_dispatch_tick.golden:\n--- got\n%s--- want\n%s", got, want)
	}
	slung := map[string]bool{}
	for _, id := range f.slung {
		slung[id] = true
	}
	for id, n := range store.shows {
		want := 1
		if slung[id] {
			want = 2 // the candidate read, then the re-read before the sling
		}
		if n != want {
			t.Errorf("bead %s read %d times, want %d", id, n, want)
		}
	}
	if n := len(store.shows); n != 16 {
		t.Errorf("read %d beads singly, want the 16 candidates", n)
	}
	// The held beads are read in the one batched call, and nothing the loop
	// already read is asked for again.
	if len(store.multis) != 1 {
		t.Errorf("held-work batches = %v, want one", store.multis)
	}
	for _, ids := range store.multis {
		for _, id := range ids {
			if _, singly := store.shows[id]; singly {
				t.Errorf("bead %s read singly and again in a batch %v", id, ids)
			}
		}
	}
}

// The ledger keeps what a tick read for the tick alone, asks bd only for what
// it lacks, retries a failed read, counts the calls that reach bd, and a nil
// ledger is a plain read.
func TestSpecTickReads(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	db.Seed(beads.Issue{ID: "gt-a"}, beads.Issue{ID: "gt-b"}, beads.Issue{ID: "gt-c"})
	store := newCountingStore(db)
	reads := newSpecTickReads()
	client := reads.client(store)
	for i := 0; i < 3; i++ {
		if _, err := client.Show("gt-a"); err != nil {
			t.Fatalf("Show: %v", err)
		}
	}
	got, err := client.ShowMultiple([]string{"gt-a", "gt-b", "gt-c", "gt-gone"})
	if err != nil || len(got) != 3 {
		t.Fatalf("ShowMultiple = %v, %v; want the three beads that exist", got, err)
	}
	if want := []string{"gt-b", "gt-c", "gt-gone"}; !reflect.DeepEqual(store.multis, [][]string{want}) {
		t.Errorf("batch asked for %v, want only what the ledger lacked", store.multis)
	}
	for i := 0; i < 2; i++ {
		if _, err := client.Show("gt-nope"); err == nil {
			t.Fatal("Show of a missing bead succeeded")
		}
	}
	if store.shows["gt-a"] != 1 || store.shows["gt-nope"] != 2 {
		t.Errorf("reads = %v, want gt-a once and the failed read tried again", store.shows)
	}
	// One show of gt-a, one batch, two failed shows.
	if reads.Calls() != 4 {
		t.Errorf("Calls = %d, want 4", reads.Calls())
	}

	var none *specTickReads
	if _, err := none.client(store).Show("gt-a"); err != nil {
		t.Errorf("nil ledger Show: %v", err)
	}
	if none.Calls() != 0 {
		t.Errorf("nil ledger counted %d calls", none.Calls())
	}
	want := "tick cost: 4 bd call(s) outside the sling path in 1.234s"
	if line := specTickCost(reads, 1234*time.Millisecond); line != want {
		t.Errorf("specTickCost = %q, want %q", line, want)
	}
}
