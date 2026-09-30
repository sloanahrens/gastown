package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/dog"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/supervisor"
	"github.com/steveyegge/gastown/internal/tmux"
)

// workBD is a fake bd that answers only work-bead queries: `list
// --status=<s>` prints <dir>/list-<s>.json (default []), `show <id>` prints
// <dir>/show-<id>.json (default: not found). Every call is appended to
// <dir>/calls.log, so a test can assert which reads were made.
type workBD struct {
	dir  string
	path string
}

func newWorkBD(t *testing.T) *workBD {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
echo "$*" >> "` + dir + `/calls.log"
case "$1" in
  list)
    for a in "$@"; do
      case "$a" in --status=*) s="${a#--status=}";; esac
    done
    f="` + dir + `/list-$s.json"
    if [ -f "$f" ]; then cat "$f"; else echo '[]'; fi
    ;;
  show)
    f="` + dir + `/show-$2.json"
    if [ -f "$f" ]; then cat "$f"; else echo '[]'; exit 1; fi
    ;;
  *) exit 1;;
esac
`
	path := filepath.Join(dir, "bd")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &workBD{dir: dir, path: path}
}

func (b *workBD) set(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(b.dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (b *workBD) calls(t *testing.T) string {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(b.dir, "calls.log"))
	return string(data)
}

// registerMyr maps rig "myr" to prefix "myr" for the test's duration.
func registerMyr(t *testing.T) {
	t.Helper()
	old := session.DefaultRegistry()
	reg := session.NewPrefixRegistry()
	reg.Register("myr", "myr")
	session.SetDefaultRegistry(reg)
	t.Cleanup(func() { session.SetDefaultRegistry(old) })
}

func writePolecatHeartbeat(t *testing.T, townRoot string, state polecat.HeartbeatState, age time.Duration) {
	t.Helper()
	hbPath := filepath.Join(townRoot, ".runtime", "heartbeats", "myr-mycat.json")
	_ = os.MkdirAll(filepath.Dir(hbPath), 0o755)
	data, _ := json.Marshal(polecat.SessionHeartbeat{Timestamp: time.Now().UTC().Add(-age), State: state})
	if err := os.WriteFile(hbPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func reaperDaemon(t *testing.T, bdPath string) (*Daemon, *strings.Builder) {
	t.Helper()
	var logBuf strings.Builder
	return &Daemon{
		config:   &Config{TownRoot: t.TempDir()},
		logger:   log.New(&logBuf, "", 0),
		tmux:     polecatSessionTmux("bash", time.Now().Add(-time.Hour)),
		notifier: notifyfake.New(),
		bdPath:   bdPath,
	}, &logBuf
}

// G1-08: the idle reaper honors the pause marker; before the supervisor it
// was the one scanner that did not.
func TestReapIdlePolecat_LeavesAPausedPolecatAlone(t *testing.T) {
	registerMyr(t)
	d, logBuf := reaperDaemon(t, "")
	writePolecatHeartbeat(t, d.config.TownRoot, polecat.HeartbeatIdle, time.Hour)
	if err := agentpause.Pause(d.config.TownRoot, "myr", "polecat", "mycat", "inspecting the pane", "human", ""); err != nil {
		t.Fatal(err)
	}

	d.reapIdlePolecat("myr", "mycat", 15*time.Minute)

	if alive, _ := d.tmux.HasSession("myr-mycat"); !alive {
		t.Fatalf("a paused polecat was reaped; log: %s", logBuf)
	}
	if !strings.Contains(logBuf.String(), "refused") {
		t.Fatalf("the refusal was not logged: %s", logBuf)
	}
}

// G1-07: a per-rig e-stop stops the reaper too.
func TestReapIdlePolecat_HonorsARigEstop(t *testing.T) {
	registerMyr(t)
	d, logBuf := reaperDaemon(t, "")
	writePolecatHeartbeat(t, d.config.TownRoot, polecat.HeartbeatIdle, time.Hour)
	if err := estop.ActivateRig(d.config.TownRoot, "myr", estop.TriggerManual, "drill"); err != nil {
		t.Fatal(err)
	}

	d.reapIdlePolecat("myr", "mycat", 15*time.Minute)

	if alive, _ := d.tmux.HasSession("myr-mycat"); !alive {
		t.Fatalf("reaped under a rig e-stop; log: %s", logBuf)
	}
}

// G1-01: the reaper decides from the work bead and the pane, never from an
// agent bead.
func TestReapIdlePolecat_NeverReadsAgentBeads(t *testing.T) {
	registerMyr(t)
	bd := newWorkBD(t)
	d, logBuf := reaperDaemon(t, bd.path)
	writePolecatHeartbeat(t, d.config.TownRoot, polecat.HeartbeatWorking, time.Hour)

	d.reapIdlePolecat("myr", "mycat", 15*time.Minute)

	if !strings.Contains(logBuf.String(), "Reaping idle polecat") {
		t.Fatalf("a stale, workless, agentless polecat was not reaped: %s", logBuf)
	}
	if calls := bd.calls(t); strings.Contains(calls, "show") || strings.Contains(calls, "polecat-mycat") {
		t.Fatalf("the reaper read an agent bead:\n%s", calls)
	}
	lines, _ := os.ReadFile(supervisor.ActionLogPath(d.config.TownRoot))
	if !strings.Contains(string(lines), `"actor":"daemon/idle-reaper"`) {
		t.Fatalf("the reap is not in the supervisor action log with its actor: %s", lines)
	}
}

// G1-01: crash detection finds the work from the work bead's assignee, with
// no agent-bead read.
func TestCheckPolecatHealth_CrashFromWorkBeadWithoutAgentBead(t *testing.T) {
	bd := newWorkBD(t)
	old := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	bd.set(t, "list-hooked.json", `[{"id":"gt-work1","status":"hooked","updated_at":"`+old+`"}]`)
	bd.set(t, "show-gt-work1.json", `[{"id":"gt-work1","status":"hooked"}]`)
	d, logBuf := reaperDaemon(t, bd.path)
	d.tmux = newFakeTmux(newFixedClock())

	d.checkPolecatHealth("myr", "mycat")

	if !strings.Contains(logBuf.String(), "CRASH DETECTED") || !strings.Contains(logBuf.String(), "gt-work1") {
		t.Fatalf("no crash detected from the assigned work bead: %s", logBuf)
	}
	if calls := bd.calls(t); strings.Contains(calls, "polecat-mycat") {
		t.Fatalf("crash detection read an agent bead:\n%s", calls)
	}
}

// The spawn grace comes from the work bead: sling hooks it before the
// session exists, so a recently updated hooked bead with no session is a
// polecat starting up (issue #1752), not a crash.
func TestCheckPolecatHealth_SpawnGraceFromWorkBead(t *testing.T) {
	bd := newWorkBD(t)
	recent := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	bd.set(t, "list-hooked.json", `[{"id":"gt-work1","status":"hooked","updated_at":"`+recent+`"}]`)
	d, logBuf := reaperDaemon(t, bd.path)
	d.tmux = newFakeTmux(newFixedClock())

	d.checkPolecatHealth("myr", "mycat")

	if strings.Contains(logBuf.String(), "CRASH DETECTED") {
		t.Fatalf("a polecat inside its spawn window was called crashed: %s", logBuf)
	}
}

// The intent record's work_bead, when a writer set it, is the seat's work.
func TestCheckPolecatHealth_UsesIntentWorkBead(t *testing.T) {
	bd := newWorkBD(t)
	bd.set(t, "show-gt-intended.json", `[{"id":"gt-intended","status":"in_progress"}]`)
	d, logBuf := reaperDaemon(t, bd.path)
	d.tmux = newFakeTmux(newFixedClock())
	if _, err := intent.Update(d.config.TownRoot, intent.Seat{Rig: "myr", Role: "polecat", Name: "mycat"}, func(r *intent.Record) error {
		r.WorkBead = "gt-intended"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	d.checkPolecatHealth("myr", "mycat")

	if !strings.Contains(logBuf.String(), "gt-intended") {
		t.Fatalf("crash detection ignored the intent record's work bead: %s", logBuf)
	}
}

// The patrol-disabled sweep kills through the supervisor: a paused witness
// stays, an unpaused one goes, and the kill is logged with its actor.
func TestKillWitnessSessions_HonorsPauseAndLogsActor(t *testing.T) {
	registerRigs(t, "aa", "bb")
	tm := newFakeTmux(newFixedClock())
	tm.addSession("aa-witness", "claude", time.Now())
	tm.addSession("bb-witness", "claude", time.Now())
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, logger: log.New(&strings.Builder{}, "", 0), tmux: tm, rigPool: newRigWorkerPool(1, 10*time.Second, nil), ctx: context.Background()}
	writeKnownRigs(t, d.config.TownRoot, "aa", "bb")
	if err := agentpause.Pause(d.config.TownRoot, "aa", "witness", "", "debugging", "human", ""); err != nil {
		t.Fatal(err)
	}

	d.killWitnessSessions()

	if has, _ := tm.HasSession("aa-witness"); !has {
		t.Error("a paused witness was killed by the patrol-disabled sweep")
	}
	if has, _ := tm.HasSession("bb-witness"); has {
		t.Error("an unpaused witness survived the patrol-disabled sweep")
	}
	lines, _ := os.ReadFile(supervisor.ActionLogPath(d.config.TownRoot))
	if !strings.Contains(string(lines), `"actor":"daemon/patrol-disabled"`) {
		t.Errorf("sweep kills missing from the action log: %s", lines)
	}
}

// Ghost sessions belong to no seat: they go through KillStray, which the
// town e-stop refuses.
func TestKillDefaultPrefixGhosts_HonorsTheTownEstop(t *testing.T) {
	registerRigs(t, "aa")
	tm := newFakeTmux(newFixedClock())
	tm.addSession("gt-witness", "claude", time.Now())
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, logger: log.New(&strings.Builder{}, "", 0), tmux: tm}
	_ = estop.Activate(d.config.TownRoot, estop.TriggerManual, "drill")

	d.killDefaultPrefixGhosts()

	if has, _ := tm.HasSession("gt-witness"); !has {
		t.Fatal("a ghost was killed under a town e-stop")
	}
	_ = estop.Deactivate(d.config.TownRoot, false)
	d.killDefaultPrefixGhosts()
	if has, _ := tm.HasSession("gt-witness"); has {
		t.Fatal("the ghost survived once the e-stop cleared")
	}
	lines, _ := os.ReadFile(supervisor.ActionLogPath(d.config.TownRoot))
	if !strings.Contains(string(lines), `"verb":"kill-stray"`) {
		t.Errorf("ghost kill missing from the action log: %s", lines)
	}
}

// The mayor's dead-agent debounce is persisted: three consecutive dead
// samples, even across three daemon values, before one restart. A missing
// session is restarted at once.
func TestEnsureMayorRunning_PersistedDebounce(t *testing.T) {
	town := t.TempDir()
	clk := newFixedClock()
	tm := newFakeTmux(clk)
	tm.addSession(session.MayorSessionName(), "bash", clk.Now()) // agent gone, shell left
	var restarts []string
	newD := func() (*Daemon, *strings.Builder) {
		var buf strings.Builder
		return &Daemon{config: &Config{TownRoot: town}, logger: log.New(&buf, "", 0), tmux: tm, clock: clk,
			restartSeatFn: func(seat supervisor.Seat) error { restarts = append(restarts, seat.SessionName()); return nil }}, &buf
	}
	for i := 1; i <= 2; i++ {
		d, buf := newD()
		d.ensureMayorRunning()
		if len(restarts) != 0 || !strings.Contains(buf.String(), "waiting before restart") {
			t.Fatalf("sample %d: restarts=%v log=%s", i, restarts, buf)
		}
		clk.Advance(3 * time.Minute)
	}
	d, _ := newD()
	d.ensureMayorRunning()
	if len(restarts) != 1 {
		t.Fatalf("third dead sample: restarts = %v, want one", restarts)
	}

	// No session at all: no debounce.
	_ = tm.KillSession(session.MayorSessionName())
	clk.Advance(3 * time.Minute)
	d, _ = newD()
	d.ensureMayorRunning()
	if len(restarts) != 2 {
		t.Fatalf("missing session: restarts = %v, want a second one at once", restarts)
	}
}

// A stale working dog under a town e-stop keeps its session.
func TestDetectStaleWorkingDogs_HonorsTheTownEstop(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	d := testHandlerDaemon(t, townRoot)
	mgr := dog.NewManager(townRoot, &config.RigsConfig{Version: 1, Rigs: map[string]config.RigEntry{}})
	sm := handlerDogSessions(d)
	testSetupWorkingDogState(t, townRoot, "stale", constants.MolConvoyFeed, time.Now().Add(-3*time.Hour))
	sessionName := sm.SessionName("stale")
	sm.tm.addSession(sessionName, "claude", time.Now().Add(-3*time.Hour))
	_ = estop.Activate(townRoot, estop.TriggerManual, "drill")

	d.detectStaleWorkingDogs(mgr, sm, &config.DaemonThresholds{})

	if has, _ := sm.tm.HasSession(sessionName); !has {
		t.Fatal("a dog session was killed under a town e-stop")
	}
	if dg, _ := mgr.Get("stale"); dg.State != dog.StateWorking {
		t.Fatalf("dog work cleared although its session was kept: state %q", dg.State)
	}
}

// writeKnownRigs writes mayor/rigs.json naming rigs.
func writeKnownRigs(t *testing.T, townRoot string, rigs ...string) {
	t.Helper()
	entries := make([]string, len(rigs))
	for i, r := range rigs {
		entries[i] = `"` + r + `": {}`
	}
	path := filepath.Join(townRoot, "mayor", "rigs.json")
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(`{"rigs": {`+strings.Join(entries, ", ")+`}}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

