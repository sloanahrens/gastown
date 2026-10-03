package cmd

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/landings"
)

func TestBuildDashPolecats(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 3, 18, 0, 0, 0, time.UTC)
	seats := []dashSeat{
		{Rig: "gastown", Name: "working", Session: "gt-working", Bead: "gt-1"},
		{Rig: "gastown", Name: "nosession", Session: "gt-nosession", Bead: "gt-2"},
		{Rig: "gastown", Name: "queued", Session: "gt-queued", Bead: "gt-3"},
		{Rig: "gastown", Name: "human", Session: "gt-human", Bead: "gt-4"},
		{Rig: "gastown", Name: "dupa", Session: "gt-dupa", Bead: "gt-5"},
		{Rig: "gastown", Name: "dupb", Session: "gt-dupb", Bead: "gt-5"},
		{Rig: "gastown", Name: "free", Session: "gt-free"},
		{Rig: "gastown", Name: "closedhook", Session: "gt-closedhook", Bead: "gt-6"},
	}
	sessions := map[string]time.Time{}
	for _, n := range []string{"working", "queued", "human", "dupa", "dupb", "free", "closedhook"} {
		sessions["gt-"+n] = now.Add(-time.Minute)
	}
	issues := map[string]*beads.Issue{
		"gt-1": {Title: "one", Priority: 1},
		"gt-4": {Title: "four", Priority: 2, Labels: []string{"gt:needs-human", "noise"}},
		"gt-6": {Title: "done already", Status: "closed"},
	}
	rec := func(name, verdict string, score float64, ago time.Duration) omRecord {
		return omRecord{Record: landings.Record{Rig: "gastown", Branch: "polecat/" + name + "/gt-x+abc", OMVerdict: verdict, OMScore: score, LandedAt: now.Add(-ago)}}
	}
	records := []omRecord{
		rec("working", "approve", 0.8, time.Hour), rec("working", "approve", 0.6, 2*time.Hour),
		rec("working", "error:om did not run", 0, 3*time.Hour), rec("working", "approve", 0.9, 48*time.Hour),
	}
	pcs := buildDashPolecats(dashPolecatInputs{
		Now: now, Seats: seats, Ready: map[string]bool{"gt-3": true}, Sessions: sessions, Known: true,
		Bead: func(_, id string) *beads.Issue { return issues[id] }, Records: records,
	})
	got := map[string]dashboard.Polecat{}
	for _, p := range pcs {
		got[p.Name] = p
	}
	for name, want := range map[string]string{
		"working": dashboard.StateWorking, "nosession": dashboard.StateStale, "queued": dashboard.StateQueued,
		"human": dashboard.StateNeedsHuman, "closedhook": dashboard.StateStale, "dupa": dashboard.StateWorking, "free": dashboard.StateIdle,
	} {
		if got[name].State != want {
			t.Errorf("%s state = %q, want %q", name, got[name].State, want)
		}
	}
	w := got["working"]
	if w.Title != "one" || w.Priority == nil || *w.Priority != 1 {
		t.Errorf("working bead facts = %+v", w)
	}
	if w.Landed24h != 3 || w.Approved24h != 2 || w.AvgScore24h == nil || *w.AvgScore24h < 0.699 || *w.AvgScore24h > 0.701 {
		t.Errorf("working record = landed %d approved %d score %v (the 48h-old landing must not count)", w.Landed24h, w.Approved24h, w.AvgScore24h)
	}
	if l := got["human"].Labels; len(l) != 1 || l[0] != "needs-human" {
		t.Errorf("human labels = %v (only badge-worthy labels, without the gt: prefix)", l)
	}
	if len(got["dupa"].AlsoHeldBy) != 1 || got["dupa"].AlsoHeldBy[0] != "gastown/dupb" {
		t.Errorf("dupa also held by = %v", got["dupa"].AlsoHeldBy)
	}
	if got["free"].Bead != "" || got["free"].Title != "" {
		t.Errorf("an idle polecat carries no bead: %+v", got["free"])
	}
	// the order puts what needs attention first and idle last
	if pcs[0].State != dashboard.StateNeedsHuman || pcs[len(pcs)-1].State != dashboard.StateIdle {
		t.Errorf("order: first %q last %q", pcs[0].State, pcs[len(pcs)-1].State)
	}
}

// When tmux cannot be read, a missing session means nothing: no polecat may
// be called stale for want of a reading.
func TestBuildDashPolecatsUnknownSessionsAreNotStale(t *testing.T) {
	t.Parallel()
	pcs := buildDashPolecats(dashPolecatInputs{
		Now: time.Now(), Known: false,
		Seats: []dashSeat{{Rig: "gastown", Name: "a", Session: "gt-a", Bead: "gt-1"}},
	})
	if pcs[0].State != dashboard.StateWorking {
		t.Errorf("state = %q, want working", pcs[0].State)
	}
}

func TestDashBeadInfoCachesForTTLAndRemembersFailure(t *testing.T) {
	t.Parallel()
	var reads atomic.Int32
	now := time.Date(2026, 10, 3, 18, 0, 0, 0, time.UTC)
	c := newDashBeadInfo(func(_, id string) *beads.Issue {
		reads.Add(1)
		if id == "gt-bad" {
			return nil
		}
		return &beads.Issue{ID: id, Title: "t"}
	})
	c.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		c.get("g", "gt-1")
		c.get("g", "gt-bad")
	}
	if reads.Load() != 2 {
		t.Fatalf("%d reads, want 2 (one per bead, a failed read included)", reads.Load())
	}
	now = now.Add(11 * time.Minute)
	c.get("g", "gt-1")
	if reads.Load() != 3 {
		t.Fatalf("%d reads after the ttl, want 3", reads.Load())
	}
}

func TestDashBadgeLabels(t *testing.T) {
	t.Parallel()
	got := dashBadgeLabels([]string{"gt:ready-to-land", "needs-human", "gt:deferred", "bug"})
	if len(got) != 2 || got[0] != "needs-human" || got[1] != "deferred" {
		t.Errorf("badges = %v", got)
	}
}
