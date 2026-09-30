package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/supervisor"
	"github.com/steveyegge/gastown/internal/tmux"
)

// workBD is an in-process bd that answers only work-bead queries: `list
// --status=<s>` prints the content set as "list-<s>.json" (default []),
// `show <id>` prints "show-<id>.json" (default: [] and exit 1, not found).
// Anything else exits 1. Every call is recorded, so a test can assert which
// reads were made. Wire it with d.execCmd = bd.run.
type workBD struct {
	*fakeCLI

	mu    sync.Mutex
	files map[string]string
}

func newWorkBD(t *testing.T) *workBD {
	t.Helper()
	b := &workBD{files: map[string]string{}}
	b.fakeCLI = newFakeCLI(b.answer)
	return b
}

func (b *workBD) answer(args []string) cliReply {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(args) == 0 {
		return cliReply{code: 1}
	}
	switch args[0] {
	case "list":
		s := ""
		for _, a := range args {
			if v, ok := strings.CutPrefix(a, "--status="); ok {
				s = v
			}
		}
		if out, ok := b.files["list-"+s+".json"]; ok {
			return cliReply{stdout: out}
		}
		return cliReply{stdout: "[]\n"}
	case "show":
		if len(args) > 1 {
			if out, ok := b.files["show-"+args[1]+".json"]; ok {
				return cliReply{stdout: out}
			}
		}
		return cliReply{stdout: "[]\n", code: 1}
	}
	return cliReply{code: 1}
}

func (b *workBD) set(t *testing.T, name, content string) {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	b.files[name] = content
}

