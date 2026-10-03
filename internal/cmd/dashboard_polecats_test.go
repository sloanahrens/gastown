package cmd

import (
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/landings"
	"github.com/steveyegge/gastown/internal/polecat"
)

func dashItem(state polecat.State, d polecat.WorkstateDisposition) polecatInventoryItem {
	return polecatInventoryItem{State: state, Disposition: d}
}

func TestDashPolecatStateFollowsTheInventory(t *testing.T) {
	t.Parallel()
	parked := polecat.WorkstateDisposition{ReuseStatus: polecat.WorkstateReuseStatusParked}
	recovery := polecat.WorkstateDisposition{NeedsRecovery: true, Reason: "has_unpushed"}
	for name, c := range map[string]struct {
		item   polecatInventoryItem
		work   bool
		ready  bool
		labels []string
		want   string
	}{
		"working":     {dashItem(polecat.StateWorking, polecat.WorkstateDisposition{}), true, false, nil, dashboard.StateWorking},
		"submitted":   {dashItem(polecat.StateSubmitted, polecat.WorkstateDisposition{}), true, false, nil, dashboard.StateQueued},
		"queue says":  {dashItem(polecat.StateWorking, polecat.WorkstateDisposition{}), true, true, nil, dashboard.StateQueued},
		"spawning":    {dashItem(polecat.StateSpawning, polecat.WorkstateDisposition{}), true, false, nil, dashboard.StateSpawning},
		"stalled":     {dashItem(polecat.StateStalled, polecat.WorkstateDisposition{}), true, false, nil, dashboard.StateStalled},
		"review":      {dashItem(polecat.StateReviewNeeded, polecat.WorkstateDisposition{}), false, false, nil, dashboard.StateReviewNeeded},
		"human":       {dashItem(polecat.StateWorking, polecat.WorkstateDisposition{}), true, true, []string{"gt:needs-human"}, dashboard.StateNeedsHuman},
		"parked":      {dashItem(polecat.StateIdle, parked), false, false, nil, dashboard.StateParked},
		"recovery":    {dashItem(polecat.StateIdle, recovery), false, false, nil, dashboard.StateRecovery},
		"idle":        {dashItem(polecat.StateIdle, polecat.WorkstateDisposition{Reusable: true}), false, false, nil, dashboard.StateIdle},
		"done":        {dashItem(polecat.StateDone, polecat.WorkstateDisposition{Reusable: true}), false, false, nil, dashboard.StateIdle},
		"label no wk": {dashItem(polecat.StateIdle, polecat.WorkstateDisposition{}), false, false, []string{"needs-human"}, dashboard.StateIdle},
	} {
		got, _ := dashPolecatState(c.item, c.work, c.ready, c.labels)
		if got != c.want {
			t.Errorf("%s: state = %q, want %q", name, got, c.want)
		}
	}
}

func TestBuildDashPolecats(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 3, 18, 0, 0, 0, time.UTC)
	cap1 := polecat.WorkstateDisposition{CountsTowardCapacity: true}
	seats := []dashSeat{
		{Rig: "gastown", Name: "free", Session: "gt-free", Item: dashItem(polecat.StateIdle, polecat.WorkstateDisposition{Reusable: true})},
		{Rig: "gastown", Name: "stalled", Session: "gt-stalled", Item: dashItem(polecat.StateStalled, cap1), Issue: &beads.Issue{ID: "gt-2", Title: "two", Priority: 1}},
		{Rig: "gastown", Name: "working", Session: "gt-working", Item: dashItem(polecat.StateWorking, cap1), Issue: &beads.Issue{ID: "gt-1", Title: "one", Priority: 2, Labels: []string{"gt:deferred", "noise"}}},
	}
	rec := func(name, verdict string, score float64, ago time.Duration) omRecord {
		return omRecord{Record: landings.Record{Rig: "gastown", Branch: "polecat/" + name + "/gt-x+abc", OMVerdict: verdict, OMScore: score, LandedAt: now.Add(-ago)}}
	}
	pcs := buildDashPolecats(dashPolecatInputs{
		Now: now, Seats: seats, SessionsKnown: true,
		Sessions: map[string]time.Time{"gt-working": now.Add(-time.Minute)},
		Records: []omRecord{
			rec("working", "approve", 0.8, time.Hour), rec("working", "approve", 0.6, 2*time.Hour),
			rec("working", "error:om did not run", 0, 3*time.Hour), rec("working", "approve", 0.9, 48*time.Hour),
		},
	})
	if len(pcs) != 3 || pcs[0].Name != "stalled" || pcs[1].Name != "working" || pcs[2].Name != "free" {
		t.Fatalf("order = %+v (what needs a look first, idle last)", pcs)
	}
	w := pcs[1]
	if w.Bead != "gt-1" || w.Title != "one" || w.Priority == nil || *w.Priority != 2 || !w.HasSession || !w.CountsTowardCapacity {
		t.Errorf("working = %+v", w)
	}
	if len(w.Labels) != 1 || w.Labels[0] != "deferred" {
		t.Errorf("labels = %v (only badge-worthy labels, without the gt: prefix)", w.Labels)
	}
	if w.Landed24h != 3 || w.Approved24h != 2 || w.AvgScore24h == nil || *w.AvgScore24h < 0.699 || *w.AvgScore24h > 0.701 {
		t.Errorf("record = landed %d approved %d score %v (a 48h-old landing must not count)", w.Landed24h, w.Approved24h, w.AvgScore24h)
	}
	if pcs[0].HasSession || pcs[2].Bead != "" || pcs[2].CountsTowardCapacity {
		t.Errorf("stalled/free = %+v / %+v", pcs[0], pcs[2])
	}
}

