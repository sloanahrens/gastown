package cmd

import (
	"strings"
	"testing"
)

// specTickDaemon is a patrol config whose spec_dispatch block names no seat: the
// towns below are about the seat keys a tick reads, so they start with the
// second seat closed and the pool seat the only one the dispatcher runs.
const specTickDaemon = `{
  "type": "daemon-patrol-config",
  "version": 1,
  "patrols": {"spec_dispatch": {"enabled": true, "interval": "60s"}}
}`

// specTickRoster is the roster the next tick of root runs with: the seat blocks
// come from the tick's own read path, and the seats are counted by the same
// budget builder, so this is the picture a tick would take.
func specTickRoster(t *testing.T, root string) string {
	t.Helper()
	ts, sd, err := specTickTown(root)
	if err != nil {
		t.Fatal(err)
	}
	budget := specBudgetFromConfig(ts, sd)
	f := newFakeSpecTown()
	f.seats = budget.Seats
	f.maxTotal = budget.MaxTotal
	return runSpecDispatchCycle(f.env()).Roster
}

// A seat the file gains or moves is the seat the next tick runs with: the tick
// reads both blocks on every run, so a seat change needs no daemon restart
// (gt-jdnab).
func TestSpecDispatchSeatsFollowTheFileOnTheNextTick(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolSecondSettings, specTickDaemon)

	before := specTickRoster(t, root)
	if !strings.Contains(before, "claude-sonnet 0/2") {
		t.Fatalf("roster %q does not name the pool seat the file opens", before)
	}
	if strings.Contains(before, "deepseek-flash") {
		t.Fatalf("roster %q names a second seat the file does not open", before)
	}

	// The operator opens the second seat on deepseek-flash.
	if _, err := poolTestSetSecond(root, "deepseek-flash", 2); err != nil {
		t.Fatal(err)
	}
	if got := specTickRoster(t, root); !strings.Contains(got, "deepseek-flash 0/2") {
		t.Errorf("roster after the second seat opened = %q, want the deepseek-flash seat", got)
	}

	// And moves its cap: the next tick runs the mix the file holds now.
	if _, err := poolTestSetSecond(root, "deepseek-flash", 3); err != nil {
		t.Fatal(err)
	}
	if got := specTickRoster(t, root); !strings.Contains(got, "deepseek-flash 0/3") {
		t.Errorf("roster after the cap moved = %q, want the deepseek-flash seat at 3", got)
	}

	// And its agent, which is the key gt pool set cannot move: hooked_agent.
	if _, err := poolTestSetSecond(root, "groq-compound", 3); err != nil {
		t.Fatal(err)
	}
	got := specTickRoster(t, root)
	if !strings.Contains(got, "groq-compound 0/3") {
		t.Errorf("roster after the agent moved = %q, want the groq-compound seat", got)
	}
	if strings.Contains(got, "deepseek-flash") {
		t.Errorf("roster %q still names the agent the second seat left", got)
	}
}

// The tick's read tells an absent patrol block from one that does not parse: a
// block that does not parse stops the tick, where the read that returns nil for
// both would tick with the second seat silently dropped (gt-jdnab).
func TestReadSpecDispatchConfigTellsAbsentFromBroken(t *testing.T) {
	t.Parallel()

	root := poolTestTown(t, poolSecondSettings, "")
	sd, err := readSpecDispatchConfig(root)
	if sd != nil || err != nil {
		t.Errorf("an absent block = %+v, %v, want no block and no error", sd, err)
	}

	root = poolTestTown(t, poolSecondSettings, specTickBrokenDaemon)
	sd, err = readSpecDispatchConfig(root)
	if err == nil || sd != nil {
		t.Fatalf("a block that does not parse = %+v, %v, want an error", sd, err)
	}
	if !strings.Contains(err.Error(), "daemon.json") {
		t.Errorf("error %q does not name the file that does not parse", err)
	}

	root = poolTestTown(t, poolSecondSettings, specTickDaemon)
	sd, err = readSpecDispatchConfig(root)
	if err != nil || sd == nil || (sd.Enabled == nil || !*sd.Enabled) {
		t.Errorf("a readable block = %+v, %v, want the block the file holds", sd, err)
	}
}

// specTickBrokenDaemon is a patrol document cut off mid-key: the tick's read
// errors on it rather than reading it as a town with no second seat.
const specTickBrokenDaemon = `{"type": "daemon-patrol-config", "version": 1, "patrols": {"spec_dispatch": {"enabled": true, "max_hooked":`

// A seat file the tick cannot read stops the tick: the tick returns the error
// instead of slinging on a picture that lost the second seat, and the error
// names the seat settings so the operator knows what to fix (gt-jdnab).
func TestSpecDispatchStopsOnASeatFileItCannotRead(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolSecondSettings, specTickBrokenDaemon)
	report, err := runSpecDispatchAt(root)
	if err == nil {
		t.Fatalf("the tick ran with a seat file that does not parse: %+v", report)
	}
	if !strings.Contains(err.Error(), "reading the seat settings") || !strings.Contains(err.Error(), "daemon.json") {
		t.Errorf("tick error = %q, want it to name the unreadable seat settings", err)
	}
}

// The tick reads the same file gt pool reads and writes: both go through
// readSpecDispatchConfig, so a seat set-second wrote is a seat the tick sees
// (gt-jdnab).
func TestSpecTickAndPoolReadTheOnePatrolBlock(t *testing.T) {
	t.Parallel()
	root := poolTestTown(t, poolSecondSettings, specTickDaemon)
	if _, err := poolTestSetSecond(root, "groq-compound", 3); err != nil {
		t.Fatal(err)
	}
	sd, err := loadPoolSpecDispatch(root)
	if err != nil {
		t.Fatal(err)
	}
	if sd == nil || sd.HookedAgent != "groq-compound" || sd.MaxHooked != 3 {
		t.Errorf("gt pool reads %+v, want the seat gt pool set-second wrote", sd)
	}
	_, ticked, err := specTickTown(root)
	if err != nil {
		t.Fatal(err)
	}
	if ticked == nil || ticked.HookedAgent != "groq-compound" || ticked.MaxHooked != 3 {
		t.Errorf("the tick reads %+v, want the seat gt pool set-second wrote", ticked)
	}
}
