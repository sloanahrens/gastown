package cmd

import (
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/config"
)

// drainingTickRoster is the roster read of the gt-97ppg tick: capable is a live
// seat, pool-claim/c1 is a claim the roster counts, and gt-flash is a draining
// polecat the roster released and the sling still holds.
func drainingTickRoster() []poolSession {
	return []poolSession{
		{name: "gt-capable", agent: "claude-sonnet", rig: "gastown", polecat: "capable"},
		{name: "pool-claim/c1", agent: "deepseek-flash"},
	}
}

// drainingTickHeld is the sling-side read over the same tick: the roster's two
// occupancies, once each, and gt-flash, which the roster does not count.
func drainingTickHeld() []poolSession {
	return []poolSession{
		{name: "gt-capable", agent: "claude-sonnet", rig: "gastown", polecat: "capable"},
		{name: "gt-flash", agent: "claude-sonnet", rig: "gastown", polecat: "flash"},
		{name: "pool-claim/c1", agent: "deepseek-flash"},
	}
}

// TestSpecTickSlingSideReadsOnceAndSkipsOnTownTotal drives a tick over the
// golden board, with a town of three where the roster reads two and the
// sling-side read holds three. The occupancy is read exactly once for the whole
// tick, not once per candidate, and every candidate that reaches a seat is
// skipped on the town total with the exact line; nothing is slung.
func TestSpecTickSlingSideReadsOnceAndSkipsOnTownTotal(t *testing.T) {
	t.Parallel()
	f := specGoldenTown()
	f.maxTotal = 3
	env := f.env()
	occupiedReads := 0
	env.Roster = func() (specRoster, error) {
		return specRosterStep(
			func() (poolSeatSnapshot, error) { return poolSeatSnapshot{sessions: drainingTickRoster()}, nil },
			nil,
			func(poolSeatSnapshot) func() ([]poolSession, error) {
				return func() ([]poolSession, error) {
					occupiedReads++
					return drainingTickHeld(), nil
				}
			},
			func(poolSeatSnapshot) {},
		)
	}
	r := runSpecDispatchCycle(env)

	if occupiedReads != 1 {
		t.Fatalf("sling-side occupancy read %d times in one tick, want exactly 1", occupiedReads)
	}
	if len(f.slingRecords) != 0 {
		t.Fatalf("slung %d bead(s) on a town at its total, want none", len(f.slingRecords))
	}
	var skips []string
	for _, e := range r.Skipped {
		if strings.Contains(e.Line, "over the town total") {
			skips = append(skips, e.Line)
		}
	}
	sort.Strings(skips)
	const reason = ": no seat: over the town total: 3/3 live polecats across every seat"
	want := []string{
		"gt-box2" + reason,
		"gt-c1" + reason,
		"gt-c2" + reason,
		"gt-c3" + reason,
		"gt-c4" + reason,
		"gt-c5" + reason,
		"gt-c6" + reason,
		"gt-flip" + reason,
		"gt-fmt" + reason,
		"gt-last" + reason,
		"gt-late" + reason,
		"gt-second" + reason,
	}
	if !reflect.DeepEqual(skips, want) {
		t.Fatalf("town-total skip lines =\n%q\nwant\n%q", skips, want)
	}
	if len(r.Errors) != 0 {
		t.Fatalf("tick errors = %q, want none on a readable roster", r.Errors)
	}
}

