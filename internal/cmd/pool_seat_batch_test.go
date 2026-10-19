package cmd

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
)

// seatReadsOver is the per-seat read over store: what the roster spent on every
// polecat before the batch.
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

// TestPoolSeatBatchReadsIdlePolecatsInBatches is the roster over 25 polecat
// directories: the seats counted are the per-seat read's, and the reads are one
// batched agent-bead call and one batched assigned-work call for the rig, where
// the per-seat read spent two on every polecat. The idle polecats with no
// session and no hooked work cost nothing of their own (gt-u8rbh).
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

	if len(store.multis) != 1 || len(store.multis[0]) != 25 {
		t.Errorf("agent bead reads = %v, want one batch of the 25 polecats", store.multis)
	}
	if store.lists != 1 || store.assigned != 0 {
		t.Errorf("assigned-work reads = %d lists and %d per-polecat, want one list", store.lists, store.assigned)
	}
	for id, n := range store.shows {
		if strings.Contains(id, "polecat") {
			t.Errorf("agent bead %s read singly %d times, want only the batch", id, n)
		}
		if n > 1 {
			t.Errorf("bead %s read %d times", id, n)
		}
	}
	if perSeat.assigned < 25 {
		t.Errorf("the per-seat read made %d assigned-work reads, want one per polecat", perSeat.assigned)
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
