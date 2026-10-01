package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/supervisor"
	"github.com/steveyegge/gastown/internal/tmux"
)

// workBD is the daemon's work-bead database for a test: a beadsfake the
// daemon reads through openWorkBeads, recording every read and the bd
// environment it was opened with. Its run is the daemon's execCmd and fails
// every subprocess, so nothing the test did not plan escapes. Wire it with
// d.openWorkBeads = bd.open and d.execCmd = bd.run.
type workBD struct {
	*fakeCLI
	db *beadsfake.Fake
	// onlyIn, when set, is the one BEADS_DIR db answers for: a read opened
	// with any other sees an empty database, as bd would.
	onlyIn string
	// listErr, when set, fails every list.
	listErr error

	mu    sync.Mutex
	reads []string
	envs  [][]string
}

func newWorkBD(t *testing.T) *workBD {
	t.Helper()
	return &workBD{
		fakeCLI: newFakeCLI(func([]string) cliReply { return cliReply{code: 1} }),
		db:      beadsfake.New(),
	}
}

// seed stores a work bead assigned to myr/polecats/mycat.
func (b *workBD) seed(id, status string, updated time.Time, labels ...string) {
	b.db.Seed(beads.Issue{ID: id, Status: status, Assignee: "myr/polecats/mycat",
		UpdatedAt: updated.UTC().Format(time.RFC3339), Labels: labels})
}

// open is the daemon's openWorkBeads.
func (b *workBD) open(env []string) workBeadReader {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.envs = append(b.envs, env)
	if b.onlyIn != "" && (cliCall{env: env}).getenv("BEADS_DIR") != b.onlyIn {
		return workBDReader{b: b, db: beadsfake.New()}
	}
	return workBDReader{b: b, db: b.db}
}

func (b *workBD) read(what string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reads = append(b.reads, what)
}

// calls returns every recorded read, one per line: "show <id>" or
// "list <status> <assignee>".
func (b *workBD) calls(t *testing.T) string {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Join(b.reads, "\n")
}

type workBDReader struct {
	b  *workBD
	db *beadsfake.Fake
}

func (r workBDReader) Show(id string) (*beads.Issue, error) {
	r.b.read("show " + id)
	return r.db.Show(id)
}

func (r workBDReader) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	r.b.read("list " + opts.Status + " " + opts.Assignee)
	if r.b.listErr != nil {
		return nil, r.b.listErr
	}
	return r.db.List(opts)
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
		d.openWorkBeads = bd.open
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
	bd.seed("gt-work1", "hooked", time.Now().Add(-time.Hour))
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
	bd.seed("gt-work1", "hooked", time.Now().Add(-time.Minute))
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
	bd.seed("gt-intended", "in_progress", time.Now())
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

// The town e-stop holds dispatch and restarts, not upkeep: the heartbeat
// still ensures Dolt under it and runs no lifecycle step (gt-4k3fj.8).
func TestHeartbeat_EstopStillEnsuresDoltRefusesRestarts(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	probes := 0
	d.doltServer = &DoltServerManager{
		config:        &DoltServerConfig{Enabled: true},
		townRoot:      d.config.TownRoot,
		logger:        func(string, ...interface{}) {},
		runningFn:     func() (int, bool) { return 4242, true },
		healthCheckFn: func() error { probes++; return nil },
	}
	var ran []string
	d.seams.heartbeatStep = func(d *Daemon, step heartbeatStep) {
		ran = append(ran, step.name)
		if step.name == "dolt" {
			step.run(d)
		}
	}
	if err := estop.Activate(d.config.TownRoot, estop.TriggerManual, "drill"); err != nil {
		t.Fatal(err)
	}

	state := &State{}
	d.heartbeat(state)

	if probes != 1 {
		t.Errorf("Dolt health probes under the e-stop = %d, want 1", probes)
	}
	for _, step := range heartbeatSteps {
		if step.lifecycle && slices.Contains(ran, step.name) {
			t.Errorf("lifecycle step %q ran under the town e-stop", step.name)
		}
	}
	if want := []string{"rigs-cache", "prefix-registry", "dolt", "branch-prune", "log-rotation", "events-prune", "git-hygiene"}; !slices.Equal(ran, want) {
		t.Errorf("steps under the e-stop = %v, want %v", ran, want)
	}
	if state.HeartbeatCount != 1 {
		t.Errorf("HeartbeatCount = %d, want 1: the e-stop heartbeat must still record itself", state.HeartbeatCount)
	}

	ran = nil
	d.heartbeatWork(&State{}, true)
	if len(ran) != len(heartbeatSteps) {
		t.Errorf("steps without the e-stop = %v, want all %d", ran, len(heartbeatSteps))
	}
	if i := slices.Index(ran, "dolt"); i < 0 || i > slices.Index(ran, "mayor") {
		t.Errorf("Dolt must be ensured before the mayor step: %v", ran)
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
