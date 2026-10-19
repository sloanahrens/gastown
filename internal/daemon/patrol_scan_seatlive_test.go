package daemon

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"

	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/patrolscan"
)

// noAgentRecord is the agent bead read of a town whose beads record no agent.
func noAgentRecord(rig, name string) (string, error) { return "", nil }

// recordedAgents is an agent bead read over a fixed "rig/name" -> agent table.
func recordedAgents(table map[string]string) func(rig, name string) (string, error) {
	return func(rig, name string) (string, error) { return table[rig+"/"+name], nil }
}

// seatMapSettings is a town settings file whose polecat_pool carries the seats.
func seatMapSettings(seats string) string {
	return `{"type":"town-settings","version":1,"polecat_pool":{"seats":{` + seats + `}}}`
}

const threeSeats = `"claude-sonnet":{"cap":3},"claude-haiku":{"cap":3},"deepseek-flash":{"cap":3}`

func seatLiveHost(t *testing.T, settings string, sessions []sessionEnv, records func(rig, name string) (string, error)) (*patrolScanHost, *bytes.Buffer) {
	t.Helper()
	town := t.TempDir()
	writeTownFile(t, town, "settings/config.json", settings)
	tm := newFakeTmux(newFixedClock())
	for _, s := range sessions {
		seatEnv(t, tm, s)
	}
	var logs bytes.Buffer
	return &patrolScanHost{
		d:           &Daemon{config: &Config{TownRoot: town}, tmux: tm, logger: log.New(&logs, "", 0)},
		agentRecord: records,
	}, &logs
}

func seatLivePool(t *testing.T, settings string) *agentconfig.PolecatPool {
	t.Helper()
	town := t.TempDir()
	writeTownFile(t, town, "settings/config.json", settings)
	ts, err := agentconfig.LoadOrCreateTownSettings(agentconfig.TownSettingsPath(town))
	if err != nil {
		t.Fatal(err)
	}
	return ts.PolecatPool
}

// The census counts each live polecat on the agent its agent bead recorded, not
// on the GT_AGENT of its session, which is the role default for a seat slung
// without one (gt-mukgk).
func TestSeatLiveCountsOnTheRecordedAgent(t *testing.T) {
	t.Parallel()
	settings := seatMapSettings(threeSeats)
	h, logs := seatLiveHost(t, settings, []sessionEnv{
		{rig: "gastown", name: "ruby", agent: "claude-sonnet"},
		{rig: "gastown", name: "pearl", agent: "claude-sonnet"}, // role default; the bead says haiku
		{rig: "gastown", name: "onyx"},                          // no GT_AGENT; the bead says haiku
		{rig: "gastown", name: "jade", agent: "deepseek-flash"},
		{rig: "gastown", name: "witness", crew: true},
	}, recordedAgents(map[string]string{
		"gastown/ruby":  "claude-sonnet",
		"gastown/pearl": "claude-haiku",
		"gastown/onyx":  "claude-haiku",
		"gastown/jade":  "deepseek-flash",
	}))

	byAgent, total, err := h.livePolecatCensus("", seatLivePool(t, settings))
	if err != nil {
		t.Fatalf("livePolecatCensus: %v", err)
	}
	want := map[string]int{"claude-sonnet": 1, "claude-haiku": 2, "deepseek-flash": 1}
	if total != 4 || len(byAgent) != len(want) {
		t.Fatalf("census = %v (total %d), want %v (total 4)", byAgent, total, want)
	}
	for agent, n := range want {
		if byAgent[agent] != n {
			t.Fatalf("census = %v, want %v", byAgent, want)
		}
	}
	if logs.Len() != 0 {
		t.Fatalf("every polecat records an agent, but the census logged %q", logs.String())
	}
}

// A polecat whose bead and session name no agent is counted on the seat map's
// only seat, and on UnknownSeatAgent when there are several: never on a seat it may
// not hold. The census lists the latter once per tick.
func TestSeatLiveUnrecordedAgent(t *testing.T) {
	t.Parallel()
	sessions := []sessionEnv{
		{rig: "gastown", name: "ruby", agent: "claude-haiku"},
		{rig: "gastown", name: "pearl"},
		{rig: "gastown", name: "onyx"},
	}
	cases := []struct {
		name     string
		settings string
		want     map[string]int
		wantLog  bool
	}{
		{
			name:     "several seats: counted on none and listed",
			settings: seatMapSettings(threeSeats),
			want:     map[string]int{"claude-haiku": 1, agentconfig.UnknownSeatAgent: 2},
			wantLog:  true,
		},
		{
			name:     "one seat: counted on it, nothing to report",
			settings: seatMapSettings(`"claude-sonnet":{"cap":3}`),
			want:     map[string]int{"claude-haiku": 1, "claude-sonnet": 2},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, logs := seatLiveHost(t, tc.settings, sessions, noAgentRecord)
			pool := seatLivePool(t, tc.settings)
			for i := 0; i < 2; i++ { // two restarts in one tick ask for the census
				byAgent, total, err := h.livePolecatCensus("", pool)
				if err != nil {
					t.Fatalf("livePolecatCensus: %v", err)
				}
				if total != 3 || len(byAgent) != len(tc.want) {
					t.Fatalf("census = %v (total %d), want %v (total 3)", byAgent, total, tc.want)
				}
				for agent, n := range tc.want {
					if byAgent[agent] != n {
						t.Fatalf("census = %v, want %v", byAgent, tc.want)
					}
				}
			}
			if got := strings.Count(logs.String(), "record no agent"); (got == 1) != tc.wantLog || got > 1 {
				t.Fatalf("logged the unrecorded polecats %d time(s) over two census reads, want %v once:\n%s", got, tc.wantLog, logs.String())
			}
			if tc.wantLog && !strings.Contains(logs.String(), "gastown/polecats/pearl") {
				t.Fatalf("the log does not name the polecats that record no agent:\n%s", logs.String())
			}
		})
	}
}

