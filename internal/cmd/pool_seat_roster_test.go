package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// rosterStartWait bounds a test's wait for the roster read to reach a fixture's
// checkpoint: another rig's read starting, or the rig the fixture holds back.
// It is a failure bound, not a duration a test measures.
const rosterStartWait = 10 * time.Second

// rosterSerialWait bounds a rig's read that waits for another rig's read to
// start. The concurrent read answers well inside it; a serial read never does,
// so the fake store reports the wait by name.
const rosterSerialWait = 25 * time.Millisecond

// rosterDoorWait bounds the check that no rig's read starts while the fan-out's
// slots are all held: a read past the bound waits at the semaphore's door, so it
// cannot reach the store before a slot frees. It is a door wait, not a duration
// a test measures.
const rosterDoorWait = 250 * time.Millisecond

// rosterTown writes a town of the named rigs, each holding one polecat
// directory per name, with one agent bead per polecat in that rig's own beads
// database, and returns the database of each rig plus a lister that sees no
// sessions. rigs.json is written by writeTestRigsConfig, so the rig registry the
// roster reads names them.
func rosterTown(t *testing.T, rigs map[string][]string) (string, map[string]*beadsfake.Fake, *fakeLister) {
	t.Helper()
	names := make([]string, 0, len(rigs))
	for name := range rigs {
		names = append(names, name)
	}
	sort.Strings(names)
	town := t.TempDir()
	writeTestRigsConfig(t, town, names...)
	dbs := make(map[string]*beadsfake.Fake, len(rigs))
	for _, rigName := range names {
		db := beadsfake.New()
		dbs[rigName] = db
		for _, polecatName := range rigs[rigName] {
			writeTownPolecatDir(t, town, rigName, polecatName)
			seedPolecatAgentBead(db, town, rigName, polecatName, "")
		}
	}
	return town, dbs, &fakeLister{sessions: map[string]map[string]string{}}
}

// rosterBatch is the batch the roster read runs over: each rig's store from
// store, read at most bound rigs at a time.
func rosterBatch(town string, store func(rigName string) beads.Client, bound int) *poolSeatBatch {
	batch := newPoolSeatBatch(town, store, func(bd beads.Client, id string) (*beads.Issue, error) { return bd.Show(id) })
	batch.bound = bound
	return batch
}

// rendezvousStore is one rig's store whose work list waits for another rig's
// work list to start, so the roster can only pass the wait by reading two rigs
// at once. A serial read never gets the second start and the store reports the
// wait it was held at, naming it.
type rendezvousStore struct {
	beads.Client
	rig      string
	started  chan<- string
	release  <-chan struct{}
	wait     time.Duration
	inFlight *inFlightCounter

	mu     sync.Mutex
	heldAt error
}

func (s *rendezvousStore) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	if s.inFlight != nil {
		s.inFlight.enter()
		defer s.inFlight.exit()
	}
	s.started <- s.rig
	select {
	case <-s.release:
		return s.Client.List(opts)
	case <-time.After(s.wait): //testpolicy:allow no-sleep — the wait is the serial read's failing assertion, not a sleep: the concurrent read releases this rig from another goroutine and never reaches it, and a serial read must be held somewhere to have a bound to name
		// The rigs are read one after another: this rig's read started and no
		// other rig's did within the wait.
		err := fmt.Errorf("rig %s: no other rig's read started within %s", s.rig, s.wait)
		s.mu.Lock()
		s.heldAt = err
		s.mu.Unlock()
		return nil, err
	}
}

func (s *rendezvousStore) heldAtError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.heldAt
}

// inFlightCounter counts the work lists read at once and remembers the peak.
type inFlightCounter struct {
	mu   sync.Mutex
	now  int
	peak int
}

func (c *inFlightCounter) enter() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now++
	if c.now > c.peak {
		c.peak = c.now
	}
}

func (c *inFlightCounter) exit() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now--
}

func (c *inFlightCounter) max() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.peak
}

// assertNoRigReadStarts fails if a rig's read starts while every fan-out slot is
// held. It releases the rigs' reads before failing, so the roster read it guards
// is not left blocked.
func assertNoRigReadStarts(t *testing.T, started <-chan string, release chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), rosterDoorWait)
	defer cancel()
	select {
	case rigName := <-started:
		close(release)
		t.Fatalf("rig %s's read started while %d were in flight: the fan-out is not bounded at %d", rigName, poolSeatRigReads, poolSeatRigReads)
	case <-ctx.Done():
	}
}

