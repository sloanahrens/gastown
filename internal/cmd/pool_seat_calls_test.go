package cmd

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/land"
)

// rosterCallsAtTwentySeats is the number of bd read calls the dispatcher's
// roster made over the twenty-seat fixture at d6851c36e, the commit before
// gt-4efmk: one ShowMultiple asking for the twenty agent beads and one List
// asking for the rig's assigned work. The fixture is seatTown(t, 20) — the same
// twenty polecat directories, three of them with a live session and one
// dead-hooked — and the count is what the store answered, taken with
// TestSpecRosterReadsTwentySeatsInHalfTheBaseline below.
const rosterCallsAtTwentySeats = 2

// countingCalls is the calls a countingStore was asked for: every read a bd
// subprocess would have been spent on.
func countingCalls(store *countingStore) int {
	total := len(store.multis) + store.lists + store.assigned
	for _, n := range store.shows {
		total += n
	}
	return total
}

// TestSpecRosterReadsTwentySeatsInHalfTheBaseline is gt-4efmk: the roster over
// the twenty-seat fixture costs at most half the bd calls it cost before, and
// the seats it counts are the per-seat read's. One assigned-work list per rig
// answers the agent beads it carries, where the roster used to read them in a
// second batch of its own.
func TestSpecRosterReadsTwentySeatsInHalfTheBaseline(t *testing.T) {
	t.Parallel()
	town, db, lister := seatTown(t, 20)
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
	assertSameSeats(t, snap.sessions, want)

	got := countingCalls(store)
	t.Logf("the roster spent %d bd calls over the twenty-seat fixture; it spent %d at d6851c36e", got, rosterCallsAtTwentySeats)
	if got*2 > rosterCallsAtTwentySeats {
		t.Errorf("roster spent %d bd calls over the twenty-seat fixture, want at most half of the %d it spent at d6851c36e",
			got, rosterCallsAtTwentySeats)
	}
	if reads.Calls() != got {
		t.Errorf("the tick's ledger counted %d calls, the store saw %d", reads.Calls(), got)
	}
}

// seatShape is one polecat of seatShapeTown: whether it has a live session, the
// hook and last source issue its agent bead names, the status its agent bead is
// filed in ("-" for no bead at all), and one bead assigned to it.
type seatShape struct {
	name           string
	live           bool
	hook           string
	hookStatus     string
	hint           string
	agentStatus    string
	assigned       string
	assignedStatus string
}

// seatShapeTown is a rig of polecats in the shapes a roster has to tell apart.
// A hook or hint bead with an empty status is left absent, as a reference to a
// bead that no longer exists is.
func seatShapeTown(t *testing.T, shapes []seatShape) (string, *beadsfake.Fake, *fakeLister) {
	t.Helper()
	town := t.TempDir()
	writeTestRigsConfig(t, town, "gastown")
	db := beadsfake.New()
	lister := &fakeLister{sessions: map[string]map[string]string{}}
	prefix := beads.GetPrefixForRig(town, "gastown")
	for _, s := range shapes {
		writeTownPolecatDir(t, town, "gastown", s.name)
		if s.live {
			lister.sessions["gt-"+s.name] = map[string]string{"GT_ROLE": "gastown/polecats/" + s.name, "GT_AGENT": "deepseek-flash"}
		}
		if s.agentStatus != "-" {
			id := beads.PolecatBeadIDWithPrefix(prefix, "gastown", s.name)
			desc := beads.FormatAgentDescription(id, &beads.AgentFields{
				RoleType: "polecat", Rig: "gastown", AgentState: "idle",
				HookBead: s.hook, LastSourceIssue: s.hint,
			})
			issue := beads.Issue{ID: id, Title: id, Type: "task", Labels: []string{"gt:agent"}, Description: desc, Status: s.agentStatus}
			db.Seed(issue)
		}
		if s.hook != "" && s.hookStatus != "" {
			db.Seed(beads.Issue{ID: s.hook, Status: s.hookStatus})
		}
		if s.hint != "" {
			db.Seed(beads.Issue{ID: s.hint, Status: string(beads.StatusClosed)})
		}
		if s.assigned != "" {
			db.Seed(beads.Issue{ID: s.assigned, Status: s.assignedStatus, Assignee: "gastown/polecats/" + s.name})
		}
	}
	return town, db, lister
}

// TestSpecRosterSeatDispositionsAreUnchanged is gt-4efmk's constraint: the
// roster answers a seat's state the way the per-seat read does, whatever shape
// the seat is in. Each row puts a polecat in one state the decision turns on —
// a live session, a hooked bead, an assigned bead, a closed or absent hook, a
// closed source issue, an agent bead filed outside the work list, or no agent
// bead at all — and holds the batched read to the per-seat read's answer.
func TestSpecRosterSeatDispositionsAreUnchanged(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		shapes []seatShape
	}{
		{"a live seat on a hooked bead", []seatShape{
			{name: "p01", live: true, hook: "gt-hooked", hookStatus: beads.StatusHooked},
		}},
		{"an idle seat with nothing live", []seatShape{
			{name: "p02"},
		}},
		{"a seat whose assigned bead is in progress", []seatShape{
			{name: "p03", assigned: "gt-prog", assignedStatus: string(beads.StatusInProgress)},
		}},
		{"a seat whose assigned bead is closed", []seatShape{
			{name: "p04", assigned: "gt-done", assignedStatus: string(beads.StatusClosed)},
		}},
		{"a seat whose hook bead is closed", []seatShape{
			{name: "p05", hook: "gt-closed-hook", hookStatus: string(beads.StatusClosed)},
		}},
		{"a seat whose hook bead is absent", []seatShape{
			{name: "p06", hook: "gt-absent"},
		}},
		{"a seat whose last source issue is closed", []seatShape{
			{name: "p07", hint: "gt-old-p07"},
		}},
		{"a seat whose agent bead is filed pinned", []seatShape{
			{name: "p08", agentStatus: string(beads.StatusPinned), assigned: "gt-prog-p08", assignedStatus: string(beads.StatusInProgress)},
		}},
		{"a seat with no agent bead", []seatShape{
			{name: "p09", agentStatus: "-"},
		}},
		{"a live seat whose agent bead is absent", []seatShape{
			{name: "p10", live: true, agentStatus: "-"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town, db, _ := seatShapeTown(t, tc.shapes)
			names := make([]string, 0, len(tc.shapes))
			for _, s := range tc.shapes {
				names = append(names, s.name)
			}
			assertSameSeatVerdicts(t, town, db, names)
		})
	}
}

