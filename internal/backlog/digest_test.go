package backlog

import (
	"os"
	"slices"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/testutil"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.HermeticMain(m))
}

var clock = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

// issue is an open bead last updated ageDays before the clock; a negative age
// leaves the update time empty.
func issue(id, typ string, prio, ageDays int) *beads.Issue {
	b := &beads.Issue{ID: id, Title: "title " + id, Type: typ, Priority: prio, Assignee: "a-" + id}
	if ageDays >= 0 {
		b.UpdatedAt = clock.Add(-time.Duration(ageDays) * 24 * time.Hour).Format(time.RFC3339)
	}
	return b
}

func ids(es []Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.ID)
	}
	return out
}

func TestBuildListsStaleOldestFirstAndCountsByPriority(t *testing.T) {
	t.Parallel()
	got := Build([]RigBeads{{Rig: "gastown", Beads: []*beads.Issue{
		issue("gt-new", "task", 1, 3),
		issue("gt-mid", "bug", 2, 15),
		issue("gt-edge", "task", 2, 14),
		issue("gt-old", "feature", 1, 40),
	}}, {Rig: "om"}}, clock, 14)

	if len(got.Rigs) != 2 || got.Rigs[1].Rig != "om" || got.Rigs[1].Count != 0 {
		t.Fatalf("rigs = %+v, want gastown and an empty om", got.Rigs)
	}
	r := got.Rigs[0]
	if want := []string{"gt-old", "gt-mid"}; !slices.Equal(ids(r.Beads), want) {
		t.Errorf("listed %q, want %q (the 14-day bead is not older than 14 days)", ids(r.Beads), want)
	}
	if r.Count != 2 || r.ByPriority[1] != 1 || r.ByPriority[2] != 1 {
		t.Errorf("count %d by priority %v, want 2 with one each at P1 and P2", r.Count, r.ByPriority)
	}
	if e := r.Beads[0]; e.AgeDays != 40 || e.Age() != "40d" || e.Priority != 1 || e.Assignee != "a-gt-old" || e.Title != "title gt-old" {
		t.Errorf("first entry = %+v, want age 40, P1, its assignee and title", e)
	}
}

func TestBuildSkipsNonWorkAndCrewSeatBeads(t *testing.T) {
	t.Parallel()
	got := Build([]RigBeads{{Rig: "gastown", Beads: []*beads.Issue{
		issue("gt-mol", "molecule", 2, 40),
		issue("gt-rig", "rig", 2, 40),
		issue("gt-crew-dunlin", "task", 2, 40),
		issue("gt-fix-crew-1", "task", 2, 20),
		issue("gt-none", "task", 3, -1),
	}}}, clock, 14)

	r := got.Rigs[0]
	if want := []string{"gt-none", "gt-fix-crew-1"}; !slices.Equal(ids(r.Beads), want) {
		t.Fatalf("listed %q, want %q: no update time is the oldest, a crew seat, a molecule and a rig are skipped", ids(r.Beads), want)
	}
	if e := r.Beads[0]; !e.Unknown || e.Age() != "unknown" {
		t.Errorf("entry with no update time = %+v, want Unknown and age \"unknown\"", e)
	}
}