// blockingStore is one rig's store whose agent-bead batch waits for the test to
// release it, so the test can ask the batch about a bead while that rig's read
// is still filling it.
type blockingStore struct {
	beads.Client
	entered chan<- struct{}
	release <-chan struct{}
}

func (s *blockingStore) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	s.entered <- struct{}{}
	<-s.release
	return s.Client.ShowMultiple(ids)
}

// TestRosterReadsRigsConcurrently is gt-0mzpm: the spec tick's roster read
// takes its per-rig reads at once, bounded by poolSeatRigReads. It drives
// specRosterSnapshotWith, the wiring the tick's roster step calls, so deleting
// the fan-out from that path turns it red. The fixtures rendezvous inside the
// fake stores, so no duration is measured: two rigs' work lists can only both
// start if the reads overlap.
func TestRosterReadsRigsConcurrently(t *testing.T) {
	t.Parallel()
	town, dbs, lister := rosterTown(t, map[string][]string{
		"arig": {"p1"}, "brig": {"p1"}, "crig": {"p1"},
		"drig": {"p1"}, "erig": {"p1"}, "frig": {"p1"},
	})
	pool := seatPool("deepseek-flash", 10)
	const bound = poolSeatRigReads

	started := make(chan string, len(dbs))
	release := make(chan struct{})
	count := &inFlightCounter{}
	// A rendezvous store per rig, keyed by the directory the roster asks the
	// batch for. The town's own directory gets a plain store: this fixture's
	// landing read never asks it for a bead.
	stores := make(map[string]*rendezvousStore, len(dbs))
	for rigName, db := range dbs {
		stores[filepath.Join(town, rigName)] = &rendezvousStore{Client: db, rig: rigName, started: started, release: release, wait: rosterStartWait, inFlight: count}
	}
	townStore := beadsfake.New()
	storeFor := func(dir string) beads.Client {
		if store, ok := stores[dir]; ok {
			return store
		}
		return townStore
	}
	done := make(chan error, 1)
	go func() {
		_, err := specRosterSnapshotWith(newSpecTickReads(), town, pool, lister, storeFor, time.Now)
		done <- err
	}()
	for i := 0; i < bound; i++ {
		select {
		case <-started:
		case <-time.After(rosterStartWait): //testpolicy:allow no-sleep — a hang guard only: the concurrent read signals every start on a channel, so this fires only when the read is serial, and the test fails naming the wait instead of hanging the suite
			close(release)
			t.Fatalf("%d of %d rig reads started within %s: the roster read the rigs one after another", i, bound, rosterStartWait)
		}
	}
	assertNoRigReadStarts(t, started, release)
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("roster read: %v", err)
	}
	if got := count.max(); got != bound {
		t.Errorf("peak rig reads in flight = %d, want the bound %d", got, bound)
	}
	for _, store := range stores {
		if err := store.heldAtError(); err != nil {
			t.Errorf("rig %s was held at a wait while the reads overlapped: %v", store.rig, err)
		}
	}
}

// TestRosterSerialReadIsHeldAtItsWait is the fixture's control for
// TestRosterReadsRigsConcurrently: the same rendezvous stores read one rig at a
// time are held at the rig's wait, so that test passes only because the roster
// overlapped the reads. It drives the roster at a bound of one, which the
// tick's own wiring does not offer.
func TestRosterSerialReadIsHeldAtItsWait(t *testing.T) {
	t.Parallel()
	town, dbs, lister := rosterTown(t, map[string][]string{"arig": {"p1"}, "brig": {"p1"}})
	pool := seatPool("deepseek-flash", 10)

	started := make(chan string, len(dbs))
	release := make(chan struct{}) // never closed: one rig read at a time
	stores := make(map[string]*rendezvousStore, len(dbs))
	for rigName, db := range dbs {
		stores[rigName] = &rendezvousStore{Client: db, rig: rigName, started: started, release: release, wait: rosterSerialWait}
	}
	batch := rosterBatch(town, func(rigName string) beads.Client { return stores[rigName] }, 1)
	if _, err := rosterSnapshotOn(batch, nil, town, pool, lister, time.Now); err != nil {
		t.Fatalf("serial roster read: %v", err)
	}
	held := 0
	for _, store := range stores {
		err := store.heldAtError()
		if err == nil {
			continue
		}
		held++
		if !strings.Contains(err.Error(), "no other rig's read started") ||
			!strings.Contains(err.Error(), store.rig) {
			t.Errorf("the wait a serial read is held at = %q, want the wait named with rig %s", err, store.rig)
		}
	}
	if held == 0 {
		t.Fatalf("no rig's read was held at its wait: the read overlapped with bound 1")
	}
}

