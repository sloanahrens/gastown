package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
	gtevents "github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/landings"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
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

// TestBuildDashPolecatsCapsTheBeadDescription: a seat row carries the bead's
// description cut at dashboard.DescriptionCap characters (counted as runes, so
// a multibyte description is not cut mid-character), trimmed, and a bead with
// none carries none (gt-kk6gb). The description comes off the issue the seat
// already holds, so the row costs no read of its own.
func TestBuildDashPolecatsCapsTheBeadDescription(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 18, 0, 0, 0, time.UTC)
	cap1 := polecat.WorkstateDisposition{CountsTowardCapacity: true}
	seat := func(name, desc string) dashSeat {
		return dashSeat{Rig: "gastown", Name: name, Item: dashItem(polecat.StateWorking, cap1), Issue: &beads.Issue{ID: "gt-" + name, Title: "t", Description: desc}}
	}
	long := strings.Repeat("é", dashboard.DescriptionCap+250)
	pcs := buildDashPolecats(dashPolecatInputs{Now: now, SessionsKnown: true, Seats: []dashSeat{
		seat("long", long), seat("exact", strings.Repeat("a", dashboard.DescriptionCap)),
		seat("padded", "\n  short one \n"), seat("none", ""),
	}})
	got := map[string]string{}
	for _, p := range pcs {
		got[p.Name] = p.Description
	}
	if n := len([]rune(got["long"])); n != dashboard.DescriptionCap || !strings.HasSuffix(got["long"], "…") {
		t.Errorf("a long description is %d characters ending %q, want exactly %d ending in an ellipsis", n, got["long"][len(got["long"])-3:], dashboard.DescriptionCap)
	}
	if got["exact"] != strings.Repeat("a", dashboard.DescriptionCap) {
		t.Errorf("a description of exactly the cap must pass whole, got %d characters", len(got["exact"]))
	}
	if got["padded"] != "short one" {
		t.Errorf("a padded description = %q, want it trimmed", got["padded"])
	}
	if got["none"] != "" {
		t.Errorf("a bead with no description carries %q", got["none"])
	}
}

// countingBeads counts the reads a seat's work costs. It embeds a nil
// beads.Client, so any read it does not count (Show, List, ...) panics the
// test rather than passing unnoticed.
type countingBeads struct {
	beads.Client
	listing      []*beads.Issue
	listCalls    int
	showMultiple int
}

func (c *countingBeads) ListIssueStatuses(...beads.IssueStatus) ([]*beads.Issue, error) {
	c.listCalls++
	return c.listing, nil
}

func (c *countingBeads) ShowMultiple([]string) (map[string]*beads.Issue, error) {
	c.showMultiple++
	return nil, nil
}