// calls returns every recorded argv, one space-joined call per line.
func (b *workBD) calls(t *testing.T) string {
	t.Helper()
	var sb strings.Builder
	for _, c := range b.recorded() {
		sb.WriteString(strings.Join(c.args, " ") + "\n")
	}
	return sb.String()
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

// reaperDaemon returns a daemon over a stale bash polecat session whose bd
// calls bd answers. A nil bd leaves the daemon without a bd at all.
func reaperDaemon(t *testing.T, bd *workBD) (*Daemon, *strings.Builder) {
	t.Helper()
	var logBuf strings.Builder
	d := &Daemon{
		config:   &Config{TownRoot: t.TempDir()},
		logger:   log.New(&logBuf, "", 0),
		tmux:     polecatSessionTmux("bash", time.Now().Add(-time.Hour)),
		notifier: notifyfake.New(),
		// The reaper names the seat's session from the registry.
		prefixRegistryFn: myrPrefixes,
	}
	if bd != nil {
		d.bdPath = "bd"
		d.execCmd = bd.run
	}
	return d, &logBuf
}

// G1-08: the idle reaper honors the pause marker; before the supervisor it
// was the one scanner that did not.
func TestReapIdlePolecat_LeavesAPausedPolecatAlone(t *testing.T) {
	t.Parallel()
	d, logBuf := reaperDaemon(t, nil)
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
	t.Parallel()
	d, logBuf := reaperDaemon(t, nil)
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
	t.Parallel()
	bd := newWorkBD(t)
	d, logBuf := reaperDaemon(t, bd)
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
	t.Parallel()
	bd := newWorkBD(t)
	old := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	bd.set(t, "list-hooked.json", `[{"id":"gt-work1","status":"hooked","updated_at":"`+old+`"}]`)
	bd.set(t, "show-gt-work1.json", `[{"id":"gt-work1","status":"hooked"}]`)
	d, logBuf := reaperDaemon(t, bd)
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
	t.Parallel()
	bd := newWorkBD(t)
	recent := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	bd.set(t, "list-hooked.json", `[{"id":"gt-work1","status":"hooked","updated_at":"`+recent+`"}]`)
	d, logBuf := reaperDaemon(t, bd)
	d.tmux = newFakeTmux(newFixedClock())

	d.checkPolecatHealth("myr", "mycat")

	if strings.Contains(logBuf.String(), "CRASH DETECTED") {
		t.Fatalf("a polecat inside its spawn window was called crashed: %s", logBuf)
	}
}

// The intent record's work_bead, when a writer set it, is the seat's work.
func TestCheckPolecatHealth_UsesIntentWorkBead(t *testing.T) {
	t.Parallel()
	bd := newWorkBD(t)
	bd.set(t, "show-gt-intended.json", `[{"id":"gt-intended","status":"in_progress"}]`)
	d, logBuf := reaperDaemon(t, bd)
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
func TestKillRetiredPatrolSessions_HonorsPauseAndLogsActor(t *testing.T) {
	t.Parallel()
	tm := newFakeTmux(newFixedClock())
	tm.addSession("aa-witness", "claude", time.Now())
	tm.addSession("bb-witness", "claude", time.Now())
	tm.addSession("hq-deacon", "claude", time.Now())
	tm.addSession("hq-boot", "claude", time.Now())
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, logger: log.New(&strings.Builder{}, "", 0), tmux: tm, rigPool: newRigWorkerPool(1, 10*time.Second, nil), ctx: context.Background(),
		prefixRegistryFn: rigPrefixes("aa", "bb")}
	writeKnownRigs(t, d.config.TownRoot, "aa", "bb")
	if err := agentpause.Pause(d.config.TownRoot, "aa", "witness", "", "debugging", "human", ""); err != nil {
		t.Fatal(err)
	}

	d.killRetiredPatrolSessions()

	if has, _ := tm.HasSession("aa-witness"); !has {
		t.Error("a paused witness was killed by the retired-role sweep")
	}
	for _, name := range []string{"bb-witness", "hq-deacon", "hq-boot"} {
		if has, _ := tm.HasSession(name); has {
			t.Errorf("%s survived the retired-role sweep", name)
		}
	}
	lines, _ := os.ReadFile(supervisor.ActionLogPath(d.config.TownRoot))
	if !strings.Contains(string(lines), `"actor":"daemon/retired-role"`) {
		t.Errorf("sweep kills missing from the action log: %s", lines)
	}
}

// Ghost sessions belong to no seat: they go through KillStray, which the
// town e-stop refuses.
func TestKillDefaultPrefixGhosts_HonorsTheTownEstop(t *testing.T) {
	t.Parallel()
	reg := session.NewPrefixRegistry()
	reg.Register("aa", "aa")
	tm := newFakeTmux(newFixedClock())
	tm.addSession("gt-witness", "claude", time.Now())
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, logger: log.New(&strings.Builder{}, "", 0), tmux: tm,
		prefixRegistryFn: func() *session.PrefixRegistry { return reg }}
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
	t.Parallel()
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

// myrPrefixes maps rig "myr" to prefix "myr"; a daemon's prefixRegistryFn.
func myrPrefixes() *session.PrefixRegistry { return rigPrefixes("myr")() }

// rigPrefixes maps each rig to a prefix equal to its name.
func rigPrefixes(rigs ...string) func() *session.PrefixRegistry {
	reg := session.NewPrefixRegistry()
	for _, r := range rigs {
		reg.Register(r, r)
	}
	return func() *session.PrefixRegistry { return reg }
}

// unknownTmuxDaemon returns a daemon whose tmux cannot answer, whose restart
// executor records instead of starting anything, and whose town has an
// operational rig "testrig" (its rig bead reads open, with no status label).
func unknownTmuxDaemon(t *testing.T) (*Daemon, *strings.Builder, *[]string) {
	t.Helper()
	town := t.TempDir()
	writeDaemonTownFile(t, town, "testrig/config.json", `{"beads":{"prefix":"gt"}}`)

	tm := newFakeTmux(newFixedClock())
	tm.mu.Lock()
	tm.hasErr = errors.New("tmux: server timed out")
	tm.mu.Unlock()
	var buf strings.Builder
	var restarts []string
	d := &Daemon{
		config: DefaultConfig(town), logger: log.New(&buf, "", 0), tmux: tm,
		restartSeatFn: func(seat supervisor.Seat) error { restarts = append(restarts, seat.SessionName()); return nil },
		rigBeadShowFn: func(_, id string) (*beads.Issue, error) {
			if id != "gt-rig-testrig" {
				return nil, fmt.Errorf("no issue found matching %q", id)
			}
			return &beads.Issue{ID: id, Title: "Rig", Type: "task", Status: "open"}, nil
		},
	}
	return d, &buf, &restarts
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
