package daemon

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

// delayTmux answers every session as alive after advancing the fake clock by
// the delay set for the seat whose session name contains the key, and records
// the order sessions were asked about.
type delayTmux struct {
	*fakeTmux
	clk   *clockwork.FakeClock
	delay map[string]time.Duration

	mu    sync.Mutex
	asked []string
}

func (d *delayTmux) HasSession(name string) (bool, error) {
	d.mu.Lock()
	d.asked = append(d.asked, name)
	d.mu.Unlock()
	for key, delay := range d.delay {
		if strings.Contains(name, key) {
			d.clk.Advance(delay)
		}
	}
	return true, nil
}

// seatTown is a daemon over a town whose rigs hold the named polecat
// directories, all alive after the delays given.
func seatTown(t *testing.T, rigs map[string][]string, delay map[string]time.Duration) (*Daemon, *lockedBuffer, *delayTmux) {
	t.Helper()
	town := t.TempDir()
	for rig, names := range rigs {
		for _, n := range names {
			if err := os.MkdirAll(filepath.Join(town, rig, "polecats", n), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	clk := clockwork.NewFakeClockAt(time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC))
	logs := &lockedBuffer{}
	tm := &delayTmux{fakeTmux: newFakeTmux(clk), clk: clk, delay: delay}
	d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(logs, "", 0), clock: clk, tmux: tm}
	return d, logs, tm
}

// A seat whose check takes a second or more logs a line of its own with the
// tmux and bd split; fast seats log nothing (gt-2a5z6).
func TestPolecatHealthLogsSlowSeats(t *testing.T) {
	t.Parallel()
	d, logs, _ := seatTown(t, map[string][]string{"myr": {"amber", "birch", "cedar"}},
		map[string]time.Duration{"amber": 3 * time.Second, "birch": 100 * time.Millisecond})

	d.checkRigPolecatHealth("myr", &seatTimes{})

	got := logs.String()
	if want := "polecat-health: myr/amber took 3s (tmux 3s in 1 calls, bd 0s in 0 calls)"; !strings.Contains(got, want) {
		t.Errorf("%s: log lacks %q:\n%s", t.Name(), want, got)
	}
	for _, fast := range []string{"myr/birch", "myr/cedar"} {
		if strings.Contains(got, fast) {
			t.Errorf("%s: fast seat %s logged a line:\n%s", t.Name(), fast, got)
		}
	}
}

// The summary names the seat and rig counts and the three slowest seats,
// longest first.
func TestPolecatHealthSummaryNamesTheSlowestThree(t *testing.T) {
	t.Parallel()
	d, _, _ := seatTown(t, map[string][]string{"rigA": {"p1", "p2", "p3"}, "rigB": {"p4", "p5"}},
		map[string]time.Duration{"p1": 4 * time.Second, "p2": time.Second, "p3": 6 * time.Second, "p4": 3 * time.Second, "p5": 9 * time.Second})
	times := &seatTimes{}

	d.checkRigPolecatHealth("rigA", times)
	d.checkRigPolecatHealth("rigB", times)

	want := "polecat-health: 5 seats in 2 rigs; slowest rigB/p5 9s, rigA/p3 6s, rigA/p1 4s"
	if got := times.summary(); got != want {
		t.Errorf("%s: summary = %q, want %q", t.Name(), got, want)
	}
}

// The heartbeat step logs the summary once the rigs have run on the rig pool.
func TestPolecatHealthStepLogsTheSummary(t *testing.T) {
	t.Parallel()
	d, logs, _ := seatTown(t, map[string][]string{"myr": {"amber", "birch"}}, map[string]time.Duration{"amber": 2 * time.Second})
	writeJSONFile(t, d.config.TownRoot+"/mayor/rigs.json", map[string]any{"version": 1, "rigs": map[string]any{"myr": map[string]any{}}})
	d.invalidateKnownRigsCache()
	d.ctx = t.Context()
	d.rigPool = newRigWorkerPool(2, 10*time.Second, nil)

	d.checkPolecatSessionHealth()

	if want := "polecat-health: 2 seats in 1 rigs; slowest myr/amber 2s"; !strings.Contains(logs.String(), want) {
		t.Errorf("%s: log lacks %q:\n%s", t.Name(), want, logs)
	}
}

// Timing adds no check and no log for fast seats: the seats are asked about in
// directory order and nothing is logged.
func TestPolecatHealthTimingChangesNothingElse(t *testing.T) {
	t.Parallel()
	d, logs, tm := seatTown(t, map[string][]string{"myr": {"amber", "birch", "cedar"}}, nil)

	d.checkRigPolecatHealth("myr", &seatTimes{})

	if len(tm.asked) != 3 || !strings.Contains(tm.asked[0], "amber") || !strings.Contains(tm.asked[1], "birch") || !strings.Contains(tm.asked[2], "cedar") {
		t.Errorf("%s: sessions asked in order %v, want amber, birch, cedar", t.Name(), tm.asked)
	}
	if got := logs.String(); got != "" {
		t.Errorf("%s: fast seats logged %q, want nothing", t.Name(), got)
	}
}