// registerRigs maps each rig to a prefix equal to its name for the test.
func registerRigs(t *testing.T, rigs ...string) {
	t.Helper()
	old := session.DefaultRegistry()
	reg := session.NewPrefixRegistry()
	for _, r := range rigs {
		reg.Register(r, r)
	}
	session.SetDefaultRegistry(reg)
	t.Cleanup(func() { session.SetDefaultRegistry(old) })
}

// unknownTmuxDaemon returns a daemon whose tmux cannot answer, whose restart
// executor records instead of starting anything, and whose town has an
// operational rig "testrig" with a pending refinery event (a fake bd on PATH
// answers the rig-bead read). Serial: it sets PATH.
func unknownTmuxDaemon(t *testing.T) (*Daemon, *strings.Builder, *[]string) {
	t.Helper()
	town := t.TempDir()
	writeDaemonTownFile(t, town, "testrig/config.json", `{"beads":{"prefix":"gt"}}`)
	binDir := t.TempDir()
	writeDaemonNoSafetyStopMockBD(t, binDir, filepath.Join(binDir, "bd.log"))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	tm := newFakeTmux(newFixedClock())
	tm.mu.Lock()
	tm.hasErr = errors.New("tmux: server timed out")
	tm.mu.Unlock()
	var buf strings.Builder
	var restarts []string
	d := &Daemon{
		config: DefaultConfig(town), logger: log.New(&buf, "", 0), tmux: tm,
		restartSeatFn: func(seat supervisor.Seat) error { restarts = append(restarts, seat.SessionName()); return nil },
	}
	return d, &buf, &restarts
}

