package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/land"
)

// rosterCallsBefore is the number of bd reads the unfolded roster makes over
// seatTown(t, 20): one ShowMultiple asking for the twenty agent beads and one
// List asking for the rig's assigned work. rosterCallsWithoutTheFold computes
// the same number by replaying those reads through the counting store, and the
// test fails if the two differ, so the constant is the fixture's baseline and
// the folded roster is held to half of it. The replay is a hand-written copy of
// the old reads, not the old code: the same count was taken from the code at
// d6851c36e, which is where the number comes from.
const rosterCallsBefore = 2

// countingCalls is the calls a countingStore was asked for: every read a bd
// subprocess would have been spent on.
func countingCalls(store *countingStore) int {
	total := len(store.multis) + store.lists + store.assigned
	for _, n := range store.shows {
		total += n
	}
	return total
}

// rosterCallsWithoutTheFold measures the reads the roster made over the fixture
// before the work list answered the agent beads: one ShowMultiple for the rig's
// agent beads and one List for its assigned work, the two batched reads the
// per-rig seat read issued. The fold removes the first of them.
func rosterCallsWithoutTheFold(t *testing.T, db *beadsfake.Fake, town, rig string, names []string) int {
	t.Helper()
	store := newCountingStore(db)
	prefix := beads.GetPrefixForRig(town, rig)
	ids := make([]string, 0, len(names))
	for _, name := range names {
		ids = append(ids, beads.PolecatBeadIDWithPrefix(prefix, rig, name))
	}
	if _, err := store.ShowMultiple(ids); err != nil {
		t.Fatalf("agent batch: %v", err)
	}
	if _, err := store.List(beads.ListOptions{Status: strings.Join(assignedStatuses, ","), Priority: -1}); err != nil {
		t.Fatalf("work list: %v", err)
	}
	return countingCalls(store)
}

// TestSpecRosterReadsTwentySeatsInHalfTheBaseline is gt-4efmk: the roster over
// the twenty-seat fixture costs at most half the bd calls it cost before the
// work list answered the agent beads, and the seats it counts are the per-seat
// read's. The baseline is measured over the same fixture, not trusted from the
// constant, and the constant records the count taken at d6851c36e.
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

	baseline := rosterCallsWithoutTheFold(t, db, town, "gastown", polecatNames(20))
	if baseline != rosterCallsBefore {
		t.Fatalf("the fixture's baseline is %d bd calls, want the %d measured at d6851c36e: the fixture changed under the constant",
			baseline, rosterCallsBefore)
	}
	got := countingCalls(store)
	t.Logf("the roster spent %d bd calls over the twenty-seat fixture, at most half of the %d it spent at d6851c36e",
		got, baseline)
	if got*2 > baseline {
		t.Errorf("roster spent %d bd calls over the twenty-seat fixture, want at most half of the %d it spent at d6851c36e",
			got, baseline)
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

// TestSpecRosterSeatDispositionsAreUnchanged: the roster answers a seat's state
// the way the per-seat read does, whatever shape the seat is in. Each row puts a
// polecat in one state the decision turns on — a live session, a hooked bead, an
// assigned bead, a closed or absent hook, a closed source issue, an agent bead
// filed outside the work list, or no agent bead at all — and holds the batched
// read to the per-seat read's answer (gt-4efmk).
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
	store := &showMultipleFails{countingStore: newCountingStore(db), fail: asksFor("gt-ruby")}

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
// per-seat read's.
func TestSeatBatchReadsALostAgentBeadLive(t *testing.T) {
	t.Parallel()
	town, db, _ := seatShapeTown(t, []seatShape{
		{name: "p01", assigned: "gt-prog-p01", assignedStatus: string(beads.StatusInProgress)},
		{name: "p02", agentStatus: string(beads.StatusPinned), assigned: "gt-prog-p02", assignedStatus: string(beads.StatusInProgress)},
	})
	pinned := beads.PolecatBeadIDWithPrefix(beads.GetPrefixForRig(town, "gastown"), "gastown", "p02")
	perSeat := poolDispositionAnswering(town, seatReadsOver(newCountingStore(db)))
	store := &showMultipleFails{countingStore: newCountingStore(db), fail: asksFor(pinned)}
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
	listed := beads.PolecatBeadIDWithPrefix(beads.GetPrefixForRig(town, "gastown"), "gastown", "p01")
	if store.shows[pinned] != 1 {
		t.Errorf("pinned agent bead read live %d times, want once (%v)", store.shows[pinned], store.shows)
	}
	if store.shows[listed] != 0 {
		t.Errorf("listed agent bead read live %d times, want none: the work list answered it", store.shows[listed])
	}
}

// TestSpecRosterReadsALandingSeatFromTheBatch: a seat mid-landing is confirmed
// from the rig work list the roster already read when the bead is live work in
// it, and the town-wide read is spent only for a bead that list does not carry
// — one filed pinned, or slung from the town. The seat counts as held either
// way, and the rig's own store is never asked for a batch.
func TestSpecRosterReadsALandingSeatFromTheBatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status string
		town   int // the town-wide ShowMultiple calls the landing read costs
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

			stores := map[string]*countingStore{}
			storeFor := func(dir string) beads.Client {
				if _, ok := stores[dir]; !ok {
					stores[dir] = newCountingStore(db)
				}
				return stores[dir]
			}
			snap, err := specRosterSnapshotWith(newSpecTickReads(), town, pool, lister, storeFor, time.Now)
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
			if townStore := stores[town]; len(townStore.multis) != tc.town {
				t.Errorf("town-wide batches = %v, want %d", townStore.multis, tc.town)
			}
			rigStore := stores[filepath.Join(town, "gastown")]
			if len(rigStore.multis) != 0 {
				t.Errorf("rig batches = %v, want none: the rig's work list answers its beads", rigStore.multis)
			}
			if rigStore.lists != 1 {
				t.Errorf("rig work lists = %d, want one", rigStore.lists)
			}
		})
	}
}