// TestRosterResultEqualsTheSequentialRead is gt-0mzpm: the same fixture read
// with bound 1 and with bound 4 gives the same seat picture. The seats, their
// agents and the seats the read released must not depend on the order the rigs
// are read in.
func TestRosterResultEqualsTheSequentialRead(t *testing.T) {
	t.Parallel()
	town, dbs, lister := rosterTown(t, map[string][]string{
		"arig": {"p01", "p02", "p03"},
		"brig": {"p01", "p02"},
		"crig": {"p01"},
	})
	// One live session in arig, one in brig, none in crig. No other polecat has
	// a session: arig's p02 is a dead-hooked seat (a hooked agent bead with
	// hooked work), the rest hold nothing.
	lister.sessions["ar-p01"] = map[string]string{"GT_ROLE": "arig/polecats/p01", "GT_AGENT": "deepseek-flash"}
	lister.sessions["br-p01"] = map[string]string{"GT_ROLE": "brig/polecats/p01", "GT_AGENT": "deepseek-flash"}
	// A session age the lister knows, so the seats' creation times are the
	// fixture's and not the clock the read happens to start at.
	lister.created = map[string]time.Time{
		"ar-p01": time.Date(2026, 11, 1, 11, 0, 0, 0, time.UTC),
		"br-p01": time.Date(2026, 11, 1, 11, 0, 0, 0, time.UTC),
	}
	seedPolecatAgentBead(dbs["arig"], town, "arig", "p02", "ar-hooked")
	dbs["arig"].Seed(beads.Issue{ID: "ar-hooked", Status: beads.StatusHooked, Assignee: "arig/polecats/p02"})
	pool := seatPool("deepseek-flash", 10)
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

	read := func(bound int) poolSeatSnapshot {
		t.Helper()
		batch := rosterBatch(town, func(rigName string) beads.Client { return dbs[rigName] }, bound)
		snap, err := rosterSnapshotOn(batch, nil, town, pool, lister, func() time.Time { return at })
		if err != nil {
			t.Fatalf("roster read at bound %d: %v", bound, err)
		}
		return snap
	}
	serial := read(1)
	concurrent := read(4)

	sorted := func(sessions []poolSession) []poolSession {
		out := append([]poolSession(nil), sessions...)
		sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
		return out
	}
	if !reflect.DeepEqual(sorted(serial.sessions), sorted(concurrent.sessions)) {
		t.Errorf("seats at bound 4 = %+v, want bound 1's %+v", concurrent.sessions, serial.sessions)
	}
	if !reflect.DeepEqual(serial.released, concurrent.released) {
		t.Errorf("released seats at bound 4 = %v, want bound 1's %v", concurrent.released, serial.released)
	}
	if serial.at != concurrent.at {
		t.Errorf("the snapshot was read at %s and %s, want one clock", concurrent.at, serial.at)
	}
}

// TestRosterErrorIsTheFirstRigInNameOrder is gt-0mzpm: when more than one rig
// cannot be read, the roster reports the first rig in name order, as the
// sequential walk does, whatever order the concurrent reads finish in. It drives
// the tick's own roster wiring. The first rig's read is held until every other
// rig's store has been built, so the other rigs' reads have all started before
// its own: a roster that reported the first error to finish would report one of
// theirs. The hold is bounded, so a roster that read the rigs one after another
// fails naming the wait instead of hanging.
func TestRosterErrorIsTheFirstRigInNameOrder(t *testing.T) {
	t.Parallel()
	town, dbs, lister := rosterTown(t, map[string][]string{
		"arig": {"p1"}, "brig": {"p1"}, "crig": {"p1"}, "drig": {"p1"},
	})
	for rigName := range dbs {
		// A polecats entry that is a file cannot be listed: every rig fails.
		polecats := filepath.Join(town, rigName, "polecats")
		if err := os.RemoveAll(polecats); err != nil {
			t.Fatalf("clear %s: %v", polecats, err)
		}
		if err := os.WriteFile(polecats, []byte("not a directory"), 0o644); err != nil {
			t.Fatalf("write %s: %v", polecats, err)
		}
	}
	pool := seatPool("deepseek-flash", 10)

	rigOfDir := make(map[string]string, len(dbs))
	for rigName := range dbs {
		rigOfDir[filepath.Join(town, rigName)] = rigName
	}
	townStore := beadsfake.New()
	others := len(dbs) - 1
	var othersMade atomic.Int32
	var heldOut atomic.Bool
	allOthersMade := make(chan struct{})
	storeFor := func(dir string) beads.Client {
		rigName, isRig := rigOfDir[dir]
		switch {
		case !isRig:
			return townStore
		case rigName == "arig":
			select {
			case <-allOthersMade:
			case <-time.After(rosterStartWait): //testpolicy:allow no-sleep — a hang guard: a roster that reads the rigs at once builds every other rig's store first, so this bound turns a serial read into a named failure instead of a hang
				heldOut.Store(true)
			}
		default:
			if othersMade.Add(1) == int32(others) {
				close(allOthersMade)
			}
		}
		return dbs[rigName]
	}

	_, err := specRosterSnapshotWith(newSpecTickReads(), town, pool, lister, storeFor, time.Now)
	if heldOut.Load() {
		t.Fatalf("arig's store was built before every other rig's within %s: no other rig's read was in flight with arig's", rosterStartWait)
	}
	if err == nil {
		t.Fatalf("roster read a town whose rigs cannot be listed")
	}
	if !strings.HasPrefix(err.Error(), "listing polecats in arig:") {
		t.Errorf("roster error = %q, want the first rig in name order (arig)", err)
	}
}