// Unknown is never acted on (G1-09): a tmux that cannot answer starts no
// witness or mayor.
func TestEnsurePaths_UnknownStartsNothing(t *testing.T) {
	for name, ensure := range map[string]func(*Daemon){
		"witness": func(d *Daemon) { d.ensureWitnessRunning("testrig") },
		"mayor":   func(d *Daemon) { d.ensureMayorRunning() },
	} {
		t.Run(name, func(t *testing.T) {
			d, buf, restarts := unknownTmuxDaemon(t)
			ensure(d)
			if len(*restarts) != 0 {
				t.Fatalf("restarted %v on an unknown liveness answer\nlog:\n%s", *restarts, buf)
			}
			if !strings.Contains(buf.String(), "liveness unknown") {
				t.Fatalf("the unknown answer was not logged\nlog:\n%s", buf)
			}
		})
	}
}

// An unreadable intent record is an Unknown liveness answer: the Deacon is
// not started over it.
func TestEnsureDeaconRunning_UnreadableIntentStartsNothing(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := supervisor.IntentSeat(deaconSeat).Path(town)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	started := false
	d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(&buf, "", 0), tmux: newFakeTmux(newFixedClock()),
		startDeaconFn: func() error { started = true; return nil }}

	d.ensureDeaconRunning()

	if started || !strings.Contains(buf.String(), "liveness unknown") {
		t.Fatalf("started=%v with an unreadable intent record\nlog:\n%s", started, buf.String())
	}
}

