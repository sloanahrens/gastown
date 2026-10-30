package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/dispatch"
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
	// children counts the per-bead children reads, so a test can hold a scan's
	// cost to the bd calls that reached the store (gt-km9jt).
	children int
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

func (c *countingStore) Children(parentID string) ([]*beads.Issue, error) {
	c.children++
	return c.Client.Children(parentID)
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

// countingClient is a beads.Client that counts the reads which reach it, so a
// test can hold the ledger's count to the calls the store answered.
type countingClient struct {
	beads.Client
	mu sync.Mutex
	n  int
}

func (c *countingClient) count() {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
}

func (c *countingClient) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func (c *countingClient) Show(id string) (*beads.Issue, error) {
	c.count()
	return &beads.Issue{ID: id}, nil
}

func (c *countingClient) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	c.count()
	out := make(map[string]*beads.Issue, len(ids))
	for _, id := range ids {
		out[id] = &beads.Issue{ID: id}
	}
	return out, nil
}

// A ledger read concurrently (a show beside a ShowMultiple, as two passes of a
// tick can) is race-free, and every count it makes is a call the store
// answered: the shows map and the per-phase counters are read and written under
// the same lock (gt-139dp).
func TestSpecTickReadsAreSafeUnderConcurrentReads(t *testing.T) {
	t.Parallel()
	reads := newSpecTickReads()
	reads.setPhase(specPhaseCandidates)
	store := &countingClient{}
	client := reads.client(store)

	const n = 64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			if _, err := client.Show(fmt.Sprintf("gt-show-%d", i)); err != nil {
				t.Errorf("Show: %v", err)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			if _, err := client.ShowMultiple([]string{"gt-shared", fmt.Sprintf("gt-batch-%d", i)}); err != nil {
				t.Errorf("ShowMultiple: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if got, want := reads.Calls(), store.calls(); got != want {
		t.Errorf("ledger counted %d calls, want the store's %d", got, want)
	}
	if got, want := reads.endPhase(), store.calls(); got != want {
		t.Errorf("candidates phase counted %d calls, want the store's %d", got, want)
	}
}

// A bd call is counted under the phase that was active when it was made: the
// roster's reads count as roster, the board's as board, and a call made with no
// phase current counts in the total alone (gt-139dp).
func TestSpecTickReadsCountsEachCallUnderItsPhase(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	db.Seed(beads.Issue{ID: "gt-a"})
	reads := newSpecTickReads()

	reads.setPhase(specPhaseRoster)
	reads.call()
	reads.call()
	if got := reads.endPhase(); got != 2 {
		t.Errorf("roster phase counted %d calls, want 2", got)
	}
	// Between phases nothing is current, so this one is no phase's.
	reads.call()
	reads.setPhase(specPhaseBoard)
	if _, err := reads.client(db).Show("gt-a"); err != nil {
		t.Fatalf("Show: %v", err)
	}
	if got := reads.endPhase(); got != 1 {
		t.Errorf("board phase counted %d calls, want 1", got)
	}
	// A phase that ended holds its count, not the calls made after it.
	if got := reads.endPhase(); got != 0 {
		t.Errorf("an ended phase counted %d more calls, want 0", got)
	}
	if got := reads.Calls(); got != 4 {
		t.Errorf("Calls = %d, want the four calls of the tick", got)
	}
}

// The tick reports each phase's bd calls through its ledger, counted as the
// phase's own wiring made them (gt-139dp).
func TestSpecDispatchReportsItsPhaseCalls(t *testing.T) {
	t.Parallel()
	f := newFakeSpecTown(cleanSpec("gt-a", 2, "2026-09-29T10:00:00Z"))
	env := f.env()
	reads := newSpecTickReads()
	env.Reads = reads
	roster, board, children := env.Roster, env.Candidates, env.Children
	env.Roster = func() (specRoster, error) {
		reads.call()
		reads.call()
		return roster()
	}
	env.Candidates = func() specBoardRead {
		reads.call()
		return board()
	}
	env.Children = func(id string) ([]specdispatch.Child, error) {
		reads.call()
		return children(id)
	}

	r := runSpecDispatchCycle(env)

	if len(r.Dispatched) != 1 {
		t.Fatalf("dispatched %d, want the one clean candidate slung", len(r.Dispatched))
	}
	for _, c := range []struct {
		phase string
		got   int
		want  int
	}{
		{specPhaseRoster, r.RosterCalls, 2},
		{specPhaseBoard, r.BoardCalls, 1},
		// gt-a's one children read; the candidate loop made no other.
		{specPhaseCandidates, r.CandidatesCalls, 1},
	} {
		if c.got != c.want {
			t.Errorf("%s phase counted %d calls, want %d", c.phase, c.got, c.want)
		}
	}
	// A call after the tick's last phase is no phase's, and still the tick's.
	reads.call()
	if got, want := reads.Calls(), r.RosterCalls+r.BoardCalls+r.CandidatesCalls+1; got != want {
		t.Errorf("ledger counted %d calls, want the phases' %d and the one after them", got, want)
	}
}

// specTickTestMu serializes the tests that run a whole tick, and every such
// test goes through runSpecTickForTest or runSpecTickAtForTest below. A tick
// defaults the package's sling actor on its first call, so two ticks in one
// test process would race on it; the process that runs ticks runs one at a
// time.
var specTickTestMu sync.Mutex

// runSpecTickForTest runs one tick with the tests' ticks serialized.
func runSpecTickForTest(t *testing.T, root, owner string, dryRun bool) (specDispatchReport, error) {
	t.Helper()
	specTickTestMu.Lock()
	defer specTickTestMu.Unlock()
	return runSpecDispatchAtWith(root, owner, dryRun)
}

// runSpecTickAtForTest is runSpecDispatchAt with the tests' ticks serialized.
func runSpecTickAtForTest(t *testing.T, townRoot string) (specDispatchReport, error) {
	t.Helper()
	specTickTestMu.Lock()
	defer specTickTestMu.Unlock()
	return runSpecDispatchAt(townRoot)
}

// A tick started by a daemon writes its bd census rows to the town's census
// file, which the daemon logs; a tick run by hand has no owner to write for,
// and a dry run is not the tick the daemon reports, so neither writes
// (gt-139dp).
func TestSpecDispatchAtWritesTheCensusOfADaemonsTick(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		owner  string
		dryRun bool
		writes bool
	}{
		{"a daemon's tick", "123.456", false, true},
		{"a tick run by hand", "", false, false},
		{"a dry run", "123.456", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := specTickTownWithHold(t)
			if _, err := runSpecTickForTest(t, root, tc.owner, tc.dryRun); err != nil {
				t.Fatalf("runSpecTickForTest: %v", err)
			}
			_, err := beads.ReadCensusSink(dispatch.SpecCensusPath(root))
			switch {
			case tc.writes && err != nil:
				t.Errorf("no census at the town's path: %v", err)
			case !tc.writes && !errors.Is(err, fs.ErrNotExist):
				t.Errorf("ReadCensusSink = %v, want the file left absent", err)
			}
		})
	}
}

// A census the tick cannot write is an error in its report, not a tick that
// reads as having made no bd calls (gt-139dp).
func TestSpecDispatchAtReportsACensusItCouldNotWrite(t *testing.T) {
	t.Parallel()
	root := specTickTownWithHold(t)
	// A regular file where the town's daemon directory goes stops the write.
	if err := os.WriteFile(filepath.Join(root, "daemon"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := runSpecTickForTest(t, root, "123.456", false)
	if err != nil {
		t.Fatalf("runSpecDispatchAtWith: %v", err)
	}
	if want := "bd census rows not written"; !strings.Contains(strings.Join(report.Errors, "\n"), want) {
		t.Errorf("report errors = %v, want one naming the lost census", report.Errors)
	}
}

// specTickTownWithHold is a town the tick runs on with the operator's hold in
// place: the hold stops the tick before it reads the roster, so a test of the
// tick's own wiring needs no tmux server.
func specTickTownWithHold(t *testing.T) string {
	t.Helper()
	root := poolTestTown(t, specTickSettings, specTickDaemon)
	if err := os.WriteFile(dispatch.HoldFilePath(root), nil, 0o644); err != nil {
		t.Fatalf("planting the operator hold: %v", err)
	}
	return root
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