// TestRosterHeldIssueSkipsARigWhoseReadIsInFlight is gt-0mzpm: the batch answers
// heldIssue only from a rig whose read has finished. The rig's batch already
// holds a bead when its agent-bead read blocks, so a heldIssue that looked at
// the rig mid-read would answer from a batch the snapshot does not have yet.
func TestRosterHeldIssueSkipsARigWhoseReadIsInFlight(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTestRigsConfig(t, town, "arig")
	writeTownPolecatDir(t, town, "arig", "p1")
	db := beadsfake.New()
	db.Seed(beads.Issue{ID: "ar-held", Status: string(beads.StatusOpen), Assignee: "arig/polecats/p1"})
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	store := &blockingStore{Client: db, entered: entered, release: release}
	batch := newPoolSeatBatch(town, func(string) beads.Client { return store },
		func(bd beads.Client, id string) (*beads.Issue, error) { return bd.Show(id) })
	// The landing read answers from the batch first and asks the town only for a
	// bead the batch does not hold, so a town bead it returns is one the batch
	// did not answer.
	work := batch.landingWork(beadsfake.New())
	done := make(chan struct{})
	go func() {
		batch.rig("arig")
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(rosterStartWait): //testpolicy:allow no-sleep — a hang guard only: the store signals the moment the read reaches its blocked agent-bead batch, so this fires only when the read never gets there
		close(release)
		t.Fatalf("the rig's read did not reach its blocked agent-bead batch within %s", rosterStartWait)
	}
	held, err := work([]string{"ar-held"})
	if err != nil {
		t.Fatalf("landing read while the rig's read was in flight: %v", err)
	}
	if held["ar-held"] != nil {
		t.Errorf("the landing read answered a bead from a rig whose read was still in flight")
	}
	close(release)
	select {
	case <-done:
	case <-time.After(rosterStartWait): //testpolicy:allow no-sleep — a hang guard only: the released read finishes at once, so this fires only when it never does
		t.Fatalf("the rig's read did not finish within %s", rosterStartWait)
	}
	held, err = work([]string{"ar-held"})
	if err != nil {
		t.Fatalf("landing read after the rig's read: %v", err)
	}
	if held["ar-held"] == nil {
		t.Fatalf("the landing read could not answer a bead the finished rig's batch holds")
	}
	if held["ar-held"].ID != "ar-held" {
		t.Errorf("the landing read answered %q, want ar-held", held["ar-held"].ID)
	}
}

// TestRosterRefusesARigRegistryItCannotRead: a registry that cannot be read is
// not a town of no rigs. The roster refuses rather than counting the live
// sessions alone as if the town had no dead seats. The refusal predates the
// fan-out — the dead-hooked walk that reads every rig already returned the same
// registry error — and this pins that the roster's new first read does not
// swallow it.
func TestRosterRefusesARigRegistryItCannotRead(t *testing.T) {
	t.Parallel()
	town, dbs, lister := rosterTown(t, map[string][]string{"arig": {"p1"}})
	if err := os.WriteFile(filepath.Join(town, "mayor", "rigs.json"), []byte("{"), 0o644); err != nil {
		t.Fatalf("write rigs.json: %v", err)
	}
	pool := seatPool("deepseek-flash", 10)
	batch := rosterBatch(town, func(rigName string) beads.Client { return dbs[rigName] }, 4)
	_, err := rosterSnapshotOn(batch, nil, town, pool, lister, time.Now)
	if err == nil {
		t.Fatalf("roster read a town whose rig registry cannot be read")
	}
	if !strings.Contains(err.Error(), "rig registry") {
		t.Errorf("roster error = %q, want the unreadable rig registry", err)
	}
}
