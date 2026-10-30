package cmd

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/config"
)

// seatReadsOver is the per-seat read over store: each polecat's agent bead, its
// assignment and the references it names, read one at a time. It is the answer
// the batch is held to.
func seatReadsOver(store beads.Client) poolSeatReads {
	return poolSeatReads{
		agentBead: func(_ beads.Client, id string) (*beads.Issue, *beads.AgentFields, error) {
			return beads.GetAgentBead(store, id)
		},
		assigned: func(_ assignedIssueLookup, rig, name string) polecatActiveWorkEvidence {
			issue, err := store.GetAssignedIssue(rig + "/polecats/" + name)
			if err != nil {
				return polecatActiveWorkLookupError(err)
			}
			return assessPolecatAssignedIssueWork(issue)
		},
		show: func(_ beads.Client, id string) (*beads.Issue, error) { return store.Show(id) },
	}
}

// seedPolecatAgentBead gives name an agent bead naming hook as its hook.
func seedPolecatAgentBead(db *beadsfake.Fake, town, rig, name, hook string) {
	id := beads.PolecatBeadIDWithPrefix(beads.GetPrefixForRig(town, rig), rig, name)
	desc := beads.FormatAgentDescription(id, &beads.AgentFields{RoleType: "polecat", Rig: rig, AgentState: "idle", HookBead: hook})
	db.Seed(beads.Issue{ID: id, Title: id, Type: "agent", Labels: []string{"gt:agent"}, Description: desc})
}

// seatTown is a rig of polecats: three with a live session (the first naming a
// hooked bead), one dead with its bead still in progress, and the rest idle
// with no session and no work.
func seatTown(t *testing.T, polecats int) (town string, db *beadsfake.Fake, lister *fakeLister) {
	t.Helper()
	town = t.TempDir()
	writeTestRigsConfig(t, town, "gastown")
	db = beadsfake.New()
	lister = &fakeLister{sessions: map[string]map[string]string{}}
	for i := 1; i <= polecats; i++ {
		name := fmt.Sprintf("p%02d", i)
		writeTownPolecatDir(t, town, "gastown", name)
		hook := ""
		if i == 1 {
			hook = "gt-hooked"
		}
		seedPolecatAgentBead(db, town, "gastown", name, hook)
		if i <= 3 {
			lister.sessions["gt-"+name] = map[string]string{"GT_ROLE": "gastown/polecats/" + name, "GT_AGENT": "deepseek-flash"}
		}
	}
	db.Seed(
		beads.Issue{ID: "gt-hooked", Status: beads.StatusHooked, Assignee: "gastown/polecats/p01"},
		beads.Issue{ID: "gt-dead", Status: string(beads.StatusInProgress), Assignee: "gastown/polecats/p04"},
		beads.Issue{ID: "gt-done", Status: "closed", Assignee: "gastown/polecats/p05"},
	)
	return town, db, lister
}

// batchOver is the batch over store.
func batchOver(town string, store *countingStore) *poolSeatBatch {
	return newPoolSeatBatch(town,
		func(string) beads.Client { return store },
		func(_ beads.Client, id string) (*beads.Issue, error) { return store.Show(id) })
}

// batchOverStore is the batch over any rig store, so a test can wrap one.
func batchOverStore(town string, store beads.Client) *poolSeatBatch {
	return newPoolSeatBatch(town,
		func(string) beads.Client { return store },
		func(_ beads.Client, id string) (*beads.Issue, error) { return store.Show(id) })
}

