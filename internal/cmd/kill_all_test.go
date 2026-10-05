package cmd

import (
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/supervisor"
)

// killAllTmux records the sessions a supervisor kills.
type killAllTmux struct {
	mu     sync.Mutex
	killed []string
}

func (k *killAllTmux) KillSessionWithProcesses(name string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.killed = append(k.killed, name)
	return nil
}

var killAllSessions = []string{"hq-overseer", "hq-mayor", "gt-crew-sloan", "gt-flint", "do-toast", "random-xyz"}

func killAllHarness(t *testing.T) (string, *killAllTmux, *supervisor.Supervisor) {
	t.Helper()
	town := t.TempDir()
	k := &killAllTmux{}
	sup := supervisor.New(supervisor.Options{TownRoot: town, Tmux: k, Prefixes: cmdTestRegistry(), Logf: func(string, ...any) {}})
	return town, k, sup
}

func TestKillAllWithoutYesOnlyPrintsThePlan(t *testing.T) {
	t.Parallel()
	town, k, sup := killAllHarness(t)
	var out strings.Builder
	req := killAllRequest{TownRoot: town, Actor: "gt kill-all/overseer"}
	if err := killAll(&out, req, cmdTestRegistry(), killAllSessions, sup); err != nil {
		t.Fatalf("killAll: %v", err)
	}
	if len(k.killed) != 0 {
		t.Fatalf("killed %v without --yes", k.killed)
	}
	if _, err := os.Stat(estop.FilePath(town)); !os.IsNotExist(err) {
		t.Fatalf("plan wrote the ESTOP sentinel (stat err %v)", err)
	}
	for _, want := range []string{"would kill gt-flint", "hq-overseer: overseer", "gt-crew-sloan: crew", "random-xyz: not a known seat", "--yes"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plan output lacks %q:\n%s", want, out.String())
		}
	}
}

// The town kill-all sets the town E-stop and kills every agent seat, crew
// with --crew, through a parked seat, never the overseer.
func TestKillAllTownSetsEstopAndKillsThroughTheSupervisor(t *testing.T) {
	t.Parallel()
	town, k, sup := killAllHarness(t)
	flint := supervisor.SeatIn(cmdTestRegistry(), "gastown", "polecat", "flint")
	if _, err := intent.Update(town, supervisor.IntentSeat(flint), func(r *intent.Record) error {
		r.Desired = intent.DesiredPark
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	req := killAllRequest{TownRoot: town, Reason: "runaway", Actor: "gt kill-all/overseer", Crew: true, Yes: true}
	if err := killAll(&out, req, cmdTestRegistry(), killAllSessions, sup); err != nil {
		t.Fatalf("killAll: %v\n%s", err, out.String())
	}
	// The hq-mayor fixture session is a leftover: its role retired, so it no
	// longer names a seat and the kill-all passes over it (gt-rwp7z).
	if got, want := strings.Join(k.killed, ","), "gt-crew-sloan,gt-flint,do-toast"; got != want {
		t.Errorf("killed %s, want %s", got, want)
	}
	info := estop.Read(town)
	if info == nil || info.Reason != "runaway" {
		t.Fatalf("town ESTOP = %+v, want one with the reason", info)
	}
	data, err := os.ReadFile(supervisor.ActionLogPath(town))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(data), `"verb":"kill-all"`); n != 3 {
		t.Errorf("action log has %d kill-all lines, want 3:\n%s", n, data)
	}
	if !strings.Contains(string(data), `"actor":"gt kill-all/overseer"`) || !strings.Contains(string(data), "kill-all: runaway") {
		t.Errorf("action log lacks actor or reason:\n%s", data)
	}
}

// --rig kills only that rig's agent seats and sets only that rig's E-stop.
func TestKillAllRigScoped(t *testing.T) {
	t.Parallel()
	town, k, sup := killAllHarness(t)
	var out strings.Builder
	req := killAllRequest{TownRoot: town, Rig: "gastown", Actor: "gt kill-all/overseer", Yes: true}
	if err := killAll(&out, req, cmdTestRegistry(), killAllSessions, sup); err != nil {
		t.Fatalf("killAll: %v", err)
	}
	if got := strings.Join(k.killed, ","); got != "gt-flint" {
		t.Errorf("killed %s, want gt-flint only", got)
	}
	if !estop.IsRigActive(town, "gastown") || estop.IsActive(town) {
		t.Errorf("want ESTOP.gastown only; town=%v rig=%v", estop.IsActive(town), estop.IsRigActive(town, "gastown"))
	}
}

func TestActivateEstopWritesOnlyTheSentinel(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	var out strings.Builder
	if err := activateEstop(&out, town, "", "closing laptop"); err != nil {
		t.Fatal(err)
	}
	if info := estop.Read(town); info == nil || info.Reason != "closing laptop" {
		t.Fatalf("ESTOP = %+v", info)
	}
	for _, want := range []string{"No new dispatch", "running sessions finish", "gt kill-all", "gt thaw"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}
