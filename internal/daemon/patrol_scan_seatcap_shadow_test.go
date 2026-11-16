package daemon

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/patrolscan"
)

// gt-eyph71: the seat cap keeps deciding on the agent bead, and reads the
// seat's intent record beside it so the shadow window measures the hook the
// cap moves onto the record. The tests here drive the cap read, not the
// comparison helper, so a read that stopped asking for the record fails them.

const seatShadowSettings = `{"type":"town-settings","version":1,"polecat_pool":{"seats":{"deepseek-flash":{"cap":1}}}}`

// seatShadowTown is one town these tests read: a seat map, a fake store of
// agent beads, and the log the daemon writes to.
type seatShadowTown struct {
	t    *testing.T
	town string
	db   *beadsfake.Fake
	logs *bytes.Buffer
	host *patrolScanHost
}

func newSeatShadowTown(t *testing.T, settings string, sessions []sessionEnv) *seatShadowTown {
	t.Helper()
	town := t.TempDir()
	writeTownFile(t, town, "settings/config.json", settings)
	clk := newFixedClock()
	tm := newFakeTmux(clk)
	for _, s := range sessions {
		seatEnv(t, tm, s)
	}
	logs := &bytes.Buffer{}
	db := beadsfake.New()
	return &seatShadowTown{
		t:    t,
		town: town,
		db:   db,
		logs: logs,
		host: &patrolScanHost{
			d:                 &Daemon{config: &Config{TownRoot: town}, tmux: tm, clock: clk, logger: log.New(logs, "", 0)},
			openRecoveryBeads: func([]string) beads.Client { return db },
		},
	}
}

// agentBead creates the agent bead of rig/name, recording agent and hook.
func (s *seatShadowTown) agentBead(rig, name, agent, hook string) {
	s.t.Helper()
	id := beads.AgentBeadIDFor(constants.RolePolecat, rig, name, s.town)
	if id == "" {
		s.t.Fatalf("the town gives %s/%s no agent bead id", rig, name)
	}
	if _, err := s.db.Create(beads.CreateOptions{
		ID: id, Title: id, Labels: []string{"gt:agent"}, Priority: -1,
		Description: beads.FormatAgentDescription(id, &beads.AgentFields{
			RoleType: "polecat", Rig: rig, AgentState: "working", HookBead: hook, Agent: agent,
		}),
	}); err != nil {
		s.t.Fatal(err)
	}
}

// slung records the seat's intent record holding workBead, the bead a sling
// handed it.
func (s *seatShadowTown) slung(rig, name, workBead string) {
	s.t.Helper()
	seat := intent.Seat{Rig: rig, Role: constants.RolePolecat, Name: name}
	if err := intent.MarkSubmitted(s.town, seat, workBead, "test", testEpoch); err != nil {
		s.t.Fatal(err)
	}
}

// unreadableIntent leaves a malformed intent record on rig/name, the state a
// half-written file leaves behind.
func (s *seatShadowTown) unreadableIntent(rig, name string) {
	s.t.Helper()
	seat := intent.Seat{Rig: rig, Role: constants.RolePolecat, Name: name}
	path := seat.Path(s.town)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		s.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		s.t.Fatal(err)
	}
}

func (s *seatShadowTown) seatCap(rig, name string) patrolscan.SeatCap {
	s.t.Helper()
	got, err := s.host.seatCap(rig, name)
	if err != nil {
		s.t.Fatalf("seatCap(%s, %s): %v", rig, name, err)
	}
	return got
}

// A seat whose intent record and agent bead name different hooks logs the
// disagreement once, however many reads ask: patrol_scan runs every few
// minutes and the shadow window it feeds is days, so a line per read would
// bury the disagreement the overseer reads for.
func TestAgentBeadRetireSeatCapShadowLogsHookOnce(t *testing.T) {
	t.Parallel()
	s := newSeatShadowTown(t, seatShadowSettings, nil)
	s.agentBead("gastown", "ruby", "deepseek-flash", "gt-bead")
	s.slung("gastown", "ruby", "gt-intent")

	for i := 0; i < 3; i++ {
		s.seatCap("gastown", "ruby")
	}
	want := "agentbead_shadow: gastown/ruby hook intent=gt-intent bead=gt-bead"
	if got := strings.Count(s.logs.String(), want); got != 1 {
		t.Fatalf("logged the hook disagreement %d time(s) over three seat cap reads, want once:\n%s", got, s.logs.String())
	}
}