// TestSpecTickRosterFailsClosed: a tick whose seat or sling-side read fails
// dispatches nothing, and the report names the failure in its exact words.
func TestSpecTickRosterFailsClosed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		read     func() (poolSeatSnapshot, error)
		occupied func() ([]poolSession, error)
		want     string
	}{
		{
			name:     "the seat snapshot read fails",
			read:     func() (poolSeatSnapshot, error) { return poolSeatSnapshot{}, errors.New("tmux down") },
			occupied: func() ([]poolSession, error) { return nil, nil },
			want:     "roster unreadable, not dispatching: listing polecat seats: tmux down",
		},
		{
			name: "the sling-side read fails",
			read: func() (poolSeatSnapshot, error) { return poolSeatSnapshot{sessions: drainingTickRoster()}, nil },
			occupied: func() ([]poolSession, error) {
				return nil, errors.New("tmux down")
			},
			want: "roster unreadable, not dispatching: counting the seats a sling will see: tmux down",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := specGoldenTown()
			f.maxTotal = 3
			env := f.env()
			env.Roster = func() (specRoster, error) {
				return specRosterStep(tc.read, nil,
					func(poolSeatSnapshot) func() ([]poolSession, error) { return tc.occupied },
					func(poolSeatSnapshot) {})
			}
			r := runSpecDispatchCycle(env)

			if len(f.slingRecords) != 0 {
				t.Fatalf("slung %d bead(s) on an unreadable roster, want none", len(f.slingRecords))
			}
			found := false
			for _, e := range r.Errors {
				if e == tc.want {
					found = true
				}
			}
			if !found {
				t.Fatalf("report errors = %q, want the line %q", r.Errors, tc.want)
			}
		})
	}
}

// tickListing is the tmux listing one tick reads twice: the seat snapshot lists
// first and the sling-side occupancy lists after it. joins are the sessions
// that spawn between the two reads, and failAt is the listing call that fails
// (0 for none).
type tickListing struct {
	*fakeLister
	joins  map[string]map[string]string
	reads  int
	failAt int
}

func (l *tickListing) ListSessions() ([]string, error) {
	l.reads++
	if l.reads == l.failAt {
		return nil, errors.New("tmux down")
	}
	if l.reads == 2 {
		for name, env := range l.joins {
			l.sessions[name] = env
		}
	}
	return l.fakeLister.ListSessions()
}

// TestSpecTickRosterReadsSlingSideOnceAndCountsTheSpawn runs the production
// roster wiring (specDispatchRoster) over fakes. One tick lists the seats once and
// reads the sling-side occupancy once, and a session that spawned between the
// two reads counts as draining, as the sling counts it. A failed occupancy read
// fails the roster, so the tick dispatches nothing on it.
func TestSpecTickRosterReadsSlingSideOnceAndCountsTheSpawn(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTestRigsConfig(t, town, "gastown")
	db := beadsfake.New()
	ts := &config.TownSettings{PolecatPool: seatPool("claude-sonnet", 5)}
	store := func(string) beads.Client { return db }
	capable := map[string]string{"GT_ROLE": "gastown/polecats/capable", "GT_AGENT": "claude-sonnet"}
	spawn := map[string]string{"GT_ROLE": "gastown/polecats/spawn", "GT_AGENT": "claude-sonnet"}
	cases := []struct {
		name         string
		failAt       int
		wantDraining int
		wantErr      string
	}{
		{name: "the spawn between the two reads is draining", wantDraining: 1},
		{name: "a failed occupancy read fails the roster", failAt: 2, wantErr: "counting the seats a sling will see"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			lister := &tickListing{
				fakeLister: &fakeLister{
					sessions: map[string]map[string]string{"gt-capable": capable},
					created:  map[string]time.Time{"gt-capable": time.Now().Add(-time.Hour)},
				},
				joins:  map[string]map[string]string{"gt-spawn": spawn},
				failAt: tc.failAt,
			}
			var snap atomic.Pointer[poolSeatSnapshot]
			roster := specDispatchRoster(newSpecTickReads(), town, ts, func() sessionLister { return lister }, store, &snap)
			got, err := roster()

			if tc.failAt == 0 && lister.reads != 2 {
				t.Fatalf("the tick listed the sessions %d times, want 2: the seat snapshot and the sling-side occupancy once each", lister.reads)
			}
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("roster error = %v, want it to contain %q", err, tc.wantErr)
				}
				if snap.Load() == nil {
					t.Fatal("the seat snapshot is not kept for the slings when the occupancy read fails")
				}
				return
			}
			if err != nil {
				t.Fatalf("roster: %v", err)
			}
			if got.Live["claude-sonnet"] != 1 {
				t.Fatalf("roster live = %d, want 1 (gt-capable)", got.Live["claude-sonnet"])
			}
			if got.Draining["claude-sonnet"] != tc.wantDraining {
				t.Fatalf("draining = %d, want %d: gt-spawn is counted by the sling and not the roster", got.Draining["claude-sonnet"], tc.wantDraining)
			}
		})
	}
}

