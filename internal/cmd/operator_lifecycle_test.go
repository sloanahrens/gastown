package cmd

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/crew"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/mayor"
	"github.com/steveyegge/gastown/internal/patrolscan"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/supervisor"
)

// opTmux is a tmux for gt down's stops and the supervisor's kills.
type opTmux struct {
	mu       sync.Mutex
	sessions map[string]bool
	keys     []string
	killed   []string
}

func newOpTmux(sessions ...string) *opTmux {
	o := &opTmux{sessions: map[string]bool{}}
	for _, s := range sessions {
		o.sessions[s] = true
	}
	return o
}

func (o *opTmux) HasSession(name string) (bool, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.sessions[name], nil
}

func (o *opTmux) SendKeysRaw(session, keys string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.keys = append(o.keys, session+":"+keys)
	return nil
}

func (o *opTmux) KillSessionWithProcesses(name string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.killed = append(o.killed, name)
	delete(o.sessions, name)
	return nil
}

func opSupervisor(town string, k supervisor.Killer) *supervisor.Supervisor {
	return supervisor.New(supervisor.Options{TownRoot: town, Tmux: k, Prefixes: cmdTestRegistry(), Logf: func(string, ...any) {}})
}

func actionLog(t *testing.T, town string) string {
	t.Helper()
	data, err := os.ReadFile(supervisor.ActionLogPath(town))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// gt down is a forced operator stop: under a town e-stop it still kills
// crew, polecat and Mayor sessions, each logged as verb stop with the actor.
func TestDownStopsThroughEstopAndLogs(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := estop.Activate(town, estop.TriggerManual, "x"); err != nil {
		t.Fatal(err)
	}
	tm := newOpTmux("gt-crew-sloan", "gt-flint", session.MayorSessionName())
	stop := downStop{tmux: tm, sup: opSupervisor(town, tm), townRoot: town, actor: "gt down/overseer", force: true}

	if err := stop.session("gt-crew-sloan"); err != nil {
		t.Fatalf("crew stop = %v", err)
	}
	if err := stop.kill("gt-flint"); err != nil {
		t.Fatalf("polecat stop = %v", err)
	}
	if was, err := stop.townSession(session.TownSessions()[0]); !was || err != nil {
		t.Fatalf("mayor stop = %v, %v", was, err)
	}
	if err := stop.session("gt-gone"); err != nil {
		t.Fatalf("absent session = %v, want nil", err)
	}
	if got, want := strings.Join(tm.killed, ","), "gt-crew-sloan,gt-flint,"+session.MayorSessionName(); got != want {
		t.Errorf("killed %s, want %s", got, want)
	}
	if len(tm.keys) != 0 {
		t.Errorf("forced down sent keys %v", tm.keys)
	}
	log := actionLog(t, town)
	if n := strings.Count(log, `"verb":"stop"`); n != 3 {
		t.Errorf("action log has %d stop lines, want 3:\n%s", n, log)
	}
	if !strings.Contains(log, `"actor":"gt down/overseer"`) || strings.Contains(log, "refused") {
		t.Errorf("action log lacks the actor or refused a stop:\n%s", log)
	}
}

// Operator stop verbs (crew stop, crew remove --force, crew rename, rig
// remove --force) use StopSession: an e-stopped rig still lets them kill.
func TestOperatorStopVerbsIgnoreRigEstop(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := estop.ActivateRig(town, "gastown", estop.TriggerManual, "x"); err != nil {
		t.Fatal(err)
	}
	tm := newOpTmux("gt-crew-max", "gt-witness")
	sup := opSupervisor(town, tm)
	for _, verb := range []string{"gt crew stop", "gt crew remove", "gt crew rename", "gt rig remove"} {
		tm.sessions["gt-crew-max"] = true
		if err := sup.StopSession("gt-crew-max", verb, operatorActorFor(verb, "overseer")); err != nil {
			t.Errorf("%s: StopSession = %v", verb, err)
		}
	}
	if len(tm.killed) != 4 {
		t.Fatalf("killed %v, want four stops", tm.killed)
	}
}

// Crew respawns (restart, refresh, start over a running or dead session,
// crew at reviving a runtime) go through supervisor.Respawn: an e-stop or a
// parked seat refuses them before anything is killed.
func TestCrewRespawnRefusedByEstop(t *testing.T) {
	t.Parallel()
	for _, scope := range []string{"town", "rig"} {
		town := t.TempDir()
		var err error
		if scope == "town" {
			err = estop.Activate(town, estop.TriggerManual, "x")
		} else {
			err = estop.ActivateRig(town, "gastown", estop.TriggerManual, "x")
		}
		if err != nil {
			t.Fatal(err)
		}
		tm := newOpTmux("gt-crew-max")
		m := &crew.Manager{}
		superviseCrewRespawns(m, opSupervisor(town, tm), cmdTestRegistry(), "gastown", "gt crew/overseer")
		for _, reason := range []string{"crew restart: replace the running session", "crew refresh: replace the running session", "crew start: replace a session whose agent exited", "crew at: runtime exited"} {
			ran := false
			err := m.Respawn("max", reason, func() error { ran = true; return nil })
			if !errors.Is(err, supervisor.ErrEstop) || ran {
				t.Errorf("%s e-stop, %s: err=%v ran=%v, want ErrEstop and nothing run", scope, reason, err, ran)
			}
		}
		if len(tm.killed) != 0 {
			t.Errorf("%s e-stop: killed %v", scope, tm.killed)
		}
	}
}

// Without an e-stop a crew respawn runs and is logged under the crew seat.
func TestCrewRespawnRunsAndLogs(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	tm := newOpTmux("gt-crew-max")
	m := &crew.Manager{}
	superviseCrewRespawns(m, opSupervisor(town, tm), cmdTestRegistry(), "gastown", "gt crew/overseer")
	ran := false
	if err := m.Respawn("max", "crew restart: replace the running session", func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("Respawn = %v ran=%v", err, ran)
	}
	log := actionLog(t, town)
	if !strings.Contains(log, `"verb":"respawn"`) || !strings.Contains(log, `"session":"gt-crew-max"`) || !strings.Contains(log, `"actor":"gt crew/overseer"`) {
		t.Errorf("action log = %s", log)
	}
}

// estopTown returns a town under a town-wide e-stop.
func estopTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	if err := estop.Activate(town, estop.TriggerManual, "x"); err != nil {
		t.Fatal(err)
	}
	return town
}

