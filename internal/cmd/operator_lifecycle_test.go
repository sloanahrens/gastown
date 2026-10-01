package cmd

import (
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/crew"
	"github.com/steveyegge/gastown/internal/estop"
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