// recordedIssues reads one recorded bd read: the issue array bd printed (the
// data of its machine envelope), unmarshalled the way the client parses it and
// keyed by id.
func recordedIssues(t *testing.T, name string) map[string]*beads.Issue {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	var issues []*beads.Issue
	if err := json.Unmarshal(raw, &issues); err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	out := make(map[string]*beads.Issue, len(issues))
	for _, issue := range issues {
		if issue == nil {
			t.Fatalf("%s holds a null issue", name)
		}
		out[issue.ID] = issue
	}
	return out
}

// TestListedAgentBeadCarriesTheShowFields is the work list's contract for the
// roster: the seat read takes an agent bead's id, its gt:agent label, its
// status and the fields its description carries, and a bd list record carries
// all of them, equal to the bd show record of the same bead. The fixtures are
// the shape of a `bd list --status=open,in_progress,hooked --json` read and of
// a `bd show --json` read of the same two beads, with invented ids, rig, agent
// and titles: every value a person wrote is replaced, and only the fields the
// two reads emit are kept. Both go through the read the roster makes of a
// batched bead, beads.GetAgentBead.
func TestListedAgentBeadCarriesTheShowFields(t *testing.T) {
	t.Parallel()
	const agentID, workID = "zz-polecat-alice", "zz-work-42"
	listed := recordedIssues(t, "pool_seat_list.json")
	shown := recordedIssues(t, "pool_seat_show.json")
	for _, id := range []string{agentID, workID} {
		if listed[id] == nil || shown[id] == nil {
			t.Fatalf("the fixtures do not both carry %s (list %v, show %v)", id, listed[id] != nil, shown[id] != nil)
		}
	}

	// The agent bead: every field the seat read takes must be there, and the
	// same, in the list form.
	_, listFields, err := beads.GetAgentBead(heldIssueStore{issue: listed[agentID]}, agentID)
	if err != nil {
		t.Fatalf("parsing the listed agent bead: %v", err)
	}
	_, showFields, err := beads.GetAgentBead(heldIssueStore{issue: shown[agentID]}, agentID)
	if err != nil {
		t.Fatalf("parsing the shown agent bead: %v", err)
	}
	if !listedAgentBeadComplete(listed[agentID]) {
		t.Errorf("the list record of %s is not a whole agent bead; the roster would spend a second read on it", agentID)
	}
	if !reflect.DeepEqual(listFields, showFields) {
		t.Errorf("listed agent fields = %+v, want the shown bead's %+v", listFields, showFields)
	}
	for name, value := range map[string]string{
		"id":          listed[agentID].ID,
		"status":      listed[agentID].Status,
		"role_type":   listFields.RoleType,
		"agent_state": listFields.AgentState,
		"hook_bead":   listFields.HookBead,
		"agent":       listFields.Agent,
	} {
		if strings.TrimSpace(value) == "" {
			t.Errorf("the list record of %s carries no %s", agentID, name)
		}
	}
	if !beads.IsAgentBead(listed[agentID]) {
		t.Errorf("the list record of %s does not read as an agent bead: it carries no gt:agent label", agentID)
	}

	// The assigned work bead: the assignee and status the batch's pick reads
	// are the shown bead's.
	for name, got := range map[string]string{
		"status":   listed[workID].Status,
		"assignee": listed[workID].Assignee,
	} {
		if strings.TrimSpace(got) == "" {
			t.Errorf("the list record of %s carries no %s", workID, name)
		}
	}
	if want := shown[workID].Status; listed[workID].Status != want {
		t.Errorf("listed work status = %q, want the shown bead's %q", listed[workID].Status, want)
	}
	if want := shown[workID].Assignee; listed[workID].Assignee != want {
		t.Errorf("listed work assignee = %q, want the shown bead's %q", listed[workID].Assignee, want)
	}
}