// Slice 2 (gt-4k3fj.4.1.1): a polecat SessionManager's hooks. Under an
// e-stop, Start's replacement of a dead session (Respawn) is refused before
// anything runs, while the operator stop (gt session stop, gt polecat nuke)
// and a failed startup's Cleanup still kill, each logged with the actor.
func TestPolecatSessionHooksUnderEstop(t *testing.T) {
	t.Parallel()
	town := estopTown(t)
	reg := cmdTestRegistry()
	toast := supervisor.SeatIn(reg, "gastown", "polecat", "Toast")
	sess := toast.SessionName()
	tm := newOpTmux(sess)
	h := polecatSessionHooks(opSupervisor(town, tm), reg, "gastown", "session stop", "gt session stop/overseer")

	ran := false
	if err := h.Respawn("Toast", "polecat start: replace a session whose agent exited", func() error { ran = true; return nil }); !errors.Is(err, supervisor.ErrEstop) || ran {
		t.Fatalf("Respawn under e-stop: err=%v ran=%v, want ErrEstop and nothing run", err, ran)
	}
	if len(tm.killed) != 0 {
		t.Fatalf("refused respawn killed %v", tm.killed)
	}
	if err := h.Cleanup("Toast", "polecat start: startup blocked"); err != nil {
		t.Fatalf("Cleanup under e-stop = %v", err)
	}
	tm.sessions[sess] = true
	if err := h.Stop(sess); err != nil {
		t.Fatalf("Stop under e-stop = %v", err)
	}
	if got := strings.Join(tm.killed, ","); got != sess+","+sess {
		t.Errorf("killed %s, want the cleanup and the stop", got)
	}
	log := actionLog(t, town)
	for _, want := range []string{`"verb":"respawn"`, `"verb":"cleanup"`, `"verb":"stop"`, `"actor":"gt session stop/overseer"`} {
		if !strings.Contains(log, want) {
			t.Errorf("action log lacks %s:\n%s", want, log)
		}
	}
}

// The polecat manager's housekeeping kills (reallocated name, reuse, repair,
// orphan) are Cleanup: not refused by an e-stop, logged.
func TestPolecatCleanupIgnoresEstop(t *testing.T) {
	t.Parallel()
	town := estopTown(t)
	reg := cmdTestRegistry()
	tm := newOpTmux()
	cleanup := polecatCleanup(opSupervisor(town, tm), reg, "gastown", "gt sling/overseer")
	if err := cleanup("nux", "polecat reuse: clear the existing session"); err != nil {
		t.Fatalf("cleanup under e-stop = %v", err)
	}
	nux := supervisor.SeatIn(reg, "gastown", "polecat", "nux")
	if want := nux.SessionName(); len(tm.killed) != 1 || tm.killed[0] != want {
		t.Fatalf("killed %v, want [%s]", tm.killed, want)
	}
	if log := actionLog(t, town); !strings.Contains(log, `"verb":"cleanup"`) || !strings.Contains(log, `"actor":"gt sling/overseer"`) {
		t.Errorf("action log = %s", log)
	}
}

