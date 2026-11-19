package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// Pool count: a running sling and the roster seat that hooks its bead are one
// seat (gt-75fma5). The roster counts a seat as soon as its session, claim,
// hook or submission exists, and the tick adds every running sling to its
// agent's live count, so one polecat read 10 on an 8-seat town. The seat's
// hooked bead is what lets the tick tell the two apart.

// TestSpecTickCountsAHookedSlingOnce is gt-75fma5's first acceptance box: a
// roster seat hooking gt-a plus a running sling for gt-a is one seat, whatever
// source the roster counted the seat from. The tick line reads 1/2 with no
// in-flight term, because the sling was not added.
func TestSpecTickCountsAHookedSlingOnce(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		seat poolSession
		// want is the tick line. A dead-hooked seat names its own part: the
		// seat has no session, which is what the term explains.
		want string
	}{
		{"a live session", poolSession{name: "gt-ember", agent: "deepseek-flash", rig: "gastown", polecat: "ember", hookedBead: "gt-a"}, "deepseek-flash 1/2"},
		{"a dead-hooked seat", poolSession{name: "dead/gastown/jade", agent: "deepseek-flash", rig: "gastown", polecat: "jade", deadHooked: true, hookedBead: "gt-a"}, "deepseek-flash 1/2 (0 live + 1 dead-hooked)"},
		{"a claim", poolSession{name: "pool-claim/c1", agent: "deepseek-flash", claim: true, hookedBead: "gt-a"}, "deepseek-flash 1/2"},
		{"a landing seat", poolSession{name: "landing/gastown/pike", agent: "deepseek-flash", rig: "gastown", polecat: "pike", landing: true, hookedBead: "gt-a"}, "deepseek-flash 1/2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeSpecTown()
			f.seats = []specdispatch.Seat{{Agent: "deepseek-flash", Cap: 2}}
			f.roster = specRosterFrom([]poolSession{tc.seat}, nil)
			env := backgroundEnv(f)
			env.InFlight = []specInFlight{{Bead: "gt-a", Rig: "gastown", Agent: "deepseek-flash"}}
			r := runSpecDispatchCycle(env)

			if r.Roster != tc.want {
				t.Fatalf("roster = %q, want %q: the running sling and the seat hooking gt-a are one seat", r.Roster, tc.want)
			}
		})
	}
}

// TestSpecTickStillCountsASlingNoSeatHooks is the other half of the same box:
// a running sling whose bead no roster seat hooks still adds one, so a seat the
// sling has not made yet is counted. A seat whose hook cannot be read names no
// bead and counts the sling as it did before. The in-flight term is the slings
// that were added, so the parts of the line still sum to the count the seat
// choice compared to the cap.
func TestSpecTickStillCountsASlingNoSeatHooks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		seats []poolSession
		want  string
	}{
		{"no seat at all", nil, "deepseek-flash 1/2 (0 live + 1 in flight (may double count))"},
		{"a seat whose hook names other work", []poolSession{
			{name: "gt-ember", agent: "deepseek-flash", rig: "gastown", polecat: "ember", hookedBead: "gt-other"},
		}, "deepseek-flash 2/2 (1 live + 1 in flight (may double count))"},
		{"a seat whose hook cannot be read", []poolSession{
			{name: "gt-ember", agent: "deepseek-flash", rig: "gastown", polecat: "ember"},
		}, "deepseek-flash 2/2 (1 live + 1 in flight (may double count))"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeSpecTown()
			f.seats = []specdispatch.Seat{{Agent: "deepseek-flash", Cap: 2}}
			f.roster = specRosterFrom(tc.seats, nil)
			env := backgroundEnv(f)
			env.InFlight = []specInFlight{{Bead: "gt-a", Rig: "gastown", Agent: "deepseek-flash"}}
			r := runSpecDispatchCycle(env)

			if r.Roster != tc.want {
				t.Fatalf("roster = %q, want %q", r.Roster, tc.want)
			}
		})
	}
}