// A restart answers to the seat of the agent its polecat was spawned on.
func TestSeatLiveCapIsPerSeat(t *testing.T) {
	t.Parallel()
	settings := seatMapSettings(`"claude-sonnet":{"cap":3},"claude-haiku":{"cap":1}`)
	sessions := []sessionEnv{
		{rig: "gastown", name: "ruby"},
		{rig: "gastown", name: "pearl"},
		{rig: "gastown", name: "onyx"},
	}
	records := recordedAgents(map[string]string{
		"gastown/ruby":  "claude-haiku", // restarting, session left out
		"gastown/pearl": "claude-haiku",
		"gastown/onyx":  "claude-sonnet",
		"gastown/nova":  "claude-sonnet",
	})
	cases := []struct {
		name    string
		restart string
		want    patrolscan.SeatCap
	}{
		{
			name:    "the haiku seat is full",
			restart: "ruby",
			want:    patrolscan.SeatCap{Live: 1, Limit: 1, Key: "polecat_pool.seats.claude-haiku.cap"},
		},
		{
			name:    "the sonnet seat has room while haiku is full",
			restart: "nova",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, _ := seatLiveHost(t, settings, sessions, records)
			got, err := h.seatCap("gastown", tc.restart)
			if err != nil {
				t.Fatalf("seatCap: %v", err)
			}
			if got != tc.want {
				t.Fatalf("seatCap = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// A bead that cannot be read is not a polecat with no agent: the census fails
// rather than move the polecat between seats.
func TestSeatLiveUnreadableRecord(t *testing.T) {
	t.Parallel()
	settings := seatMapSettings(threeSeats)
	h, _ := seatLiveHost(t, settings, []sessionEnv{{rig: "gastown", name: "ruby"}},
		func(rig, name string) (string, error) { return "", errors.New("bd timed out") })
	if _, _, err := h.livePolecatCensus("", seatLivePool(t, settings)); err == nil {
		t.Fatal("livePolecatCensus read an unreadable agent bead as a polecat with no agent")
	}
}

// A pool with no seat map counts GT_AGENT alone and never reads an agent bead:
// a census that did not need bd before must not fail with it.
func TestSeatLiveNoSeatMapReadsNoBead(t *testing.T) {
	t.Parallel()
	settings := `{"type":"town-settings","version":1,"polecat_pool":{"agent":"claude-sonnet","max_seats":2}}`
	h, _ := seatLiveHost(t, settings, []sessionEnv{
		{rig: "gastown", name: "ruby", agent: "claude-sonnet"},
		{rig: "gastown", name: "pearl", agent: "claude-sonnet"},
	}, func(rig, name string) (string, error) { return "", errors.New("bd timed out") })

	byAgent, total, err := h.livePolecatCensus("", seatLivePool(t, settings))
	if err != nil {
		t.Fatalf("livePolecatCensus: %v", err)
	}
	if total != 2 || byAgent["claude-sonnet"] != 2 {
		t.Fatalf("census = %v (total %d), want both on claude-sonnet", byAgent, total)
	}
}

// A restart of a polecat that records no agent launches on the role default, so
// it answers to that seat's cap: it is not let past a full seat as a polecat on
// no seat.
func TestSeatLiveUnrecordedRestartAnswersToTheRoleDefault(t *testing.T) {
	t.Parallel()
	settings := `{"type":"town-settings","version":1,"role_agents":{"polecat":"claude-haiku"},` +
		`"polecat_pool":{"seats":{"claude-sonnet":{"cap":3},"claude-haiku":{"cap":1}}}}`
	sessions := []sessionEnv{{rig: "gastown", name: "pearl"}}
	records := recordedAgents(map[string]string{"gastown/pearl": "claude-haiku"})
	h, _ := seatLiveHost(t, settings, sessions, records)

	got, err := h.seatCap("gastown", "nova") // no record: restarts on claude-haiku
	if err != nil {
		t.Fatalf("seatCap: %v", err)
	}
	want := patrolscan.SeatCap{Live: 1, Limit: 1, Key: "polecat_pool.seats.claude-haiku.cap"}
	if got != want {
		t.Fatalf("seatCap = %+v, want %+v", got, want)
	}
}
