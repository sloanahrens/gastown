package cmd

import (
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/polecat"
)

func seatMapTown(seats ...string) *config.TownSettings {
	ts := config.NewTownSettings()
	ts.PolecatPool = &config.PolecatPool{Seats: map[string]config.SeatConfig{}}
	for _, a := range seats {
		ts.PolecatPool.Seats[a] = config.SeatConfig{Cap: 3}
	}
	return ts
}

// TestSeatLiveRosterCountsEachSeatOnItsAgent is gt-mukgk: three agents' live
// polecats are each counted on their own seat, and a polecat the seat map
// cannot place is listed, not counted on a seat it may not hold.
func TestSeatLiveRosterCountsEachSeatOnItsAgent(t *testing.T) {
	t.Parallel()
	three := seatMapTown("claude-sonnet", "claude-haiku", "deepseek-flash")
	cases := []struct {
		name         string
		ts           *config.TownSettings
		sessions     []poolSession
		want         map[string]int
		wantNoAgent  []string
		wantDeadHook map[string]int
	}{
		{
			name: "three agents each on their own seat",
			ts:   three,
			sessions: []poolSession{
				{name: "a", agent: "claude-sonnet"},
				{name: "b", agent: "claude-haiku"},
				{name: "c", agent: "claude-haiku"},
				{name: "d", agent: "deepseek-flash"},
			},
			want: map[string]int{"claude-sonnet": 1, "claude-haiku": 2, "deepseek-flash": 1},
		},
		{
			name: "a polecat with no recorded agent is listed and fills no seat",
			ts:   three,
			sessions: []poolSession{
				{name: "a", agent: "claude-sonnet"},
				{name: "b"},
			},
			want:        map[string]int{"claude-sonnet": 1, config.UnknownSeatAgent: 1},
			wantNoAgent: []string{"b"},
		},
		{
			name:     "a single-seat map is where an unrecorded polecat sits",
			ts:       seatMapTown("claude-haiku"),
			sessions: []poolSession{{name: "a"}},
			want:     map[string]int{"claude-haiku": 1},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := specRosterFrom(c.sessions, c.ts)
			if !reflect.DeepEqual(r.Live, c.want) {
				t.Fatalf("Live = %v, want %v", r.Live, c.want)
			}
			if !reflect.DeepEqual(r.Unrecorded, c.wantNoAgent) {
				t.Fatalf("Unrecorded = %v, want %v", r.Unrecorded, c.wantNoAgent)
			}
		})
	}
}

// TestSeatLiveSessionlessSeatsOnTheSeatMap is the gt-2q283 case on a pool that
// names its agents in the seat map and has no polecat_pool.agent: a seat with no
// session, mid-landing or dead-hooked, counts on the agent its record names, and
// one whose record names none counts on the only seat, or on the polecat role
// default where the map has several: the seat its restart lands on (gt-zj9yf).
func TestSeatLiveSessionlessSeatsOnTheSeatMap(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		deadHooked bool
		slungAgent string
		seats      []string
		// roleDefault is the town's default agent, which a restart runs a seat
		// with no record on;
		// empty is claude-sonnet.
		roleDefault string
		want        map[string]int
	}{
		{"mid-landing on its recorded agent", false, "claude-haiku", []string{"claude-sonnet", "claude-haiku"}, "", map[string]int{"claude-haiku": 1}},
		{"dead-hooked on its recorded agent", true, "claude-haiku", []string{"claude-sonnet", "claude-haiku"}, "", map[string]int{"claude-haiku": 1}},
		{"mid-landing, no record, several seats, on the role default", false, "", []string{"claude-sonnet", "claude-haiku"}, "", map[string]int{"claude-sonnet": 1}},
		{"dead-hooked, no record, one seat", true, "", []string{"claude-haiku"}, "", map[string]int{"claude-haiku": 1}},
		{"mid-landing, no record, role default is no seat, counts on no seat", false, "", []string{"claude-sonnet", "claude-haiku"}, "claude-opus", map[string]int{config.UnknownSeatAgent: 1}},
		{"dead-hooked, no record, role default is no seat, counts on no seat", true, "", []string{"claude-sonnet", "claude-haiku"}, "claude-opus", map[string]int{config.UnknownSeatAgent: 1}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ts := seatMapTown(c.seats...)
			// The role default a restart starts a seat with no record on
			// (gt-zj9yf): the town's default agent, role_agents.polecat being
			// retired.
			roleDefault := c.roleDefault
			if roleDefault == "" {
				roleDefault = "claude-sonnet"
			}
			ts.DefaultAgent = roleDefault
			town := t.TempDir()
			if err := config.SaveTownSettings(config.TownSettingsPath(town), ts); err != nil {
				t.Fatalf("SaveTownSettings: %v", err)
			}
			writeTestRigsConfig(t, town, "gastown")
			writeTownPolecatDir(t, town, "gastown", "jade")
			seat := intent.Seat{Rig: "gastown", Role: constants.RolePolecat, Name: "jade"}
			rec := intent.Record{Agent: c.slungAgent}
			var disposition polecatDispositionFunc
			var work poolSeatWorkFunc
			if c.deadHooked {
				disposition = hookedNeedsRecovery
			} else {
				rec.Desired = intent.DesiredSubmitted
				rec.WorkBead = "gt-jade"
				work = submittedWork("gt-jade")
			}
			writeIntentRecord(t, seat, town, rec)

			sessions, err := poolSeatSessionsWith(&fakeLister{}, town, disposition, work, ts.PolecatPool)
			if err != nil {
				t.Fatalf("poolSeatSessionsWith: %v", err)
			}
			if got := specRosterFrom(sessions, ts).Live; !reflect.DeepEqual(got, c.want) {
				t.Fatalf("Live = %v, want %v (%+v)", got, c.want, sessions)
			}
		})
	}
}

// hookedNeedsRecovery is the seat-state read of a crashed polecat whose bead is
// still hooked.
func hookedNeedsRecovery(_, _ string) (poolSeatState, error) {
	return poolSeatState{
		Disposition: polecat.WorkstateDisposition{
			Verdict:              polecat.WorkstateVerdictNeedsRecovery,
			NeedsRecovery:        true,
			CountsTowardCapacity: true,
		},
		HookHeld: true,
	}, nil
}
