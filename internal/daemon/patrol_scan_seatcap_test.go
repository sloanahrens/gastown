package daemon

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/patrolscan"
)

// gt-02tcp: the restart patrol_scan would make answers to the pool's cap, so
// the daemon reads polecat_pool the way the dispatcher does and counts the
// town's live polecats behind it. The census is the live half of the pool's
// picture: a witness, refinery or crew session holds no seat, and a polecat on
// another agent is not on the pool's seat.
func TestPatrolScanSeatCap(t *testing.T) {
	t.Parallel()
	const (
		poolAgent = "deepseek-flash"
		poolSeat  = "polecat_pool.seats.deepseek-flash.cap"
		total     = "polecat_pool.max_total"
	)
	pool := func(body string) string {
		return `{"type":"town-settings","version":1,"polecat_pool":{` + body + `}}`
	}
	polecat := func(rig, name, agent string) sessionEnv {
		return sessionEnv{rig: rig, name: name, agent: agent}
	}
	cases := []struct {
		name     string
		settings string
		sessions []sessionEnv
		want     patrolscan.SeatCap
		wantErr  bool
		// restart is the seat the read is for, "rig/name"; empty is a seat
		// with no session of its own among sessions.
		restart string
		// wantFull is what patrolscan acts on: whether the restart is held.
		wantFull bool
	}{
		{
			// gt-zwiww: an agent-gone seat keeps its session, and its restart
			// replaces that session, so it is not counted against the cap.
			name:     "the restarting seat's own session is the only one live",
			settings: pool(`"seats":{"` + poolAgent + `":{"cap":1}}`),
			sessions: []sessionEnv{polecat("gastown", "ruby", poolAgent)},
			restart:  "gastown/ruby",
		},
		{
			name:     "the restarting seat's own session is left out of the total too",
			settings: pool(`"seats":{"` + poolAgent + `":{"cap":3}},"max_total":2`),
			sessions: []sessionEnv{
				polecat("gastown", "ruby", poolAgent),
				polecat("gastown", "pearl", "deepseek-pro"),
			},
			restart: "gastown/ruby",
		},
		{
			name:     "other live seats still fill the cap against the restarting one",
			settings: pool(`"seats":{"` + poolAgent + `":{"cap":2}}`),
			sessions: []sessionEnv{
				polecat("gastown", "ruby", poolAgent),
				polecat("gastown", "pearl", poolAgent),
				polecat("gastown", "onyx", poolAgent),
			},
			restart:  "gastown/ruby",
			want:     patrolscan.SeatCap{Live: 2, Limit: 2, Key: poolSeat},
			wantFull: true,
		},
		{
			name:     "a polecat of the same name in another rig is another seat",
			settings: pool(`"seats":{"` + poolAgent + `":{"cap":1}}`),
			sessions: []sessionEnv{polecat("beads", "ruby", poolAgent)},
			restart:  "gastown/ruby",
			want:     patrolscan.SeatCap{Live: 1, Limit: 1, Key: poolSeat},
			wantFull: true,
		},
		{
			name:     "the pool seat is full",
			settings: pool(`"seats":{"` + poolAgent + `":{"cap":3}}`),
			sessions: []sessionEnv{
				polecat("gastown", "ruby", poolAgent),
				polecat("gastown", "pearl", poolAgent),
				polecat("gastown", "onyx", poolAgent),
			},
			want:     patrolscan.SeatCap{Live: 3, Limit: 3, Key: poolSeat},
			wantFull: true,
		},
		{
			name:     "the pool seat has room",
			settings: pool(`"seats":{"` + poolAgent + `":{"cap":3}}`),
			sessions: []sessionEnv{
				polecat("gastown", "ruby", poolAgent),
				polecat("gastown", "pearl", poolAgent),
			},
		},
		{
			name:     "the town is at its total across seats",
			settings: pool(`"seats":{"` + poolAgent + `":{"cap":3}},"max_total":2`),
			sessions: []sessionEnv{
				polecat("gastown", "ruby", poolAgent),
				polecat("gastown", "pearl", "deepseek-pro"),
			},
			want:     patrolscan.SeatCap{Live: 2, Limit: 2, Key: total},
			wantFull: true,
		},
		{
			name:     "sessions that hold no seat are not counted",
			settings: pool(`"seats":{"` + poolAgent + `":{"cap":3}}`),
			sessions: []sessionEnv{
				polecat("gastown", "ruby", poolAgent),
				polecat("gastown", "pearl", poolAgent),
				{rig: "gastown", name: "witness", crew: true},  // a session with no polecat role
				{rig: "gastown", name: "refinery", crew: true}, // a session with no polecat role
				{rig: "gastown", name: "sloan", crew: true},
			},
		},
		{
			// gt-h6423: gt pool set <agent>=0 closes the seat. The restart is
			// held like a full seat's, not read as "no cap".
			name:     "a seat the operator closed with cap 0 holds the restart",
			settings: `{"type":"town-settings","version":1,"polecat_pool":{"seats":{"` + poolAgent + `":{"cap":0}}}}`,
			sessions: []sessionEnv{polecat("gastown", "ruby", poolAgent)},
			restart:  "gastown/nova",
			want:     patrolscan.SeatCap{Live: 1, Closed: true, Key: "polecat_pool.seats." + poolAgent + ".cap"},
			wantFull: true,
		},
		{
			name:     "a closed seat holds the restart with no polecat live on it",
			settings: `{"type":"town-settings","version":1,"polecat_pool":{"seats":{"` + poolAgent + `":{"cap":0}}}}`,
			restart:  "gastown/nova",
			want:     patrolscan.SeatCap{Closed: true, Key: "polecat_pool.seats." + poolAgent + ".cap"},
			wantFull: true,
		},
		{
			name:     "a seat with room is not closed",
			settings: `{"type":"town-settings","version":1,"polecat_pool":{"seats":{"` + poolAgent + `":{"cap":2}}}}`,
			sessions: []sessionEnv{polecat("gastown", "ruby", poolAgent)},
		},
		{
			// gt-her01: a crash inside the seat's closed window spends no
			// seat, exactly as a sling to that seat is refused. The fixed
			// clock is at 15:04 UTC.
			name:     "a seat inside its closed window holds the restart",
			settings: pool(`"timezone":"UTC","seats":{"` + poolAgent + `":{"cap":3,"closed":"14:00-16:00"}}`),
			sessions: []sessionEnv{polecat("gastown", "pearl", poolAgent)},
			restart:  "gastown/ruby",
			want: patrolscan.SeatCap{
				Live:         1,
				Closed:       true,
				Key:          "polecat_pool.seats." + poolAgent + ".closed",
				ClosedDetail: "the seat is closed until 16:00 (polecat_pool.seats." + poolAgent + ".closed)",
			},
			wantFull: true,
		},
		{
			name:     "a seat outside its closed window takes the restart",
			settings: pool(`"timezone":"UTC","seats":{"` + poolAgent + `":{"cap":3,"closed":"16:00-18:00"}}`),
			sessions: []sessionEnv{polecat("gastown", "ruby", poolAgent)},
			restart:  "gastown/ruby",
		},
		{
			// A window the loader would refuse is not read as open: the
			// restart waits for the seat cap to be readable.
			name:     "a closed window with no timezone cannot be read",
			settings: pool(`"seats":{"` + poolAgent + `":{"cap":3,"closed":"14:00-16:00"}}`),
			sessions: []sessionEnv{polecat("gastown", "ruby", poolAgent)},
			restart:  "gastown/ruby",
			wantErr:  true,
		},
		{
			name:     "a pool with no cap set",
			settings: pool(`"min_spawn_gap":"4m"`),
		},
		{
			name:     "a town with no pool",
			settings: `{"type":"town-settings","version":1}`,
		},
		{
			name:     "a town with no settings file",
			settings: "",
		},
		{
			name:     "the settings cannot be read",
			settings: `{"type":"town-settings","version":1,"polecat_pool":{`,
			wantErr:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if tc.settings != "" {
				writeTownFile(t, town, "settings/config.json", tc.settings)
			}
			clk := newFixedClock()
			tm := newFakeTmux(clk)
			for _, s := range tc.sessions {
				seatEnv(t, tm, s)
			}
			// The closed window is read at the daemon's clock, the same fixed
			// clock the fake tmux runs on.
			h := &patrolScanHost{d: &Daemon{config: &Config{TownRoot: town}, tmux: tm, clock: clk}, agentRecord: noAgentRecord}

			restart := tc.restart
			if restart == "" {
				restart = "gastown/nova" // a seat none of the sessions belong to
			}
			rig, name, _ := strings.Cut(restart, "/")
			got, err := h.seatCap(rig, name)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("seatCap = %+v, want an error for unreadable settings", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("seatCap: %v", err)
			}
			if got != tc.want {
				t.Fatalf("seatCap = %+v, want %+v", got, tc.want)
			}
			if got.Full() != tc.wantFull {
				t.Fatalf("seatCap %+v: Full() = %v, want %v", got, got.Full(), tc.wantFull)
			}
		})
	}
}

// sessionEnv is one tmux session a seat-cap case runs: a polecat of rig/name
// on agent, or a session of another role, which GT_ROLE says holds no seat.
type sessionEnv struct {
	rig, name, agent string
	crew             bool
}

func seatEnv(t *testing.T, tm *fakeTmux, s sessionEnv) {
	t.Helper()
	role := s.rig + "/polecats/" + s.name
	if s.crew {
		role = s.rig + "/crew/" + s.name
	}
	env := map[string]string{"GT_ROLE": role}
	if s.agent != "" {
		env["GT_AGENT"] = s.agent
	}
	name := strings.Join([]string{s.rig, "session", s.name}, "-")
	if err := tm.NewSessionWithCommandAndEnv(name, "", "claude", env); err != nil {
		t.Fatal(err)
	}
}