// The census reads a live seat's agent bead too, so it is shadowed as well: a
// restart of another seat asks the cap about every live polecat, and each one
// reports its own hook.
func TestAgentBeadRetireSeatCapShadowLogsCensusSeats(t *testing.T) {
	t.Parallel()
	s := newSeatShadowTown(t, `{"type":"town-settings","version":1,"polecat_pool":{"seats":{"deepseek-flash":{"cap":3}}}}`,
		[]sessionEnv{{rig: "gastown", name: "pearl", agent: "deepseek-flash"}})
	s.agentBead("gastown", "pearl", "deepseek-flash", "gt-bead")
	s.slung("gastown", "pearl", "gt-intent")

	s.seatCap("gastown", "nova") // no bead and no record: nothing to report for it
	if got, want := s.logs.String(), "agentbead_shadow: gastown/pearl hook intent=gt-intent bead=gt-bead"; strings.Count(got, want) != 1 || strings.Count(got, "\n") != 1 {
		t.Fatalf("the census did not log exactly the live seat's disagreement %q:\n%s", want, got)
	}
}

// The shadow decides nothing: the cap answers the same whichever way the
// intent record reads, including one that cannot be read at all. Only the
// line it logs changes.
func TestAgentBeadRetireSeatCapDecisionIgnoresIntent(t *testing.T) {
	t.Parallel()
	const wantKey = "polecat_pool.seats.deepseek-flash.cap"
	want := patrolscan.SeatCap{Live: 1, Limit: 1, Key: wantKey}
	cases := []struct {
		name string
		// record is what the restarting seat's intent record holds.
		record  func(s *seatShadowTown)
		wantLog bool
	}{
		{
			// A seat with no record at all is one the intent record cannot
			// vouch for: the bead alone names the hook, which is itself the
			// drift the window measures.
			name:    "no record",
			record:  func(*seatShadowTown) {},
			wantLog: true,
		},
		{
			name:   "the record agrees",
			record: func(s *seatShadowTown) { s.slung("gastown", "ruby", "gt-bead") },
		},
		{
			name:    "the record disagrees",
			record:  func(s *seatShadowTown) { s.slung("gastown", "ruby", "gt-intent") },
			wantLog: true,
		},
		{
			name:    "the record cannot be read",
			record:  func(s *seatShadowTown) { s.unreadableIntent("gastown", "ruby") },
			wantLog: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSeatShadowTown(t, seatShadowSettings,
				[]sessionEnv{{rig: "gastown", name: "pearl", agent: "deepseek-flash"}})
			s.agentBead("gastown", "pearl", "deepseek-flash", "")
			s.agentBead("gastown", "ruby", "deepseek-flash", "gt-bead")
			tc.record(s)

			if got := s.seatCap("gastown", "ruby"); got != want {
				t.Fatalf("seatCap = %+v, want %+v whichever way the intent record reads", got, want)
			}
			if gotLog := s.logs.Len() > 0; gotLog != tc.wantLog {
				t.Fatalf("logged %q, want a line: %v", s.logs.String(), tc.wantLog)
			}
		})
	}
}

// An intent record that cannot be read is reported once and decides nothing:
// the read fails closed, and a shadow that returned that failure would change
// the cap answer it is measuring.
func TestAgentBeadRetireSeatCapShadowLogsUnreadableIntentOnce(t *testing.T) {
	t.Parallel()
	s := newSeatShadowTown(t, seatShadowSettings, nil)
	s.agentBead("gastown", "ruby", "deepseek-flash", "gt-bead")
	s.unreadableIntent("gastown", "ruby")

	for i := 0; i < 3; i++ {
		if got := s.seatCap("gastown", "ruby"); got != (patrolscan.SeatCap{}) {
			t.Fatalf("seatCap = %+v, want the seat free however its intent record reads", got)
		}
	}
	if got := strings.Count(s.logs.String(), "agentbead_shadow: gastown/ruby hook unreadable intent record"); got != 1 {
		t.Fatalf("logged the unreadable intent record %d time(s) over three reads, want once:\n%s", got, s.logs.String())
	}
}
