package cmd

import (
	"strings"
	"testing"
)

// specTickSettings is a town whose seat map holds one seat, on claude-sonnet,
// with the town's total beside it. deepseek-flash is defined as a custom agent,
// the way the real town does, so a seat can be opened on it.
const specTickSettings = `{
  "type": "town-settings",
  "version": 1,
  "agents": {"claude-sonnet": {"command": "claude"}, "deepseek-flash": {"command": "claude"}},
  "polecat_pool": {"seats": {"claude-sonnet": {"cap": 2}}, "max_total": 4}
}
`

// specTickDaemon is a patrol config whose spec_dispatch block names no knob: the
// towns below are about the seat map a tick reads, which lives in the town
// settings, so the patrol block only has to be readable.
const specTickDaemon = `{
  "type": "daemon-patrol-config",
  "version": 1,
  "patrols": {"spec_dispatch": {"enabled": true, "interval": "60s"}}
}`

// specTickRoster is the roster the next tick of root runs with: the seats come
// from the tick's own read path and budget builder, so this is the picture a
// tick would take.
func specTickRoster(t *testing.T, root string) string {
	t.Helper()
	ts, _, err := specTickTown(root)
	if err != nil {
		t.Fatal(err)
	}
	budget, err := specBudgetFor(root, ts, poolTestNow)
	if err != nil {
		t.Fatal(err)
	}
	f := newFakeSpecTown()
	f.seats = budget.Seats
	f.maxTotal = budget.MaxTotal
	return runSpecDispatchCycle(f.env()).Roster
}

// A seat the file gains, moves or loses is the seat the next tick runs with: the
// tick reads the seat map on every run, so a seat change made by gt pool set
// needs no daemon restart (gt-jdnab, gt-mukgk).
func TestSpecDispatchSeatsFollowTheFileOnTheNextTick(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, specTickSettings, specTickDaemon)

	before := specTickRoster(t, root)
	if !strings.Contains(before, "claude-sonnet 0/2") {
		t.Fatalf("roster %q does not name the seat the file opens", before)
	}
	if strings.Contains(before, "deepseek-flash") {
		t.Fatalf("roster %q names a seat the file does not open", before)
	}

	// The operator opens a seat on deepseek-flash.
	poolTestSetSeats(t, root, []poolSeatCap{{Agent: "deepseek-flash", Cap: 2}}, nil, 0, false)
	if got := specTickRoster(t, root); !strings.Contains(got, "deepseek-flash 0/2") || !strings.Contains(got, "claude-sonnet 0/2") {
		t.Errorf("roster after the seat opened = %q, want both seats", got)
	}

	// And moves its cap: the next tick runs the cap the file holds now.
	poolTestSetSeats(t, root, []poolSeatCap{{Agent: "deepseek-flash", Cap: 3}}, nil, 0, false)
	if got := specTickRoster(t, root); !strings.Contains(got, "deepseek-flash 0/3") {
		t.Errorf("roster after the cap moved = %q, want the deepseek-flash seat at 3", got)
	}

	// And swaps it for another agent: the seat it left is gone from the tick.
	poolTestSetSeats(t, root, []poolSeatCap{{Agent: "groq-compound", Cap: 3}}, []string{"deepseek-flash"}, 0, false)
	got := specTickRoster(t, root)
	if !strings.Contains(got, "groq-compound 0/3") {
		t.Errorf("roster after the agent moved = %q, want the groq-compound seat", got)
	}
	if strings.Contains(got, "deepseek-flash") {
		t.Errorf("roster %q still names the agent the seat left", got)
	}
}

// The tick's read tells an absent patrol block from one that does not parse: a
// block that does not parse stops the tick, where the read that returns nil for
// both would tick on defaults silently (gt-jdnab).
func TestReadSpecDispatchConfigTellsAbsentFromBroken(t *testing.T) {
	t.Parallel()

	root := poolTestTown(t, specTickSettings, "")
	sd, err := readSpecDispatchConfig(root)
	if sd != nil || err != nil {
		t.Errorf("an absent block = %+v, %v, want no block and no error", sd, err)
	}

	root = poolTestTown(t, specTickSettings, specTickBrokenDaemon)
	sd, err = readSpecDispatchConfig(root)
	if err == nil || sd != nil {
		t.Fatalf("a block that does not parse = %+v, %v, want an error", sd, err)
	}
	if !strings.Contains(err.Error(), "daemon.json") {
		t.Errorf("error %q does not name the file that does not parse", err)
	}

	root = poolTestTown(t, specTickSettings, specTickDaemon)
	sd, err = readSpecDispatchConfig(root)
	if err != nil || sd == nil || (sd.Enabled == nil || !*sd.Enabled) {
		t.Errorf("a readable block = %+v, %v, want the block the file holds", sd, err)
	}
}

// specTickBrokenDaemon is a patrol document cut off mid-key: the tick's read
// errors on it rather than reading it as a town with no knobs set.
const specTickBrokenDaemon = `{"type": "daemon-patrol-config", "version": 1, "patrols": {"spec_dispatch": {"enabled": true, "max_per_tick":`

// A patrol file the tick cannot read stops the tick: the tick returns the error
// instead of slinging on a picture that lost its knobs, and the error names the
// settings so the operator knows what to fix (gt-jdnab).
func TestSpecDispatchStopsOnASeatFileItCannotRead(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, specTickSettings, specTickBrokenDaemon)
	report, err := runSpecDispatchAt(root)
	if err == nil {
		t.Fatalf("the tick ran with a seat file that does not parse: %+v", report)
	}
	if !strings.Contains(err.Error(), "reading the seat settings") || !strings.Contains(err.Error(), "daemon.json") {
		t.Errorf("tick error = %q, want it to name the unreadable seat settings", err)
	}
}