// TestSpecRosterFailsOnALandingReadItLoses is the failure branch of the
// landing read: a landing bead the batch does not hold is read by town, and a
// town read that fails fails the roster rather than reading the seat as free.
func TestSpecRosterFailsOnALandingReadItLoses(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTestRigsConfig(t, town, "gastown")
	writeLandingSeat(t, town, "gastown", "ruby", "gt-ruby")
	db := beadsfake.New()
	seedPolecatAgentBead(db, town, "gastown", "ruby", "")
	db.Seed(beads.Issue{ID: "gt-ruby", Status: string(beads.StatusPinned), Labels: []string{land.LabelReadyToLand}})
	store := &sourceBatchFails{countingStore: newCountingStore(db)}

	_, err := specRosterSnapshotWith(newSpecTickReads(), town, seatPool("deepseek-flash", 30),
		&fakeLister{sessions: map[string]map[string]string{}}, func(string) beads.Client { return store }, time.Now)
	if err == nil || !strings.Contains(err.Error(), "reading landing seats' work beads") {
		t.Fatalf("roster = %v, want the failed landing read reported", err)
	}
}

// TestSeatBatchReadsALostAgentBeadLive is the failure branch of the batch's
// agent read: an agent bead the work list does not carry is asked for in one
// batch, and a batch that fails leaves that bead to the live per-seat read
// rather than answering it as a bead that does not exist. A seat filed pinned
// is the case — the list never carries one — and its disposition is still the
// per-seat read's (gt-4efmk).
func TestSeatBatchReadsALostAgentBeadLive(t *testing.T) {
	t.Parallel()
	town, db, _ := seatShapeTown(t, []seatShape{
		{name: "p01", assigned: "gt-prog-p01", assignedStatus: string(beads.StatusInProgress)},
		{name: "p02", agentStatus: string(beads.StatusPinned), assigned: "gt-prog-p02", assignedStatus: string(beads.StatusInProgress)},
	})
	perSeat := poolDispositionAnswering(town, seatReadsOver(newCountingStore(db)))
	store := &sourceBatchFails{countingStore: newCountingStore(db)}
	batched := batchOverStore(town, store).disposition

	for _, name := range []string{"p01", "p02"} {
		want, err := perSeat("gastown", name)
		if err != nil {
			t.Fatalf("per-seat read of %s: %v", name, err)
		}
		got, err := batched("gastown", name)
		if err != nil {
			t.Fatalf("batched read of %s: %v", name, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("seat %s = %+v, want the per-seat read's %+v", name, got, want)
		}
	}
	// The bead the work list answered is never read live; the one it could not
	// answer is, once.
	pinned := beads.PolecatBeadIDWithPrefix(beads.GetPrefixForRig(town, "gastown"), "gastown", "p02")
	listed := beads.PolecatBeadIDWithPrefix(beads.GetPrefixForRig(town, "gastown"), "gastown", "p01")
	if store.shows[pinned] != 1 {
		t.Errorf("pinned agent bead read live %d times, want once (%v)", store.shows[pinned], store.shows)
	}
	if store.shows[listed] != 0 {
		t.Errorf("listed agent bead read live %d times, want none: the work list answered it", store.shows[listed])
	}
}

// TestSpecRosterReadsALandingSeatFromTheBatch is gt-4efmk's second cut: a seat
// mid-landing is confirmed from the batch the roster already made when the
// bead is live work in the rig's list, and the town read is spent only for a
// bead that list does not carry — one filed pinned, or slung from the town. The
// seat counts as held either way.
func TestSpecRosterReadsALandingSeatFromTheBatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status string
		town   int // the town-wide ShowMultiple calls the read costs
	}{
		{"a landing bead the work list carries", string(beads.StatusOpen), 0},
		{"a landing bead the work list does not carry", string(beads.StatusPinned), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			writeTestRigsConfig(t, town, "gastown")
			writeLandingSeat(t, town, "gastown", "ruby", "gt-ruby")
			db := beadsfake.New()
			seedPolecatAgentBead(db, town, "gastown", "ruby", "")
			db.Seed(beads.Issue{ID: "gt-ruby", Status: tc.status, Labels: []string{land.LabelReadyToLand}})
			pool := seatPool("deepseek-flash", 30)
			lister := &fakeLister{sessions: map[string]map[string]string{}}

			store := newCountingStore(db)
			snap, err := specRosterSnapshotWith(newSpecTickReads(), town, pool, lister, func(string) beads.Client { return store }, time.Now)
			if err != nil {
				t.Fatalf("roster: %v", err)
			}
			held := 0
			for _, s := range snap.sessions {
				if s.name == "landing/gastown/ruby" && s.landing {
					held++
				}
			}
			if held != 1 {
				t.Fatalf("roster = %+v, want the landing seat held once", snap.sessions)
			}
			if len(store.multis) != tc.town {
				t.Errorf("town-wide batches = %v, want %d", store.multis, tc.town)
			}
		})
	}
}
