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
	}{
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
			h := &patrolScanHost{d: &Daemon{config: &Config{TownRoot: town}, tmux: tm}}

			got, err := h.seatCap()
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
