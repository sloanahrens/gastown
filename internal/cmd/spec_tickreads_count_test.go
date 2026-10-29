package cmd

import (
	"errors"
	"reflect"
	"sort"
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
// through the tick's reads over a counting store: the candidate loop reads no
// bead, since each candidate's spec is its board row (gt-bqj8q). A bead the
// roster already read is not read again, and each bead that reaches the
// last-moment re-read before its sling is read once there. The tick's decisions
// are the golden's (gt-u8rbh).
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
	env.Show = func(id string) (specdispatch.Spec, error) {
		t.Errorf("candidate loop read %s singly; its spec is the board row", id)
		return specdispatch.Spec{}, errors.New("unexpected candidate read")
	}
	fresh := map[string]int{}
	env.ShowFresh = func(id string) (specdispatch.Spec, error) {
		fresh[id]++
		return showSpecWith(nil, id, read)
	}
	r := runSpecDispatchCycle(env)

	if got, want := renderSpecTick(f.slung, r), readSpecTickGolden(t); got != want {
		t.Fatalf("tick differs from testdata/spec_dispatch_tick.golden:\n--- got\n%s--- want\n%s", got, want)
	}
	for _, id := range f.slung {
		if fresh[id] != 1 {
			t.Errorf("slung bead %s re-read %d times before its sling, want 1", id, fresh[id])
		}
	}
	// gt-c4 is the roster's read, made before the loop; every other bead the
	// tick read is one of its last-moment re-reads.
	for id, n := range store.shows {
		want := fresh[id]
		if id == "gt-c4" {
			want++
		}
		if n != want {
			t.Errorf("bead %s read %d times, want %d", id, n, want)
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
	want := "tick cost: 4 bd read call(s) outside the sling path in 1.234s"
	if line := specTickCost(reads, 1234*time.Millisecond, 0, 0); line != want {
		t.Errorf("specTickCost = %q, want %q", line, want)
	}
}

// The roster the tick draws, through its production wiring over a fake, counts
// the seats the per-seat read counts, and the reads it made are exactly the bd
// reads that reached the store, so the tick's cost line is the store's count
// (gt-u8rbh).
func TestSpecRosterSessionsWiringCountsItsReads(t *testing.T) {
	t.Parallel()
	town, db, lister := seatTown(t, 25)
	pool := seatPool("deepseek-flash", 30)

	perSeat := newCountingStore(db)
	want, err := poolSeatSessionsWith(lister, town, poolDispositionAnswering(town, seatReadsOver(perSeat)), nil, pool)
	if err != nil {
		t.Fatalf("per-seat roster: %v", err)
	}

	store := newCountingStore(db)
	reads := newSpecTickReads()
	snap, err := specRosterSnapshotWith(reads, town, pool, lister, func(string) beads.Client { return store }, time.Now)
	if err != nil {
		t.Fatalf("roster: %v", err)
	}
	got := snap.sessions

	sortSeats := func(s []poolSession) {
		sort.Slice(s, func(i, j int) bool { return s[i].name < s[j].name })
	}
	sortSeats(got)
	sortSeats(want)
	if len(got) != len(want) {
		t.Fatalf("roster = %+v, want the per-seat read's %+v", got, want)
	}
	for i := range got {
		if got[i].name != want[i].name || got[i].agent != want[i].agent || got[i].deadHooked != want[i].deadHooked {
			t.Errorf("seat %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if n := poolSeatCount("deepseek-flash", got); n != 4 {
		t.Fatalf("seat count = %d, want 3 live and 1 dead-hooked", n)
	}

	reached := len(store.multis) + store.lists + store.assigned
	for _, n := range store.shows {
		reached += n
	}
	if reads.Calls() != reached {
		t.Errorf("tick cost counts %d reads, the store saw %d", reads.Calls(), reached)
	}
	if store.assigned != 0 || store.lists != 1 {
		t.Errorf("assigned-work reads = %d lists and %d per-polecat, want one list", store.lists, store.assigned)
	}
}