// TestSpecTickFailsClosedOnItsOccupancyRead runs the production roster wiring
// through the tick's cycle: when the sling-side read fails, the tick logs the
// fail-closed line and starts no sling, on the golden board that would
// otherwise dispatch.
func TestSpecTickFailsClosedOnItsOccupancyRead(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTestRigsConfig(t, town, "gastown")
	ts := &config.TownSettings{PolecatPool: seatPool("claude-sonnet", 5)}
	lister := &tickListing{
		fakeLister: &fakeLister{sessions: map[string]map[string]string{
			"gt-capable": {"GT_ROLE": "gastown/polecats/capable", "GT_AGENT": "claude-sonnet"},
		}},
		failAt: 2,
	}
	var snap atomic.Pointer[poolSeatSnapshot]
	f := specGoldenTown()
	env := f.env()
	env.Roster = specDispatchRoster(newSpecTickReads(), town, ts, func() sessionLister { return lister }, func(string) beads.Client { return beadsfake.New() }, &snap)
	r := runSpecDispatchCycle(env)

	if len(f.slingRecords) != 0 {
		t.Fatalf("slung %d bead(s) on an unreadable occupancy, want none", len(f.slingRecords))
	}
	const want = "roster unreadable, not dispatching: counting the seats a sling will see: tmux down"
	if len(r.Errors) != 1 || r.Errors[0] != want {
		t.Fatalf("report errors = %q, want exactly %q", r.Errors, want)
	}
}

// TestSpecTickCountsAnInFlightSlingAndADrainingSessionOnce pins the tick total when a
// sling is running for the same agent as a draining session: the roster's two
// occupancies, the draining session the sling still counts, and the running sling
// each enter the total once, so it is 4 and a town of four takes no more. An in-flight
// sling carries an agent, not a session key, so it cannot suppress or duplicate a held
// session; a sling that spawned between the two reads is the one overlap, and it
// errs toward fewer slings (gt-97ppg). Mutation: skip a held session whose agent has
// a sling in flight, and the total reads 3 and the candidates are slung.
func TestSpecTickCountsAnInFlightSlingAndADrainingSessionOnce(t *testing.T) {
	t.Parallel()
	f := specGoldenTown()
	f.maxTotal = 4
	env := f.env()
	env.InFlight = []specInFlight{{Bead: "gt-running", Agent: "claude-sonnet", Started: time.Now()}}
	env.Roster = func() (specRoster, error) {
		return specRosterStep(
			func() (poolSeatSnapshot, error) { return poolSeatSnapshot{sessions: drainingTickRoster()}, nil },
			nil,
			func(poolSeatSnapshot) func() ([]poolSession, error) {
				return func() ([]poolSession, error) { return drainingTickHeld(), nil }
			},
			func(poolSeatSnapshot) {},
		)
	}
	r := runSpecDispatchCycle(env)

	if len(f.slingRecords) != 0 {
		t.Fatalf("slung %d bead(s) on a town at its total of 4, want none", len(f.slingRecords))
	}
	var total []string
	for _, e := range r.Skipped {
		if strings.Contains(e.Line, "over the town total") {
			total = append(total, e.Line)
		}
	}
	if len(total) == 0 {
		t.Fatalf("no candidate was skipped on the town total; skipped = %q", r.Skipped)
	}
	for _, line := range total {
		if !strings.Contains(line, ": no seat: over the town total: 4/4 live polecats across every seat") {
			t.Errorf("skip line %q does not read the total 4/4 (2 roster + 1 draining + 1 in flight)", line)
		}
	}
}