// Witnesses are never restarted for a stall (serial killer bug): an idle
// witness produces no output while it waits for work.
func TestEnsureWitnessRunning_StalledIsNotRestarted(t *testing.T) {
	d, buf, restarts := unknownTmuxDaemon(t)
	tm := d.tmux.(*fakeTmux)
	tm.mu.Lock()
	tm.hasErr = nil
	tm.mu.Unlock()
	seat := supervisor.SeatFor("testrig", "witness", "")
	tm.addSession(seat.SessionName(), "claude", time.Now().Add(-3*time.Hour))
	seedStalledSample(t, d, seat, tm, 3*time.Hour)

	d.ensureWitnessRunning("testrig")

	if len(*restarts) != 0 {
		t.Fatalf("a stalled witness was restarted: %v\nlog:\n%s", *restarts, buf)
	}
	if !strings.Contains(buf.String(), "already running") {
		t.Fatalf("stalled witness not treated as running\nlog:\n%s", buf)
	}
}

// seedStalledSample records a sample for seat whose evidence has not changed
// for quiet.
func seedStalledSample(t *testing.T, d *Daemon, seat supervisor.Seat, tm *fakeTmux, quiet time.Duration) {
	t.Helper()
	pane, _ := tm.CapturePane(seat.SessionName(), 200)
	created, _ := tm.GetSessionCreatedTime(seat.SessionName())
	now := d.clk().Now()
	if _, err := intent.Update(d.config.TownRoot, supervisor.IntentSeat(seat), func(r *intent.Record) error {
		r.Progress = &intent.Progress{SessionCreated: created, PaneHash: tmux.PaneProgressSignature(pane, tmux.DefaultReadyPromptPrefix),
			SampledAt: now.Add(-time.Minute), ChangedAt: now.Add(-quiet)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A seat the daemon cannot start (a polecat: the witness restarts those)
// declines the restart and spends no budget.
func TestRestartSeat_NoStarterDeclinesWithoutSpendingBudget(t *testing.T) {
	t.Parallel()
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, logger: log.New(&strings.Builder{}, "", 0), tmux: newFakeTmux(newFixedClock())}
	seat := supervisor.SeatFor("myr", "polecat", "mycat")
	for i := 0; i < 4; i++ {
		if err := d.sup().Restart(seat, "dead", "daemon"); !errors.Is(err, supervisor.ErrDeclined) {
			t.Fatalf("attempt %d = %v, want ErrDeclined", i+1, err)
		}
	}
	if rec, _ := intent.Read(d.config.TownRoot, supervisor.IntentSeat(seat)); rec.Frozen || len(rec.Restarts) != 0 {
		t.Fatalf("record after declined restarts = %+v, want no budget spent", rec)
	}
}