func TestDashBadgeLabels(t *testing.T) {
	t.Parallel()
	got := dashBadgeLabels([]string{"gt:ready-to-land", "needs-human", "gt:deferred", "bug"})
	if len(got) != 2 || got[0] != "needs-human" || got[1] != "deferred" {
		t.Errorf("badges = %v", got)
	}
}

func TestDashSeatCacheReusesUntilKeyOrTTLChanges(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 3, 18, 0, 0, 0, time.UTC)
	c := newDashSeatCache()
	c.now = func() time.Time { return now }
	item := polecatInventoryItem{Name: "agate"}
	if _, ok := c.get("gastown/agate", "k1"); ok {
		t.Fatal("an empty cache hit")
	}
	c.put("gastown/agate", "k1", item)
	if got, ok := c.get("gastown/agate", "k1"); !ok || got.Name != "agate" {
		t.Fatalf("same key missed: %+v %v", got, ok)
	}
	if _, ok := c.get("gastown/agate", "k2"); ok {
		t.Error("a changed agent bead, work or session must re-classify")
	}
	now = now.Add(5 * time.Minute)
	if _, ok := c.get("gastown/agate", "k1"); ok {
		t.Error("an entry past its ttl must re-classify")
	}
}

// A failed tmux read leaves the session list empty by failure, not by fact. A
// polecat with work and no session must then read unknown, never stalled: a
// stalled polecat is a red alert, and a monitor must not raise one on a read
// that did not work.
func TestFailedTmuxReadIsUnknownNotStalled(t *testing.T) {
	t.Parallel()
	seat := dashSeat{Rig: "gastown", Name: "busy", Session: "gt-busy",
		Item:  dashItem(polecat.StateStalled, polecat.WorkstateDisposition{CountsTowardCapacity: true}),
		Issue: &beads.Issue{ID: "gt-1", Title: "one"}}
	got := buildDashPolecats(dashPolecatInputs{Now: time.Now(), Seats: []dashSeat{seat}, SessionsKnown: false})
	if got[0].State != dashboard.StateUnknown {
		t.Errorf("unreadable tmux: state = %q, want unknown", got[0].State)
	}
	// With the read working, the same polecat and an empty session list is a
	// real stall: no tmux server means no session.
	got = buildDashPolecats(dashPolecatInputs{Now: time.Now(), Seats: []dashSeat{seat}, SessionsKnown: true})
	if got[0].State != dashboard.StateStalled {
		t.Errorf("readable tmux with no session: state = %q, want stalled", got[0].State)
	}
}

func TestDashSeatKeyChangesWhenTheSpawnGraceEnds(t *testing.T) {
	t.Parallel()
	updated := time.Date(2026, 10, 3, 18, 0, 0, 0, time.UTC)
	grace := 2 * time.Minute
	inside := dashSeatKey(updated, updated.Add(time.Minute), grace, "gt-1", false)
	outside := dashSeatKey(updated, updated.Add(3*time.Minute), grace, "gt-1", false)
	if inside == outside {
		t.Error("the key must change when the polecat leaves its spawn grace, or 'spawning' outlives the grace until the ttl")
	}
	if dashSeatKey(updated, updated.Add(time.Minute), grace, "gt-1", false) != inside {
		t.Error("the key must be stable while nothing changed")
	}
	if dashSeatKey(updated, updated.Add(time.Minute), grace, "gt-1", true) == inside {
		t.Error("a session coming up must change the key")
	}
}