// TestSeatDescriptionCostsNoBeadRead: the description a seat row carries rides
// on the issue the rig's one listing already returned, so adding a seat adds no
// bd read per poll, and building the rows reads nothing (gt-kk6gb).
func TestSeatDescriptionCostsNoBeadRead(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 15, 18, 0, 0, 0, time.UTC)
	names := []string{"agate", "amber", "onyx"}
	bd := &countingBeads{}
	hooks := map[string]string{}
	for _, n := range names {
		id := "gt-" + n
		bd.listing = append(bd.listing, &beads.Issue{ID: id, Title: n, Description: "about " + n,
			Status: string(beads.IssueStatusHooked), Assignee: "gastown/polecats/" + n})
		hooks[n] = id
	}
	active, err := listActivePolecatWorkByName(bd, "gastown", hooks)
	if err != nil {
		t.Fatalf("listActivePolecatWorkByName: %v", err)
	}
	if bd.listCalls != 1 || bd.showMultiple != 0 {
		t.Fatalf("reads for %d seats = %d listing, %d routed; want one listing and no per-seat read", len(names), bd.listCalls, bd.showMultiple)
	}
	cap1 := polecat.WorkstateDisposition{CountsTowardCapacity: true}
	var seats []dashSeat
	for _, n := range names {
		seats = append(seats, dashSeat{Rig: "gastown", Name: n, Item: dashItem(polecat.StateWorking, cap1), Issue: active[n]})
	}
	for _, p := range buildDashPolecats(dashPolecatInputs{Now: now, SessionsKnown: true, Seats: seats}) {
		if want := "about " + p.Name; p.Description != want {
			t.Errorf("%s: description = %q, want %q from the listing", p.Name, p.Description, want)
		}
	}
	if bd.listCalls != 1 || bd.showMultiple != 0 {
		t.Errorf("building the rows read beads again: %d listing, %d routed", bd.listCalls, bd.showMultiple)
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

// A sandboxed seat's session lives inside its sandbox, so the host tmux never
// lists it. The table classifies seats from the registry's names too — the
// answer gt polecat list reads — or every live sandbox seat reads sessionless
// and shows stalled (gt-43k7c). A seat the patrol has recorded dead leaves
// that answer.
func TestDashSeatSessionsCountsTheSandboxRegistry(t *testing.T) {
	t.Parallel()
	reg := session.NewPrefixRegistry()
	reg.Register("gt", "gastown")
	host := newPolecatSessionSet(reg, []string{"gt-agate"})
	dead := func(_ string, _ string, polecatName string) bool { return polecatName == "zombie" }
	got := dashSeatSessions(host, reg, []string{"gt-ember", "gt-zombie"}, dead)
	if _, ok := got.session("gastown", "agate"); !ok {
		t.Error("the host's own session left the set")
	}
	if entry, ok := got.session("gastown", "ember"); !ok || entry.Name != "gt-ember" {
		t.Errorf("a registry-listed sandbox seat = %+v %v, want it live", entry, ok)
	}
	if _, ok := got.session("gastown", "zombie"); ok {
		t.Error("a seat the patrol recorded dead must not read as running")
	}
	if entry, _ := got.session("gastown", "ember"); entry.Agent != "" {
		t.Errorf("a registry entry carries agent %q; the seat's agent comes from its agent bead", entry.Agent)
	}
}

// A sandbox seat is never shown stalled on the host tmux's silence: a live one
// is working at any age, one the patrol has not sampled yet is starting, and
// only the town's own answer of "not running" reads stalled (gt-43k7c).
func TestBuildDashPolecatsSandboxSeatLiveness(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 25, 8, 37, 0, 0, time.UTC)
	cap1 := polecat.WorkstateDisposition{CountsTowardCapacity: true}
	tests := []struct {
		name       string
		state      polecat.State
		live       bool // the town's session list has the seat
		sandbox    dashSandboxSeat
		noWork     bool
		want       string
		wantReason string
	}{
		{"live sandbox seat slung 8m ago", polecat.StateWorking, true, dashSandboxSeat{Seat: true}, false,
			dashboard.StateWorking, ""},
		{"no liveness sample yet", polecat.StateStalled, false, dashSandboxSeat{Seat: true}, false,
			dashboard.StateSpawning, "sandbox seat starting: no liveness sample yet"},
		{"sampled, not running", polecat.StateStalled, false, dashSandboxSeat{Seat: true, Sampled: true}, false,
			dashboard.StateStalled, "assigned work and no live session"},
		{"sandbox seat with no work", polecat.StateIdle, false, dashSandboxSeat{Seat: true}, true,
			dashboard.StateIdle, ""},
		{"host seat unchanged", polecat.StateStalled, false, dashSandboxSeat{}, false,
			dashboard.StateStalled, "assigned work and no live session"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			item := dashItem(tt.state, cap1)
			item.SessionRunning = tt.live
			s := dashSeat{Rig: "gastown", Name: "ember", Session: "gt-ember", Item: item,
				Issue: &beads.Issue{ID: "gt-1", Title: "one"}, Sandbox: tt.sandbox,
				HookedAt: now.Add(-8 * time.Minute)}
			if tt.noWork {
				s.Issue = nil
			}
			got := buildDashPolecats(dashPolecatInputs{Now: now, SessionsKnown: true, Seats: []dashSeat{s}})
			if len(got) != 1 {
				t.Fatalf("rows = %d, want 1", len(got))
			}
			if got[0].State != tt.want || got[0].Reason != tt.wantReason {
				t.Errorf("state = %q (%q), want %q (%q)", got[0].State, got[0].Reason, tt.want, tt.wantReason)
			}
			if wantSession := tt.live && !tt.noWork; wantSession != got[0].HasSession {
				t.Errorf("HasSession = %v, want %v", got[0].HasSession, wantSession)
			}
			if tt.live && got[0].LastActive != nil {
				t.Error("a session the host tmux cannot clock must carry no last-active time")
			}
		})
	}
}