// TestPoolSeatBatchReadsIdlePolecatsInBatches is the roster over 25 polecat
// directories: the seats counted are the per-seat read's, and the reads are one
// batched assigned-work list for the rig, which also answers the agent beads it
// carries. The idle polecats with no session and no hooked work cost nothing of
// their own (gt-u8rbh, gt-4efmk).
func TestPoolSeatBatchReadsIdlePolecatsInBatches(t *testing.T) {
	t.Parallel()
	town, db, lister := seatTown(t, 25)
	pool := seatPool("deepseek-flash", 30)

	perSeat := newCountingStore(db)
	want, err := poolSeatSessionsWith(lister, town, poolDispositionAnswering(town, seatReadsOver(perSeat)), nil, pool)
	if err != nil {
		t.Fatalf("per-seat roster: %v", err)
	}

	store := newCountingStore(db)
	got, err := poolSeatSessionsWith(lister, town, batchOver(town, store).disposition, nil, pool)
	if err != nil {
		t.Fatalf("batched roster: %v", err)
	}

	if n := poolSeatCount("deepseek-flash", got); n != 4 {
		t.Fatalf("seat count = %d, want 3 live and 1 dead-hooked (%+v)", n, got)
	}
	sort.Slice(got, func(i, j int) bool { return got[i].name < got[j].name })
	sort.Slice(want, func(i, j int) bool { return want[i].name < want[j].name })
	if len(got) != len(want) {
		t.Fatalf("batched roster = %+v, want the per-seat read's %+v", got, want)
	}
	for i := range got {
		if got[i].name != want[i].name || got[i].agent != want[i].agent || got[i].deadHooked != want[i].deadHooked {
			t.Errorf("seat %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	if len(store.multis) != 0 {
		t.Errorf("batched reads = %v, want none: the work list answers every agent bead", store.multis)
	}
	if store.lists != 1 || store.assigned != 0 {
		t.Errorf("assigned-work reads = %d lists and %d per-polecat, want one list", store.lists, store.assigned)
	}
	for id, n := range store.shows {
		if strings.Contains(id, "polecat") {
			t.Errorf("agent bead %s read singly %d times, want only the work list", id, n)
		}
		if n > 1 {
			t.Errorf("bead %s read %d times", id, n)
		}
	}
	if perSeat.assigned < 25 {
		t.Errorf("the per-seat read made %d assigned-work reads, want one per polecat", perSeat.assigned)
	}
}

// TestPoolShowLiveReadsTheRigInBatches is gt-lj299: the occupancy gt pool show
// prints over 20 polecat directories is the per-seat read's count, and the
// rig's agent beads and assigned work are read in one batch each, not two bd
// calls per directory. The store builder records the directories it is asked
// for, so a batch that read the town's store instead of the rig's fails here
// rather than counted a store the caller's builder never named.
func TestPoolShowLiveReadsTheRigInBatches(t *testing.T) {
	t.Parallel()
	town, db, lister := seatTown(t, 20)
	ts := config.NewTownSettings()
	ts.PolecatPool = seatPool("deepseek-flash", 30)
	if err := config.SaveTownSettings(config.TownSettingsPath(town), ts); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}

	store := newCountingStore(db)
	var dirs []string
	got := poolLiveForBatch(town, lister, func(dir string) beads.Client {
		dirs = append(dirs, dir)
		return store
	})

	// seatTown's seats: p01 to p03 live, p04 dead with its bead still in
	// progress — and the dead-hooked seat counted inside the live count, the
	// way the page prints it (gt-bc9b6).
	if !got.ok {
		t.Fatalf("occupancy read failed: ok=false, want the seats counted")
	}
	if got.total != 4 || got.live["deepseek-flash"] != 4 || got.dead["deepseek-flash"] != 1 {
		t.Fatalf("count live=%v dead=%v total=%d, want 4 seats on deepseek-flash with 1 of them dead-hooked",
			got.live, got.dead, got.total)
	}

	if want := []string{filepath.Join(town, "gastown")}; !reflect.DeepEqual(dirs, want) {
		t.Errorf("stores opened at %v, want the rig's own directory %v", dirs, want)
	}
	if len(store.multis) != 0 {
		t.Errorf("batched reads = %v, want none: the work list answers the agent beads", store.multis)
	}
	if store.lists != 1 || store.assigned != 0 {
		t.Errorf("assigned-work reads = %d lists and %d per-polecat, want one list", store.lists, store.assigned)
	}
	for id, n := range store.shows {
		if strings.Contains(id, "polecat") {
			t.Errorf("agent bead %s read singly %d times, want only the work list", id, n)
		}
	}
}

// TestPoolShowLiveCountsWhenTheBatchFails is the batch-failure path at the
// entry point gt pool show uses: a rig whose batched reads fail is read
// polecat by polecat, and the page still prints the count. A failed batch
// costs calls, never the occupancy (gt-lj299).
func TestPoolShowLiveCountsWhenTheBatchFails(t *testing.T) {
	t.Parallel()
	town, db, lister := seatTown(t, 20)
	ts := config.NewTownSettings()
	ts.PolecatPool = seatPool("deepseek-flash", 30)
	if err := config.SaveTownSettings(config.TownSettingsPath(town), ts); err != nil {
		t.Fatalf("SaveTownSettings: %v", err)
	}

	store := newCountingStore(db)
	store.failBatch = true
	got := poolLiveForBatch(town, lister, func(string) beads.Client { return store })

	if !got.ok {
		t.Fatalf("occupancy read failed with the batch down: ok=false, want the seats counted one by one")
	}
	if got.total != 4 || got.live["deepseek-flash"] != 4 || got.dead["deepseek-flash"] != 1 {
		t.Fatalf("count with the batch down live=%v dead=%v total=%d, want 4 seats on deepseek-flash with 1 of them dead-hooked",
			got.live, got.dead, got.total)
	}
	if len(store.multis) != 1 || len(store.multis[0]) != 20 {
		t.Errorf("batched reads = %v, want the 20 agent beads asked for once before the fallback", store.multis)
	}
	if store.assigned != 20 {
		t.Errorf("per-polecat assigned-work reads = %d, want one for each of the 20", store.assigned)
	}
}

// A rig whose batched reads fail is read polecat by polecat, and counts the
// seats it always did.
func TestPoolSeatBatchFallsBackWhenTheBatchFails(t *testing.T) {
	t.Parallel()
	town, db, lister := seatTown(t, 6)
	store := newCountingStore(db)
	store.failBatch = true
	got, err := poolSeatSessionsWith(lister, town, batchOver(town, store).disposition, nil, seatPool("deepseek-flash", 30))
	if err != nil {
		t.Fatalf("poolSeatSessionsWith: %v", err)
	}
	if n := poolSeatCount("deepseek-flash", got); n != 4 {
		t.Fatalf("seat count = %d, want 3 live and 1 dead-hooked (%+v)", n, got)
	}
	if store.assigned != 6 {
		t.Errorf("per-polecat assigned-work reads = %d, want one for each of the 6", store.assigned)
	}
}

// The batch picks each assignee's issue from one list of every assignee's
// work. The pick must be the one GetAssignedIssue makes for that assignee alone:
// the most-preferred status first, closed work never picked, and an assignee
// with nothing live gets no pick.
func TestAssignedByAssigneeMatchesGetAssignedIssue(t *testing.T) {
	t.Parallel()
	db := beadsfake.New()
	db.Seed(
		beads.Issue{ID: "gt-closed", Status: "closed", Assignee: "gastown/polecats/a"},
		beads.Issue{ID: "gt-hooked", Status: beads.StatusHooked, Assignee: "gastown/polecats/a"},
		beads.Issue{ID: "gt-prog", Status: string(beads.StatusInProgress), Assignee: "gastown/polecats/a"},
		beads.Issue{ID: "gt-open", Status: string(beads.StatusOpen), Assignee: "gastown/polecats/a"},
		beads.Issue{ID: "gt-prog-b", Status: string(beads.StatusInProgress), Assignee: "gastown/polecats/b"},
		beads.Issue{ID: "gt-hooked-b", Status: beads.StatusHooked, Assignee: "gastown/polecats/b"},
		beads.Issue{ID: "gt-free", Status: string(beads.StatusOpen)},
		beads.Issue{ID: "gt-closed-c", Status: "closed", Assignee: "gastown/polecats/c"},
	)
	work, err := db.List(beads.ListOptions{Status: strings.Join(assignedStatuses, ","), Priority: -1})
	if err != nil {
		t.Fatalf("batched list: %v", err)
	}
	picks := assignedByAssignee(work)
	for _, who := range []string{"gastown/polecats/a", "gastown/polecats/b", "gastown/polecats/c", "gastown/polecats/none"} {
		want, err := db.GetAssignedIssue(who)
		if err != nil {
			t.Fatalf("GetAssignedIssue(%s): %v", who, err)
		}
		got := picks[who]
		switch {
		case want == nil && got == nil:
		case want == nil || got == nil || want.ID != got.ID:
			t.Errorf("%s: batch picked %v, GetAssignedIssue picks %v", who, got, want)
		}
	}
}

// closedSourceTown is a rig of polecats with no hook, each last working a
// source issue lastSource names. The source issues are closed beads, so the
// seat read needs their status and nothing else (gt-kvp82). The first two
// polecats have a live session.
func closedSourceTown(t *testing.T, polecats int, lastSource func(name string) string) (town string, db *beadsfake.Fake, lister *fakeLister) {
	t.Helper()
	town = t.TempDir()
	writeTestRigsConfig(t, town, "gastown")
	db = beadsfake.New()
	lister = &fakeLister{sessions: map[string]map[string]string{}}
	prefix := beads.GetPrefixForRig(town, "gastown")
	for i := 1; i <= polecats; i++ {
		name := fmt.Sprintf("p%02d", i)
		writeTownPolecatDir(t, town, "gastown", name)
		id := beads.PolecatBeadIDWithPrefix(prefix, "gastown", name)
		desc := beads.FormatAgentDescription(id, &beads.AgentFields{RoleType: "polecat", Rig: "gastown", AgentState: "idle", LastSourceIssue: lastSource(name)})
		db.Seed(beads.Issue{ID: id, Title: id, Type: "agent", Labels: []string{"gt:agent"}, Description: desc},
			beads.Issue{ID: "gt-old-" + name, Status: "closed"})
		if i <= 2 {
			lister.sessions["gt-"+name] = map[string]string{"GT_ROLE": "gastown/polecats/" + name, "GT_AGENT": "deepseek-flash"}
		}
	}
	return town, db, lister
}

// closedSourceOf names each polecat's own closed source issue.
func closedSourceOf(name string) string { return "gt-old-" + name }

// polecatNames names the first n polecats a fixture town seeds.
func polecatNames(n int) []string {
	names := make([]string, 0, n)
	for i := 1; i <= n; i++ {
		names = append(names, fmt.Sprintf("p%02d", i))
	}
	return names
}

// assertSameSeats fails unless got holds the seats want does, in the same
// states, whatever order either roster lists them in.
func assertSameSeats(t *testing.T, got, want []poolSession) {
	t.Helper()
	sort.Slice(got, func(i, j int) bool { return got[i].name < got[j].name })
	sort.Slice(want, func(i, j int) bool { return want[i].name < want[j].name })
	if len(got) != len(want) {
		t.Fatalf("batched roster = %+v, want the per-seat read's %+v", got, want)
	}
	for i := range got {
		if got[i].name != want[i].name || got[i].agent != want[i].agent || got[i].deadHooked != want[i].deadHooked {
			t.Errorf("seat %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// assertSameSeatVerdicts fails unless the batched seat read decides each named
// polecat's seat the way the per-seat read does. assertSameSeats compares the
// seats a roster holds, and a wrong closed/absent verdict can hold the same
// seats: a closed hook bead read as absent, or an absent one read as closed, is
// one seat either way. The terminality such a verdict changes shows in the
// disposition behind the seat, which this compares. Both reads are made here
// over their own counting stores, so a caller's read counts stay its own.
func assertSameSeatVerdicts(t *testing.T, town string, db *beadsfake.Fake, names []string) {
	t.Helper()
	perSeat := poolDispositionAnswering(town, seatReadsOver(newCountingStore(db)))
	batched := batchOver(town, newCountingStore(db)).disposition
	for _, name := range names {
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
}

// TestPoolSeatBatchReadsClosedSourceIssuesInOneBatch is the roster over 20
// polecat directories whose last source issue is closed: the batch reads the
// agent beads and then, in one more batch, the source issues. No polecat costs
// a live read of its own, and the seats count as the per-seat read counts them.
func TestPoolSeatBatchReadsClosedSourceIssuesInOneBatch(t *testing.T) {
	t.Parallel()
	town, db, lister := closedSourceTown(t, 20, closedSourceOf)
	pool := seatPool("deepseek-flash", 30)

	perSeat := newCountingStore(db)
	want, err := poolSeatSessionsWith(lister, town, poolDispositionAnswering(town, seatReadsOver(perSeat)), nil, pool)
	if err != nil {
		t.Fatalf("per-seat roster: %v", err)
	}

	store := newCountingStore(db)
	batch := batchOver(town, store)
	got, err := poolSeatSessionsWith(lister, town, batch.disposition, nil, pool)
	if err != nil {
		t.Fatalf("batched roster: %v", err)
	}
	assertSameSeats(t, got, want)
	names := polecatNames(20)
	assertSameSeatVerdicts(t, town, db, names)

	if len(store.multis) != 1 || len(store.multis[0]) != 20 {
		t.Errorf("batched reads = %v, want the 20 source issues: the work list answers the agent beads", store.multis)
	}
	if len(store.shows) != 0 {
		t.Errorf("live reads = %v, want none: every source issue came from the batch", store.shows)
	}
	// The verdict each seat is decided from is the closed bead the batch read,
	// not an absent one: the terminality is what makes these seats free.
	for _, name := range names {
		id := closedSourceOf(name)
		issue, ok := batch.rig("gastown").held[id]
		if !ok || issue.Status != "closed" {
			t.Errorf("batch holds %v for %s, want the closed source issue", issue, id)
		}
	}
}

// A rig whose source-issue batch fails reads each closed source issue live, once,
// and counts the seats the per-seat read counts.
func TestPoolSeatBatchReadsSourceIssuesLiveWhenTheBatchFails(t *testing.T) {
	t.Parallel()
	town, db, lister := closedSourceTown(t, 6, closedSourceOf)
	pool := seatPool("deepseek-flash", 30)

	perSeat := newCountingStore(db)
	want, err := poolSeatSessionsWith(lister, town, poolDispositionAnswering(town, seatReadsOver(perSeat)), nil, pool)
	if err != nil {
		t.Fatalf("per-seat roster: %v", err)
	}

	store := &showMultipleFails{countingStore: newCountingStore(db), fail: sourcesUnder}
	got, err := poolSeatSessionsWith(lister, town, batchOverStore(town, store).disposition, nil, pool)
	if err != nil {
		t.Fatalf("batched roster: %v", err)
	}
	assertSameSeats(t, got, want)

	// The agent beads came from the work list, which answered; only the source
	// batch failed, so every source issue is read live and no agent bead is.
	if len(store.multis) != 1 || !sourcesUnder(store.multis[0]) {
		t.Errorf("batched reads = %v, want the one source batch that failed", store.multis)
	}
	for id := range store.shows {
		if !strings.HasPrefix(id, "gt-old-") {
			t.Errorf("source batch's failure also cost a live read of %s, want the agent beads from the work list", id)
		}
	}
	if len(store.shows) != 6 {
		t.Errorf("live reads = %v, want each of the 6 source issues read once", store.shows)
	}
	for id, n := range store.shows {
		if n != 1 {
			t.Errorf("source issue %s read live %d times, want once", id, n)
		}
	}
}

// A source issue the rig's batch does not hold is read live, and the seat counts
// as the per-seat read counts it: the batch never reads an absent bead as closed.
func TestPoolSeatBatchReadsAbsentSourceIssueLive(t *testing.T) {
	t.Parallel()
	town, db, lister := closedSourceTown(t, 4, func(name string) string {
		if name == "p04" {
			return "gt-elsewhere"
		}
		return closedSourceOf(name)
	})
	pool := seatPool("deepseek-flash", 30)

	perSeat := newCountingStore(db)
	want, err := poolSeatSessionsWith(lister, town, poolDispositionAnswering(town, seatReadsOver(perSeat)), nil, pool)
	if err != nil {
		t.Fatalf("per-seat roster: %v", err)
	}

	store := newCountingStore(db)
	batch := batchOver(town, store)
	got, err := poolSeatSessionsWith(lister, town, batch.disposition, nil, pool)
	if err != nil {
		t.Fatalf("batched roster: %v", err)
	}
	assertSameSeats(t, got, want)
	assertSameSeatVerdicts(t, town, db, polecatNames(4))

	// The bead p04's hint names does not exist, and the batch says so by
	// holding nothing for it: an entry here is what would let the seat read
	// call it closed.
	if issue, ok := batch.rig("gastown").held["gt-elsewhere"]; ok {
		t.Errorf("batch holds %v for the absent source issue, want nothing", issue)
	}
	if store.shows["gt-elsewhere"] != 1 {
		t.Errorf("absent source issue read live %d times, want once", store.shows["gt-elsewhere"])
	}
}

// hookCase is one polecat of hookTown: the last source issue its agent bead
// names, its hook, and the status that hook bead is seeded with ("" leaves the
// bead absent).
type hookCase struct {
	name, hint, hook, status string
}

// hookTown is a rig of sessionless polecats, each agent bead naming a hook and,
// when hint names one, a last source issue. No polecat has a session or an
// assignment, so the hook reference decides whether its seat is held.
func hookTown(t *testing.T, hooks []hookCase) (town string, db *beadsfake.Fake, lister *fakeLister) {
	t.Helper()
	town = t.TempDir()
	writeTestRigsConfig(t, town, "gastown")
	db = beadsfake.New()
	lister = &fakeLister{sessions: map[string]map[string]string{}}
	prefix := beads.GetPrefixForRig(town, "gastown")
	for _, h := range hooks {
		writeTownPolecatDir(t, town, "gastown", h.name)
		id := beads.PolecatBeadIDWithPrefix(prefix, "gastown", h.name)
		desc := beads.FormatAgentDescription(id, &beads.AgentFields{RoleType: "polecat", Rig: "gastown", AgentState: "idle", HookBead: h.hook, LastSourceIssue: h.hint})
		db.Seed(beads.Issue{ID: id, Title: id, Type: "agent", Labels: []string{"gt:agent"}, Description: desc})
		if h.status != "" {
			db.Seed(beads.Issue{ID: h.hook, Status: h.status})
		}
		if h.hint != "" {
			db.Seed(beads.Issue{ID: h.hint, Status: "closed"})
		}
	}
	return town, db, lister
}

// TestPoolSeatBatchAnswersHookBeads drives polecats with a hook bead through
// the batch. p01's hook is a live hooked bead the work list already holds. p02
// last worked a source issue and its hook names a different, closed bead, so
// the two references differ and both cost the batch's second read. p03's hook
// names no bead at all: it is asked for, the batch holds nothing for it, and
// the seat read falls back to one live read. No hook costs its own live read,
// and each seat's verdict is the per-seat read's: the hooked one holds a seat,
// the closed one does not, and the unreadable one holds it as a failed lookup
// (gt-kvp82).
func TestPoolSeatBatchAnswersHookBeads(t *testing.T) {
	t.Parallel()
	town, db, lister := hookTown(t, []hookCase{
		{"p01", "", "gt-hooked", beads.StatusHooked},
		{"p02", "gt-old-p02", "gt-closed-hook", "closed"},
		{"p03", "", "gt-absent", ""},
	})
	pool := seatPool("deepseek-flash", 30)

	perSeat := newCountingStore(db)
	want, err := poolSeatSessionsWith(lister, town, poolDispositionAnswering(town, seatReadsOver(perSeat)), nil, pool)
	if err != nil {
		t.Fatalf("per-seat roster: %v", err)
	}

	store := newCountingStore(db)
	got, err := poolSeatSessionsWith(lister, town, batchOver(town, store).disposition, nil, pool)
	if err != nil {
		t.Fatalf("batched roster: %v", err)
	}
	assertSameSeats(t, got, want)
	assertSameSeatVerdicts(t, town, db, []string{"p01", "p02", "p03"})

	if n := poolSeatCount("deepseek-flash", got); n != 2 {
		t.Errorf("seat count = %d, want p01's hooked seat and p03's unreadable one (%+v)", n, got)
	}
	// The work list holds the agent beads and p01's hook; the batch asks for the
	// three references it does not hold, so p02's hook costs no more than its
	// hint does and neither pays a live read.
	if len(store.multis) != 1 {
		t.Fatalf("batched reads = %v, want only the references the work list does not hold", store.multis)
	}
	if got := sortedCopy(store.multis[0]); strings.Join(got, ",") != "gt-absent,gt-closed-hook,gt-old-p02" {
		t.Errorf("reference batch = %v, want p02's hint and hook and p03's hook", store.multis[0])
	}
	if store.shows["gt-hooked"] != 0 || store.shows["gt-closed-hook"] != 0 || store.shows["gt-old-p02"] != 0 {
		t.Errorf("live reads = %v, want none: the batch answers every reference it holds", store.shows)
	}
	if store.shows["gt-absent"] != 1 {
		t.Errorf("absent hook read live %d times, want once", store.shows["gt-absent"])
	}
}

// sortedCopy returns ids in sorted order, so a set the batch collects from a
// map compares the same on every run.
func sortedCopy(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

// The parse the batch caches is the live read's, error included: an id that
// names a bead which is not an agent bead is reported, not answered as a seat
// with no fields.
func TestPoolSeatBatchReportsABeadThatIsNotAnAgentBead(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTestRigsConfig(t, town, "gastown")
	writeTownPolecatDir(t, town, "gastown", "p01")
	db := beadsfake.New()
	id := beads.PolecatBeadIDWithPrefix(beads.GetPrefixForRig(town, "gastown"), "gastown", "p01")
	db.Seed(beads.Issue{ID: id, Title: id, Type: "task"})

	_, err := batchOver(town, newCountingStore(db)).disposition("gastown", "p01")
	if err == nil || !strings.Contains(err.Error(), "is not an agent bead") {
		t.Fatalf("batched seat read = %v, want the live read's not-an-agent-bead error", err)
	}
}

// showMultipleFails is a store whose ShowMultiple fails for the ids fail names,
// so a test can lose one batched read and leave the others answering.
type showMultipleFails struct {
	*countingStore
	fail func(ids []string) bool
}

func (s *showMultipleFails) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	if s.fail(ids) {
		// The call was made, so it is counted even though bd failed it.
		s.multis = append(s.multis, append([]string(nil), ids...))
		return nil, errors.New("bd show: boom")
	}
	return s.countingStore.ShowMultiple(ids)
}

// asksFor selects a ShowMultiple that asks the store for id.
func asksFor(id string) func([]string) bool {
	return func(ids []string) bool {
		for _, asked := range ids {
			if asked == id {
				return true
			}
		}
		return false
	}
}

// sourcesUnder is the selector for a batch of the closed source issues
// closedSourceTown seeds: the reads whose ids are all gt-old-*.
func sourcesUnder(ids []string) bool {
	for _, id := range ids {
		if !strings.HasPrefix(id, "gt-old-") {
			return false
		}
	}
	return len(ids) > 0
}

// workListFails is a rig store whose assigned-work List fails; its batched
// agent-bead reads still answer.
type workListFails struct {
	*countingStore
}

func (w *workListFails) List(beads.ListOptions) ([]*beads.Issue, error) {
	return nil, errors.New("bd list: boom")
}

// agentListOmits is a store whose work list drops every agent bead's
// description, the field a list form that did not carry it would leave out.
type agentListOmits struct {
	*countingStore
}

func (s *agentListOmits) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	issues, err := s.countingStore.List(opts)
	if err != nil {
		return nil, err
	}
	for _, issue := range issues {
		if beads.IsAgentBead(issue) {
			issue.Description = ""
		}
	}
	return issues, nil
}

// TestSeatBatchReadsAnAgentBeadTheListDidNotCarryWhole is the batch's guard on
// the work list as an agent-bead source: a list record that does not carry the
// fields the seat read parses is not answered as an agent bead holding nothing —
// which would read its seat free — but read by id, and the seat keeps the work
// its description names.
func TestSeatBatchReadsAnAgentBeadTheListDidNotCarryWhole(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTestRigsConfig(t, town, "gastown")
	writeTownPolecatDir(t, town, "gastown", "p01")
	db := beadsfake.New()
	seedPolecatAgentBead(db, town, "gastown", "p01", "gt-hooked")
	db.Seed(beads.Issue{ID: "gt-hooked", Status: beads.StatusHooked})
	store := &agentListOmits{countingStore: newCountingStore(db)}

	perSeat := poolDispositionAnswering(town, seatReadsOver(newCountingStore(db)))
	want, err := perSeat("gastown", "p01")
	if err != nil {
		t.Fatalf("per-seat read: %v", err)
	}
	got, err := batchOverStore(town, store).disposition("gastown", "p01")
	if err != nil {
		t.Fatalf("batched read: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("seat = %+v, want the per-seat read's %+v", got, want)
	}
	if !got.HookHeld || !got.Disposition.CountsTowardCapacity {
		t.Errorf("seat = %+v, want the hooked work its description names to hold it", got)
	}
	// The record the list could not carry whole is read by id instead.
	agentID := beads.PolecatBeadIDWithPrefix(beads.GetPrefixForRig(town, "gastown"), "gastown", "p01")
	if len(store.multis) != 1 || len(store.multis[0]) != 1 || store.multis[0][0] != agentID {
		t.Errorf("batched reads = %v, want the one agent bead the list left incomplete", store.multis)
	}
}

// agentListTruncates is a store whose work list keeps each agent bead but drops
// the description lines drop names, the shape of a list form that cut a
// description short.
type agentListTruncates struct {
	*countingStore
	drop []string
}

func (s *agentListTruncates) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	issues, err := s.countingStore.List(opts)
	if err != nil {
		return nil, err
	}
	for _, issue := range issues {
		if beads.IsAgentBead(issue) {
			issue.Description = withoutDescriptionKeys(issue.Description, s.drop)
		}
	}
	return issues, nil
}

// withoutDescriptionKeys returns description without the lines whose key is in
// drop.
func withoutDescriptionKeys(description string, drop []string) string {
	gone := make(map[string]bool, len(drop))
	for _, key := range drop {
		gone[strings.ToLower(key)] = true
	}
	var kept []string
	for _, line := range strings.Split(description, "\n") {
		key, _, ok := strings.Cut(line, ":")
		if ok && gone[strings.ToLower(strings.TrimSpace(key))] {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// TestSeatBatchReadsAnAgentBeadTheListTruncated is the completeness gate on a
// record that kept the role and state lines and lost the rest: a working seat's
// hook line is gone, so answering from the list record would read the seat idle
// with a free seat. The batch reads that bead by id instead, and the seat keeps
// the work its description names (gt-4efmk).
func TestSeatBatchReadsAnAgentBeadTheListTruncated(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTestRigsConfig(t, town, "gastown")
	writeTownPolecatDir(t, town, "gastown", "p01")
	db := beadsfake.New()
	seedPolecatAgentBead(db, town, "gastown", "p01", "gt-hooked")
	db.Seed(beads.Issue{ID: "gt-hooked", Status: beads.StatusHooked})
	store := &agentListTruncates{
		countingStore: newCountingStore(db),
		drop:          []string{"hook_bead"},
	}

	perSeat := poolDispositionAnswering(town, seatReadsOver(newCountingStore(db)))
	want, err := perSeat("gastown", "p01")
	if err != nil {
		t.Fatalf("per-seat read: %v", err)
	}
	got, err := batchOverStore(town, store).disposition("gastown", "p01")
	if err != nil {
		t.Fatalf("batched read: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("seat = %+v, want the per-seat read's %+v", got, want)
	}
	if !got.HookHeld || !got.Disposition.CountsTowardCapacity {
		t.Errorf("seat = %+v, want the hooked work its description names to hold it", got)
	}
	// The record the list carried short is read by id instead.
	agentID := beads.PolecatBeadIDWithPrefix(beads.GetPrefixForRig(town, "gastown"), "gastown", "p01")
	if len(store.multis) != 1 || len(store.multis[0]) != 1 || store.multis[0][0] != agentID {
		t.Errorf("batched reads = %v, want the one agent bead the list left incomplete", store.multis)
	}
}

// TestPoolSeatBatchCountsSeatsWhenTheWorkListFails is the work list's failure
// branch: the list is what answers both agent beads and assignments, and its
// failure costs a batch of agent-bead reads and one live read per polecat, not
// a picture of a pool with free seats.
func TestPoolSeatBatchCountsSeatsWhenTheWorkListFails(t *testing.T) {
	t.Parallel()
	town, db, lister := seatTown(t, 20)
	pool := seatPool("deepseek-flash", 30)

	perSeat := newCountingStore(db)
	want, err := poolSeatSessionsWith(lister, town, poolDispositionAnswering(town, seatReadsOver(perSeat)), nil, pool)
	if err != nil {
		t.Fatalf("per-seat roster: %v", err)
	}

	store := &workListFails{countingStore: newCountingStore(db)}
	got, err := poolSeatSessionsWith(lister, town, batchOverStore(town, store).disposition, nil, pool)
	if err != nil {
		t.Fatalf("batched roster: %v", err)
	}
	assertSameSeats(t, got, want)
	if n := poolSeatCount("deepseek-flash", got); n != 4 {
		t.Fatalf("seat count = %d, want 3 live and 1 dead-hooked with the work list down (%+v)", n, got)
	}
	// The agent beads the failed list could not answer cost one batch, and the
	// assignments the failed list could not answer cost one live read each.
	if len(store.multis) == 0 || len(store.multis[0]) != 20 {
		t.Errorf("batched reads = %v, want the 20 agent beads asked for in the first batch", store.multis)
	}
	if store.assigned != 20 {
		t.Errorf("per-polecat assigned-work reads = %d, want one for each of the 20", store.assigned)
	}
}

// TestSeatBatchHeldIssueIsOneBeadWhicheverRigListsIt: a bead id two rigs' work
// lists carry is answered by the same rig on every read, so a batch's answers do
// not change from one call to the next.
func TestSeatBatchHeldIssueIsOneBeadWhicheverRigListsIt(t *testing.T) {
	t.Parallel()
	batch := newPoolSeatBatch("", nil, nil)
	batch.rigs["arig"] = &rigSeatBatch{held: map[string]*beads.Issue{"gt-shared": {ID: "gt-shared", Title: "arig"}}}
	batch.rigs["brig"] = &rigSeatBatch{held: map[string]*beads.Issue{"gt-shared": {ID: "gt-shared", Title: "brig"}}}
	first, ok := batch.heldIssue("gt-shared")
	if !ok {
		t.Fatalf("the batch holds nothing for gt-shared, want a rig's record")
	}
	for i := 0; i < 64; i++ {
		issue, ok := batch.heldIssue("gt-shared")
		if !ok || issue.Title != first.Title {
			t.Fatalf("read %d answered gt-shared with %v (%v), want the first read's %q",
				i, issue, ok, first.Title)
		}
	}
}