// gt session restart over a running session is a Respawn: an e-stop refuses
// it before the stop. A restart with nothing running is a plain start, and
// the daemon's own restart executor (already guarded by supervisor.Restart)
// is not refused a second time.
func TestSessionRestartRespawnRefusedByEstop(t *testing.T) {
	t.Parallel()
	town := estopTown(t)
	reg := cmdTestRegistry()
	seat := sessionSeat{Rig: "gastown", Name: "Toast", Mgr: polecat.NewSessionManager(nil, &rig.Rig{Name: "gastown"}, reg)}
	sup := opSupervisor(town, newOpTmux())

	ran := false
	restart := func() error { ran = true; return nil }
	if err := superviseSessionRestartWith(sup, reg, seat, true, "", "gt session restart/overseer", restart); !errors.Is(err, supervisor.ErrEstop) || ran {
		t.Fatalf("running restart under e-stop: err=%v ran=%v, want ErrEstop and nothing run", err, ran)
	}
	if err := superviseSessionRestartWith(sup, reg, seat, false, "", "gt session restart/overseer", restart); err != nil || !ran {
		t.Fatalf("restart with nothing running: err=%v ran=%v, want a plain start", err, ran)
	}
	ran = false
	if err := superviseSessionRestartWith(sup, reg, seat, true, patrolscan.Actor, "gt session restart/"+patrolscan.Actor, restart); err != nil || !ran {
		t.Fatalf("daemon executor restart: err=%v ran=%v, want it run (already supervised)", err, ran)
	}
}

// The Mayor: gt mayor stop is an operator Stop (not refused), a start over a
// dead session and gt mayor restart over a running one are Respawns (refused
// by an e-stop), and gt mayor attach reviving an exited runtime is too.
func TestMayorVerbsUnderEstop(t *testing.T) {
	t.Parallel()
	town := estopTown(t)
	tm := newOpTmux(mayorSeat.SessionName())
	sup := opSupervisor(town, tm)

	mgr := mayor.NewManager(town)
	superviseMayor(mgr, sup, "gt mayor/overseer")
	ran := false
	if err := mgr.Respawn("mayor start: replace a session whose agent exited", func() error { ran = true; return nil }); !errors.Is(err, supervisor.ErrEstop) || ran {
		t.Fatalf("mayor start respawn under e-stop: err=%v ran=%v", err, ran)
	}

	stopper := &fakeMayorStopper{running: true}
	if err := restartMayor(stopper, sup, "gt mayor restart/overseer", func() error { ran = true; return nil }); !errors.Is(err, supervisor.ErrEstop) || ran || stopper.stopped {
		t.Fatalf("mayor restart under e-stop: err=%v ran=%v stopped=%v", err, ran, stopper.stopped)
	}
	stopper.running = false
	if err := restartMayor(stopper, sup, "gt mayor restart/overseer", func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("mayor restart with nothing running: err=%v ran=%v, want a plain start", err, ran)
	}

	f := &fakeMayorTmux{}
	if err := restartMayorRuntimeIfDead(f, mayorSeat.SessionName(), town, sup, "gt mayor attach/overseer"); !errors.Is(err, supervisor.ErrEstop) || len(f.calls) != 0 {
		t.Fatalf("mayor attach revive under e-stop: err=%v calls=%v", err, f.calls)
	}

	if err := mgr.StopKill(mayorSeat.SessionName()); err != nil {
		t.Fatalf("mayor stop under e-stop = %v", err)
	}
	if len(tm.killed) != 1 || tm.killed[0] != mayorSeat.SessionName() {
		t.Fatalf("killed %v, want only the stop", tm.killed)
	}
	if log := actionLog(t, town); !strings.Contains(log, `"verb":"stop"`) || !strings.Contains(log, `"seat":"mayor"`) {
		t.Errorf("action log = %s", log)
	}
}

type fakeMayorStopper struct {
	running, stopped bool
}

func (f *fakeMayorStopper) IsRunning() (bool, error) { return f.running, nil }
func (f *fakeMayorStopper) Stop() error {
	f.stopped = true
	return nil
}

// gt handoff and gt mol step done cycle a session through respawnSession: an
// e-stop refuses the cycle before run, so the session keeps running.
func TestHandoffRespawnRefusedByEstop(t *testing.T) {
	t.Parallel()
	for _, scope := range []string{"town", "rig"} {
		town := t.TempDir()
		var err error
		if scope == "town" {
			err = estop.Activate(town, estop.TriggerManual, "x")
		} else {
			err = estop.ActivateRig(town, "gastown", estop.TriggerManual, "x")
		}
		if err != nil {
			t.Fatal(err)
		}
		sup := opSupervisor(town, newOpTmux())
		for _, reason := range []string{"handoff", "handoff --cycle", "handoff (remote)", "molecule step: respawn for the next step"} {
			ran := false
			err := respawnSession(sup, cmdTestRegistry(), "gt-crew-max", reason, "gt handoff/overseer", func() error { ran = true; return nil })
			if !errors.Is(err, supervisor.ErrEstop) || ran {
				t.Errorf("%s e-stop, %s: err=%v ran=%v, want ErrEstop and nothing run", scope, reason, err, ran)
			}
		}
	}
}

// Without an e-stop the cycle runs and is logged as a respawn before run.
func TestHandoffRespawnRunsAndLogs(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	sup := opSupervisor(town, newOpTmux())
	ran := false
	if err := respawnSession(sup, cmdTestRegistry(), "gt-crew-max", "handoff", "gt handoff/overseer", func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("respawnSession = %v ran=%v", err, ran)
	}
	if log := actionLog(t, town); !strings.Contains(log, `"verb":"respawn"`) || !strings.Contains(log, `"outcome":"started"`) || !strings.Contains(log, `"actor":"gt handoff/overseer"`) {
		t.Errorf("action log = %s", log)
	}
}