// dashDispatchStarts is the Age column's clock: the bead's first dispatch
// across seats, which the overrun guard judges too, so the two readings cannot
// disagree (gt-tzbtf). An unreadable event log is an error, not a town where
// no bead was ever dispatched.
func TestDashDispatchStartsReadsTheBeadsFirstDispatch(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	line := func(ts, typ, bead, target string) string {
		return fmt.Sprintf(`{"ts":%q,"source":"gt","type":%q,"actor":"x","payload":{"bead":%q,"target":%q}}`, ts, typ, bead, target)
	}
	body := strings.Join([]string{
		line("2026-10-27T09:00:00Z", "sling", "gt-a", "gastown/polecats/capable"),
		line("2026-10-27T09:30:00Z", "sling", "gt-a", "gastown/polecats/rictus"),
		line("2026-10-27T10:00:00Z", "done", "gt-a", "gastown/polecats/rictus"),
		line("2026-10-27T10:30:00Z", "sling", "gt-a", "gastown/polecats/nux"),
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(town, gtevents.EventsFile), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := dashDispatchStarts(town)
	if err != nil {
		t.Fatalf("dashDispatchStarts: %v", err)
	}
	if want := time.Date(2026, 10, 27, 10, 30, 0, 0, time.UTC); !got["gt-a"].Equal(want) {
		t.Errorf("gt-a dispatch = %v, want the dispatch after its done event, %v", got["gt-a"], want)
	}
}

func TestDashDispatchStartsReportsAnUnreadableLog(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// A directory where the log belongs: the read fails, and a town whose
	// dispatches cannot be read must not date every bead from now.
	if err := os.Mkdir(filepath.Join(town, gtevents.EventsFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := dashDispatchStarts(town); err == nil {
		t.Fatal("dashDispatchStarts read an unreadable log as an empty one")
	}
}

// dashSeats fails closed on an unreadable dispatch record: the polecat panel
// reads as unavailable (the hub keeps its last good list) rather than dating
// every seat from a fallback the guard does not use (gt-tzbtf).
func TestDashSeatsReportsAnUnreadableDispatchRecord(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "mayor", "rigs.json"), []byte(`{"version":1,"rigs":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(town, gtevents.EventsFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := dashSeats(town, nil, false, newDashSeatCache(), fakeSessionLister{}); err == nil {
		t.Fatal("dashSeats read an unreadable dispatch record as a town with no dispatches")
	}
}

// The seat's record of the hook is carried to the pane, which falls back to it
// when the feed holds no dispatch line for the bead (gt-jwmxo). A seat holding
// no work has no bead to date, so the time is not carried.
func TestBuildDashPolecatsCarriesTheSeatsHookTime(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 11, 18, 0, 0, 0, time.UTC)
	hooked := now.Add(-2 * time.Hour)
	got := buildDashPolecats(dashPolecatInputs{
		Now: now, SessionsKnown: true,
		Seats: []dashSeat{
			{Rig: "gastown", Name: "working", HookedAt: hooked,
				Item: dashItem(polecat.StateWorking, polecat.WorkstateDisposition{}), Issue: &beads.Issue{ID: "gt-1"}},
			{Rig: "gastown", Name: "idle", HookedAt: hooked,
				Item: dashItem(polecat.StateIdle, polecat.WorkstateDisposition{Reusable: true})},
		},
	})
	byName := map[string]dashboard.Polecat{}
	for _, p := range got {
		byName[p.Name] = p
	}
	if p := byName["working"]; p.Hooked == nil || !p.Hooked.Equal(hooked) {
		t.Errorf("working = %+v (want the seat's hook time)", p)
	}
	if p := byName["idle"]; p.Hooked != nil {
		t.Errorf("idle = %+v (a seat holding no work has no bead to date)", p)
	}
}

// A queued row is dated by its submit, not its hook: the bead's last write is
// the label write gt done makes, which is the same field the Landings panel
// ages the same bead's wait from, so the two readings cannot disagree
// (gt-iv7jf). Every other state leaves the clock to the hook time the hub
// fills.
func TestBuildDashPolecatsDatesAQueuedRowByItsSubmit(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 25, 18, 0, 0, 0, time.UTC)
	submitted := now.Add(-3 * time.Minute)
	issue := &beads.Issue{ID: "gt-1", Title: "one", UpdatedAt: submitted.Format(time.RFC3339)}
	got := buildDashPolecats(dashPolecatInputs{
		Now: now, SessionsKnown: true, Ready: map[string]bool{"gt-1": true, "gt-3": true},
		Seats: []dashSeat{
			{Rig: "gastown", Name: "queued", HookedAt: now.Add(-15 * time.Minute),
				Item: dashItem(polecat.StateWorking, polecat.WorkstateDisposition{}), Issue: issue},
			{Rig: "gastown", Name: "working",
				Item:  dashItem(polecat.StateWorking, polecat.WorkstateDisposition{}),
				Issue: &beads.Issue{ID: "gt-2", Title: "two", UpdatedAt: submitted.Format(time.RFC3339)}},
			{Rig: "gastown", Name: "undated", HookedAt: now.Add(-15 * time.Minute),
				Item: dashItem(polecat.StateWorking, polecat.WorkstateDisposition{}), Issue: &beads.Issue{ID: "gt-3", Title: "three"}},
		},
	})
	byName := map[string]dashboard.Polecat{}
	for _, p := range got {
		byName[p.Name] = p
	}
	if p := byName["queued"]; p.State != dashboard.StateQueued || p.StateSince == nil || !p.StateSince.Equal(submitted) {
		t.Errorf("queued = %+v (want its state since the submit, %s)", p, submitted)
	}
	if p := byName["working"]; p.StateSince != nil {
		t.Errorf("working = %+v (only the queued clock is not the hook time)", p)
	}
	if p := byName["undated"]; p.StateSince != nil {
		t.Errorf("undated = %+v (a bead with no last write leaves the hook time)", p)
	}
}

// A queued row whose last write does not parse has no submit clock the page can
// read, so it dates the wait from the hook. The row says so: the fallback must
// not pass for the submit time (gt-uqmdg).
func TestBuildDashPolecatsSaysWhenAQueuedRowsLastWriteDoesNotParse(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 25, 18, 0, 0, 0, time.UTC)
	got := buildDashPolecats(dashPolecatInputs{
		Now: now, SessionsKnown: true, Ready: map[string]bool{"gt-1": true},
		Seats: []dashSeat{{
			Rig: "gastown", Name: "queued", HookedAt: now.Add(-15 * time.Minute),
			Item:  dashItem(polecat.StateWorking, polecat.WorkstateDisposition{}),
			Issue: &beads.Issue{ID: "gt-1", Title: "one", UpdatedAt: "yesterday evening"},
		}},
	})
	if len(got) != 1 {
		t.Fatalf("got %d polecats, want 1", len(got))
	}
	p := got[0]
	if p.State != dashboard.StateQueued {
		t.Errorf("state = %q, want queued", p.State)
	}
	if p.StateSince != nil {
		t.Errorf("StateSince = %v, want nil: the last write does not parse", p.StateSince)
	}
	if !strings.Contains(p.Reason, "last write could not be read") {
		t.Errorf("reason = %q, want it to say the last write could not be read", p.Reason)
	}
}

func TestBuildDashPolecatsCarriesTheRigParkedFlag(t *testing.T) {
	t.Parallel()
	got := buildDashPolecats(dashPolecatInputs{
		Now: time.Now(), SessionsKnown: true,
		Seats: []dashSeat{
			{Rig: "beads", Name: "mutant", RigParked: true, Item: dashItem(polecat.StateIdle, polecat.WorkstateDisposition{})},
			{Rig: "gastown", Name: "agate", Item: dashItem(polecat.StateIdle, polecat.WorkstateDisposition{})},
		},
	})
	flags := map[string]bool{}
	for _, p := range got {
		flags[p.Name] = p.RigParked
	}
	if !flags["mutant"] || flags["agate"] {
		t.Errorf("rig parked flags = %v", flags)
	}
}

// Each polecat row carries the agent its own session was started with, so a
// session left over from before a model switch shows its own model beside one
// started after it (gt-lqa6p).
func TestBuildDashPolecatsCarriesEachPolecatsAgent(t *testing.T) {
	t.Parallel()
	cap1 := polecat.WorkstateDisposition{CountsTowardCapacity: true}
	old := dashItem(polecat.StateWorking, cap1)
	old.Agent = "deepseek-flash"
	fresh := dashItem(polecat.StateWorking, cap1)
	fresh.Agent = "claude-sonnet"
	pcs := buildDashPolecats(dashPolecatInputs{
		Now:           time.Now(),
		SessionsKnown: true,
		Seats: []dashSeat{
			{Rig: "gastown", Name: "leftover", Item: old, Issue: &beads.Issue{ID: "gt-1", Title: "one"}},
			{Rig: "gastown", Name: "newer", Item: fresh, Issue: &beads.Issue{ID: "gt-2", Title: "two"}},
		},
	})
	got := map[string]string{}
	for _, p := range pcs {
		got[p.Name] = p.Agent
	}
	if got["leftover"] != "deepseek-flash" || got["newer"] != "claude-sonnet" {
		t.Errorf("polecat agents = %v, want each session's own", got)
	}
}

// TestDashBeadSummary: a seat's hover text drops the dispatcher's attachment
// lines (a multi-line attached_args included) and shows the bead's Goal body,
// or the first paragraph when there is no Goal section (gt-dhc6o).
func TestDashBeadSummary(t *testing.T) {
	t.Parallel()
	block := beads.FormatAttachmentFields(&beads.AttachmentFields{
		AttachedMolecule: "gt-wisp-1",
		AttachedFormula:  "mol-polecat-work",
		AttachedAt:       "2026-10-16T03:33:52Z",
		AttachedArgs:     "HOST SAFETY\nno_merge: true\nnever rm",
		AttachedVars:     []string{"issue=gt-1"},
		DispatchedBy:     "daemon/spec-dispatch",
		FormulaVars:      "issue=gt-1",
	})
	tests := []struct {
		name, desc, want string
	}{
		{"goal body only", block + "\n\n## Goal\n\nShow the goal.\nSecond line.\n\n## Constraints\n\n- one\n", "Show the goal.\nSecond line."},
		{"goal last section", block + "\n\n## Constraints\n\n- one\n\n## Goal\n\nTail goal.\n", "Tail goal."},
		{"no goal: first paragraph", block + "\n\nFirst para\nstill first.\n\nSecond para.", "First para\nstill first."},
		{"only attachment lines", block, ""},
		{"empty", "  \n", ""},
		{"padded plain", "\n  short one \n", "short one"},
		{"goal heading any case and indent", "Intro.\n\n  ## goal  \n\nBody.\n", "Body."},
		{"empty goal section", "## Goal\n\n## Constraints\n\n- one\n", ""},
		{"goal keeps its ### subheadings", "## Goal\n\nTop.\n\n### Detail\n\nMore.\n\n## Gate\n\nx", "Top.\n\n### Detail\n\nMore."},
		{"first goal section wins", "## Goal\n\nOne.\n\n## Goal\n\nTwo.\n", "One."},
		{"goal heading with trailing text is not a goal", "## Goal: ship\n\nBody.\n\nOther.", "## Goal: ship"},
		{"single hash goal is not a goal", "# Goal\n\nBody.\n\nOther.", "# Goal"},
		{"CRLF line endings", "## Goal\r\n\r\nCrlf body.\r\n\r\n## Gate\r\n\r\nx\r\n", "Crlf body."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := dashBeadSummary(tt.desc); got != tt.want {
				t.Errorf("dashBeadSummary = %q, want %q", got, tt.want)
			}
		})
	}
}
