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
		poolSeat  = "polecat_pool.max_seats"
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
	}{
		{
			// gt-zwiww: an agent-gone seat keeps its session, and its restart
			// replaces that session, so it is not counted against the cap.
			name:     "the restarting seat's own session is the only one live",
			settings: pool(`"agent":"` + poolAgent + `","max_seats":1`),
			sessions: []sessionEnv{polecat("gastown", "ruby", poolAgent)},
			restart:  "gastown/ruby",
		},
		{
			name:     "the restarting seat's own session is left out of the total too",
			settings: pool(`"agent":"` + poolAgent + `","max_seats":3,"max_total":2`),
			sessions: []sessionEnv{
				polecat("gastown", "ruby", poolAgent),
				polecat("gastown", "pearl", "deepseek-pro"),
			},
			restart: "gastown/ruby",
		},
		{
			name:     "other live seats still fill the cap against the restarting one",
			settings: pool(`"agent":"` + poolAgent + `","max_seats":2`),
			sessions: []sessionEnv{
				polecat("gastown", "ruby", poolAgent),
				polecat("gastown", "pearl", poolAgent),
				polecat("gastown", "onyx", poolAgent),
			},
			restart: "gastown/ruby",
			want:    patrolscan.SeatCap{Live: 2, Limit: 2, Key: poolSeat},
		},
		{
			name:     "a polecat of the same name in another rig is another seat",
			settings: pool(`"agent":"` + poolAgent + `","max_seats":1`),
			sessions: []sessionEnv{polecat("beads", "ruby", poolAgent)},
			restart:  "gastown/ruby",
			want:     patrolscan.SeatCap{Live: 1, Limit: 1, Key: poolSeat},
		},
		{
			name:     "the pool seat is full",
			settings: pool(`"agent":"` + poolAgent + `","max_seats":3`),
			sessions: []sessionEnv{
				polecat("gastown", "ruby", poolAgent),
				polecat("gastown", "pearl", poolAgent),
				polecat("gastown", "onyx", poolAgent),
			},
			want: patrolscan.SeatCap{Live: 3, Limit: 3, Key: poolSeat},
		},
		{
			name:     "the pool seat has room",
			settings: pool(`"agent":"` + poolAgent + `","max_seats":3`),
			sessions: []sessionEnv{
				polecat("gastown", "ruby", poolAgent),
				polecat("gastown", "pearl", poolAgent),
			},
		},
		{
			name:     "the town is at its total across seats",
			settings: pool(`"agent":"` + poolAgent + `","max_seats":3,"max_total":2`),
			sessions: []sessionEnv{
				polecat("gastown", "ruby", poolAgent),
				polecat("gastown", "pearl", "deepseek-pro"),
			},
			want: patrolscan.SeatCap{Live: 2, Limit: 2, Key: total},
		},
		{
			name:     "sessions that hold no seat are not counted",
			settings: pool(`"agent":"` + poolAgent + `","max_seats":3`),
			sessions: []sessionEnv{
				polecat("gastown", "ruby", poolAgent),
				polecat("gastown", "pearl", poolAgent),
				{rig: "gastown", name: "witness"},  // a witness session
				{rig: "gastown", name: "refinery"}, // a refinery session
				{rig: "gastown", name: "sloan", crew: true},
			},
		},
		{
			name:     "a pool with no cap set",
			settings: pool(`"agent":"` + poolAgent + `"`),
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
			tm := newFakeTmux(newFixedClock())
			for _, s := range tc.sessions {
				seatEnv(t, tm, s)
			}
			h := &patrolScanHost{d: &Daemon{config: &Config{TownRoot: town}, tmux: tm}, agentRecord: noAgentRecord}

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