// listedRecord is a bd list record of an agent bead whose description is the
// given text.
func listedRecord(description string) *beads.Issue {
	return &beads.Issue{
		ID: "zz-polecat-alice", Status: "open", Labels: []string{"gt:agent"},
		Description: description,
	}
}

// TestListedAgentBeadCompletenessCatchesATruncatedRecord holds the gate to the
// truncations it can see: a record cut inside a line, cut before a live seat's
// agent line, or cut before the always-written lines is not whole and is read
// by id, and a whole record (an idle seat that ends on notification_level, a
// live seat that ends on its agent line, one that ends on a completion line the
// formatter writes) is. Mutation: dropping the last-line check turns the mid-line
// row red; dropping the live-seat agent check turns the live row red.
func TestListedAgentBeadCompletenessCatchesATruncatedRecord(t *testing.T) {
	t.Parallel()
	const head = "zz-polecat-alice\n\nrole_type: polecat\nrig: zzrig\nagent_state: %s\nhook_bead: %s\ncleanup_status: null\nactive_mr: null\nnotification_level: null"
	idle := fmt.Sprintf(head, "idle", "null")
	live := fmt.Sprintf(head, "working", "zz-work-42")
	cases := []struct {
		name string
		desc string
		want bool
	}{
		{"an idle seat that ends on notification_level", idle, true},
		{"a live seat with its agent line", live + "\nagent: zz-test-agent", true},
		{"a live seat that ends on a completion line the formatter writes", live + "\nagent: zz-test-agent\nexit_type: COMPLETED", true},
		{"a live seat cut before its agent line", live, false},
		{"a record cut inside a line", live + "\nagen", false},
		{"a record cut inside a value line", idle + "\nagent:", false},
		{"a record cut before the always-written lines", "zz-polecat-alice\n\nrole_type: polecat\nrig: zzrig\nagent_state: idle", false},
		{"a record ending on an unknown line", idle + "\nnot_a_key: x", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := listedAgentBeadComplete(listedRecord(c.desc)); got != c.want {
				t.Errorf("listedAgentBeadComplete = %v, want %v for:\n%s", got, c.want, c.desc)
			}
		})
	}
}

// TestRosterAlwaysKeysAreTheFormatterAlwaysKeys: the lines the completeness
// gate treats as always present are the lines beads.FormatAgentDescription
// writes for a description with no optional field set, not a list this file
// asserts. A line the formatter starts or stops writing unconditionally turns
// this red, so the gate cannot drift from it.
func TestRosterAlwaysKeysAreTheFormatterAlwaysKeys(t *testing.T) {
	t.Parallel()
	written := agentDescriptionKeys(beads.FormatAgentDescription("t", &beads.AgentFields{}))
	always := map[string]bool{}
	for _, field := range rosterAgentFields {
		if field.always {
			always[field.key] = true
		}
	}
	if !reflect.DeepEqual(written, always) {
		t.Errorf("the formatter always writes %v, rosterAgentFields marks %v always", written, always)
	}
	for _, field := range rosterAgentFields {
		if !knownAgentKeys()[field.key] {
			t.Errorf("rosterAgentFields names %q, which the formatter never writes", field.key)
		}
	}
}