// writeSeatClaimForBead stages a sling's claim naming the bead it is for, at
// the path and in the shape poolSeatClaimSessions reads.
func writeSeatClaimForBead(t *testing.T, townRoot, id, agent, bead string) {
	t.Helper()
	dir := poolSeatClaimDir(townRoot)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(poolSeatClaim{ID: id, PID: os.Getpid(), Agent: agent, Bead: bead, CreatedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestPoolSeatSessionsCarryTheBeadTheirHookNames is the wiring: every kind of
// seat the roster counts carries the bead a running sling for it would be
// counted twice for — the live session's hook, the dead-hooked seat's hook, the
// claim's bead and the landing record's bead. A hook that names work already
// landed, or one that cannot be read, carries none, so its sling counts as
// before. The roster collects exactly the carried beads (gt-75fma5).
func TestPoolSeatSessionsCarryTheBeadTheirHookNames(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTestRigsConfig(t, town, "gastown")
	db := beadsfake.New()
	lister := &fakeLister{sessions: map[string]map[string]string{}}

	liveSeat := func(name, hook string) {
		writeTownPolecatDir(t, town, "gastown", name)
		seedPolecatAgentBead(db, town, "gastown", name, hook)
		lister.sessions["gt-"+name] = map[string]string{"GT_ROLE": "gastown/polecats/" + name, "GT_AGENT": "deepseek-flash"}
	}
	// A live session hooking live work.
	liveSeat("ember", "gt-live")
	// A live session whose hook names work that already landed.
	liveSeat("pike", "gt-landed")
	// A live session whose hook cannot be read.
	liveSeat("wren", "gt-nowhere")
	// A seat with no session whose hook still names live work.
	writeTownPolecatDir(t, town, "gastown", "jade")
	seedPolecatAgentBead(db, town, "gastown", "jade", "gt-dead")
	// A claim a sling holds before its session exists, and a seat mid-landing.
	writeSeatClaimForBead(t, town, "claim-1", "deepseek-flash", "gt-claim")
	writeLandingSeatOnAgent(t, town, "gastown", "ruby", "gt-ruby", "deepseek-flash")

	db.Seed(
		beads.Issue{ID: "gt-live", Status: beads.StatusHooked, Assignee: "gastown/polecats/ember"},
		beads.Issue{ID: "gt-dead", Status: string(beads.StatusInProgress), Assignee: "gastown/polecats/jade"},
		beads.Issue{ID: "gt-landed", Status: "closed", Assignee: "gastown/polecats/pike"},
		beads.Issue{ID: "gt-ruby", Status: string(beads.StatusOpen), Labels: []string{land.LabelReadyToLand}, Assignee: "gastown/polecats/ruby"},
	)
	recordSlungSeats(t, town, lister)

	batch := batchOverStore(town, db)
	pool := seatPool("deepseek-flash", 30)
	seats, err := poolSeatSessionsWith(lister, town, batch.disposition, batch.hookHeld, submittedWork("gt-ruby"), pool)
	if err != nil {
		t.Fatalf("poolSeatSessionsWith: %v", err)
	}

	byName := make(map[string]poolSession, len(seats))
	for _, s := range seats {
		byName[s.name] = s
	}
	want := map[string]string{
		"gt-ember":             "gt-live",
		"dead/gastown/jade":    "gt-dead",
		"pool-claim/claim-1":   "gt-claim",
		"landing/gastown/ruby": "gt-ruby",
		"gt-pike":              "",
		"gt-wren":              "",
	}
	for name, bead := range want {
		s, ok := byName[name]
		if !ok {
			t.Fatalf("seat %s is missing from the picture (%+v)", name, seats)
		}
		if s.hookedBead != bead {
			t.Errorf("seat %s carries hookedBead %q, want %q", name, s.hookedBead, bead)
		}
	}

	ts := config.NewTownSettings()
	ts.PolecatPool = pool
	roster := specRosterFrom(seats, ts)
	for _, bead := range []string{"gt-live", "gt-dead", "gt-claim", "gt-ruby"} {
		if !roster.HookedBeads[bead] {
			t.Errorf("the roster does not name %s among its hooked beads (%v)", bead, roster.HookedBeads)
		}
	}
	for _, bead := range []string{"gt-landed", "gt-nowhere"} {
		if roster.HookedBeads[bead] {
			t.Errorf("the roster names %s among its hooked beads, want only the live hooks (%v)", bead, roster.HookedBeads)
		}
	}
}

// TestSpecRosterHookedBeadsSkipAParkedStaleSeat keeps the released seat out of
// the dedupe: a parked seat past its release holds nothing and counts nothing,
// so a sling for its bead is not hidden behind it (gt-75fma5, gt-t7k92).
func TestSpecRosterHookedBeadsSkipAParkedStaleSeat(t *testing.T) {
	t.Parallel()
	roster := specRosterFrom([]poolSession{
		{name: "dead/gastown/mariner", agent: "deepseek-flash", parkedStale: true, hookedBead: "gt-a"},
	}, nil)
	if roster.Live["deepseek-flash"] != 0 {
		t.Fatalf("a parked stale seat was counted in Live (%v)", roster.Live)
	}
	if roster.HookedBeads["gt-a"] {
		t.Fatalf("a parked stale seat's bead reached HookedBeads (%v), want it released with the seat", roster.HookedBeads)
	}
}
