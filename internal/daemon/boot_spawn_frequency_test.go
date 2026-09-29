package daemon

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/boot"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

// bootSpawner stands in for boot.Boot.Spawn on a fakeTmux: like the real
// spawn it replaces any live Boot session with a fresh one, and it counts
// the spawns the daemon asked for.
type bootSpawner struct {
	tm *fakeTmux

	mu     sync.Mutex
	spawns int
}

func (s *bootSpawner) spawn(*boot.Boot) error {
	s.mu.Lock()
	s.spawns++
	s.mu.Unlock()
	name := session.BootSessionName()
	_ = s.tm.KillSession(name)
	s.tm.addSession(name, "claude", time.Now())
	return nil
}

func (s *bootSpawner) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spawns
}

// bootTestTown returns a town root and a daemon over it whose tmux is a fake
// and whose Boot spawns are counted by the returned bootSpawner.
func bootTestTown(t *testing.T) (townRoot string, d *Daemon, spawner *bootSpawner) {
	t.Helper()
	townRoot = t.TempDir()
	tm := newFakeTmux(newFixedClock())
	spawner = &bootSpawner{tm: tm}
	d = &Daemon{
		config:   &Config{TownRoot: townRoot},
		logger:   log.New(&bytes.Buffer{}, "", 0),
		tmux:     tm,
		notifier: notifyfake.New(),
	}
	d.spawnBootFn = spawner.spawn
	d.startDeaconFn = func() error { return nil }
	return townRoot, d, spawner
}

// bootTestDaemon returns an agent-mode daemon over a town with a fake tmux,
// plus its log buffer and the Boot spawn counter.
func bootTestDaemon(t *testing.T) (d *Daemon, logs *bytes.Buffer, spawner *bootSpawner) {
	t.Helper()
	townRoot, d, spawner := bootTestTown(t)
	useAgentBootMode(t, townRoot)
	logs = &bytes.Buffer{}
	d.logger = log.New(logs, "", 0)
	return d, logs, spawner
}

// liveBootSession makes the fake tmux report a live Boot session created at
// created.
func liveBootSession(d *Daemon, created time.Time) {
	d.tmux.(*fakeTmux).addSession(session.BootSessionName(), "claude", created)
}

// bootSessionCreated is when the live Boot session was created, or the zero
// time when there is none.
func bootSessionCreated(d *Daemon) time.Time {
	created, err := d.tmux.GetSessionCreatedTime(session.BootSessionName())
	if err != nil {
		return time.Time{}
	}
	return created
}

func writeFakeTmux(t *testing.T, dir string) {
	t.Helper()
	script := `#!/usr/bin/env bash
set -euo pipefail

cmd=""
skip_next=0
for arg in "$@"; do
  if [[ "$skip_next" -eq 1 ]]; then
    skip_next=0
    continue
  fi
  if [[ "$arg" == "-u" ]]; then
    continue
  fi
  if [[ "$arg" == "-L" ]]; then
    skip_next=1
    continue
  fi
  cmd="$arg"
  break
done

if [[ -n "${TMUX_LOG:-}" ]]; then
  printf "%s %s\n" "$cmd" "$*" >> "$TMUX_LOG"
fi

if [[ "${1:-}" == "-V" ]]; then
  echo "tmux 3.3a"
  exit 0
fi

# Keep session checks simple for this regression repro: no existing boot session.
# TMUX_FAKE_SESSION=alive reports the queried session as live instead, and
# TMUX_FAKE_SESSION_CREATED supplies the creation time the turn-budget guard reads.
if [[ "$cmd" == "has-session" ]]; then
  if [[ "${TMUX_FAKE_SESSION:-}" == "alive" ]]; then
    exit 0
  fi
  exit 1
fi

if [[ "$cmd" == "list-sessions" ]]; then
  if [[ -n "${TMUX_FAKE_SESSION_CREATED:-}" ]]; then
    printf "%s\n" "$TMUX_FAKE_SESSION_CREATED"
  fi
  exit 0
fi

exit 0
`
	path := filepath.Join(dir, "tmux")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
}

// TestEnsureBootRunning_SpawnsThroughBoot is the wiring guard for spawnBoot's
// production path: every other ensureBootRunning test replaces the spawn
// (spawnBootFn), so this one leaves it nil and proves boot.Boot.Spawn really
// ran, by the new-session a bash tmux on PATH records. Serial: it sets PATH.
func TestEnsureBootRunning_SpawnsThroughBoot(t *testing.T) {
	townRoot := t.TempDir()
	fakeBinDir := t.TempDir()
	tmuxLog := filepath.Join(t.TempDir(), "tmux.log")
	if err := os.WriteFile(tmuxLog, []byte{}, 0o644); err != nil {
		t.Fatalf("create tmux log: %v", err)
	}
	writeFakeTmux(t, fakeBinDir)
	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX_LOG", tmuxLog)
	t.Setenv("GT_DEGRADED", "false")
	useAgentBootMode(t, townRoot)

	d := &Daemon{
		config:   &Config{TownRoot: townRoot},
		logger:   log.New(&bytes.Buffer{}, "", 0),
		tmux:     tmux.NewTmux(),
		notifier: notifyfake.New(),
	}
	d.ensureBootRunning()

	data, err := os.ReadFile(tmuxLog)
	if err != nil {
		t.Fatalf("read tmux log: %v", err)
	}
	spawns := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "new-session ") && strings.Contains(line, session.BootSessionName()) {
			spawns++
		}
	}
	if spawns != 1 {
		t.Fatalf("boot.Boot.Spawn opened %d Boot session(s), want 1; tmux log:\n%s", spawns, data)
	}
	if d.bootLastSpawned.IsZero() {
		t.Error("a successful spawn must stamp the cooldown")
	}
}

// Regression test for gt-1z0:
// daemon should not spawn a fresh Boot session every heartbeat when triage was just run.
func TestEnsureBootRunning_DoesNotSpawnEveryTick(t *testing.T) {
	t.Parallel()
	d, _, spawner := bootTestDaemon(t)

	// Simulate two adjacent heartbeats.
	d.ensureBootRunning()
	d.ensureBootRunning()

	// Desired behavior (cooldown): single spawn in this short interval.
	if got := spawner.count(); got != 1 {
		t.Fatalf("boot spawn count = %d, want 1 (avoid spawning every daemon tick)", got)
	}
}

// Regression test for gt-qu883c:
// daemon should suppress Boot spawns when Boot's last action was "nothing" (deacon healthy).
func TestEnsureBootRunning_SuppressesWhenDeaconHealthy(t *testing.T) {
	t.Parallel()
	d, _, spawner := bootTestDaemon(t)

	// Write a boot-status.json indicating deacon was healthy ("nothing") recently.
	if err := boot.New(d.config.TownRoot).SaveStatus(&boot.Status{
		StartedAt:   time.Now().Add(-30 * time.Second),
		CompletedAt: time.Now().Add(-20 * time.Second),
		LastAction:  "nothing",
	}); err != nil {
		t.Fatalf("save boot status: %v", err)
	}

	// Even though cooldown has expired (bootLastSpawned is zero),
	// idle suppression should prevent spawning.
	d.ensureBootRunning()

	if got := spawner.count(); got != 0 {
		t.Fatalf("boot spawn count = %d, want 0 (should suppress when deacon healthy)", got)
	}
}

// Test that idle suppression does NOT prevent spawning when Boot's last action was not "nothing".
func TestEnsureBootRunning_SpawnsWhenDeaconUnhealthy(t *testing.T) {
	t.Parallel()
	d, _, spawner := bootTestDaemon(t)

	// Write a boot-status.json indicating Boot had to wake deacon recently.
	if err := boot.New(d.config.TownRoot).SaveStatus(&boot.Status{
		StartedAt:   time.Now().Add(-30 * time.Second),
		CompletedAt: time.Now().Add(-20 * time.Second),
		LastAction:  "wake",
		Target:      "deacon",
	}); err != nil {
		t.Fatalf("save boot status: %v", err)
	}

	// When last action was "wake" (not "nothing"), Boot should still spawn.
	d.ensureBootRunning()

	if got := spawner.count(); got != 1 {
		t.Fatalf("boot spawn count = %d, want 1 (should spawn when deacon was unhealthy)", got)
	}
}

// Regression test for gt-w28o: the daemon must leave a Boot session that is
// still working alone. It used to kill the session and spawn a fresh Boot on
// the next heartbeat, paying a new ~23k-token prefill every ~4 minutes.
func TestEnsureBootRunning_LeavesWorkingBootAlive(t *testing.T) {
	t.Parallel()
	d, logs, spawner := bootTestDaemon(t)
	created := time.Now().Add(-time.Minute)
	liveBootSession(d, created)

	// Two heartbeats inside one Boot turn.
	d.ensureBootRunning()
	d.ensureBootRunning()

	if got := spawner.count(); got != 0 {
		t.Errorf("boot spawns = %d, want 0 (a working Boot must survive the next heartbeat)", got)
	}
	if got := bootSessionCreated(d); !got.Equal(created) {
		t.Errorf("Boot session created at %v, want the original %v (killing mid-turn is the prefill tax)", got, created)
	}
	// The keep branch and the undated branch both spawn nothing: name the one
	// this test means to exercise, or a broken session dating would pass.
	if !strings.Contains(logs.String(), "within the turn budget") {
		t.Errorf("expected the session kept for its turn budget, got logs: %s", logs)
	}
}

// Boot idles at its prompt after `gt boot triage`, so a session whose run is
// already complete is spent: waiting out its turn budget would leave the
// Deacon untriaged for the rest of the budget (gt-w28o).
func TestEnsureBootRunning_ReapsFinishedBootSession(t *testing.T) {
	t.Parallel()
	d, logs, spawner := bootTestDaemon(t)
	liveBootSession(d, time.Now().Add(-2*time.Minute))
	// "nudge" rather than "nothing": an idle-suppressed status returns before
	// the live-session guard is reached.
	if err := boot.New(d.config.TownRoot).SaveStatus(&boot.Status{
		StartedAt:   time.Now().Add(-90 * time.Second),
		CompletedAt: time.Now().Add(-time.Minute),
		LastAction:  "nudge",
	}); err != nil {
		t.Fatalf("save boot status: %v", err)
	}

	d.ensureBootRunning()

	// Replacing the session (killing the spent one) is boot.Boot.Spawn's
	// job; TestEnsureBootRunning_SpawnsThroughBoot guards that the daemon
	// really calls it.
	if got := spawner.count(); got != 1 {
		t.Errorf("boot spawns = %d, want 1 (a spent Boot session must be replaced)", got)
	}
	if !strings.Contains(logs.String(), "finished its triage run") {
		t.Errorf("expected the finished run to be the reason for the reap, got logs: %s", logs)
	}
}

// A triage run that completed before this session started is the previous
// run's stamp, not this one's: the session is still working and keeps its turn
// budget. Reading the comparison the other way would respawn Boot forever.
func TestEnsureBootRunning_KeepsBootAfterEarlierCompletion(t *testing.T) {
	t.Parallel()
	d, logs, spawner := bootTestDaemon(t)
	liveBootSession(d, time.Now().Add(-30*time.Second))
	if err := boot.New(d.config.TownRoot).SaveStatus(&boot.Status{
		StartedAt:   time.Now().Add(-6 * time.Minute),
		CompletedAt: time.Now().Add(-5 * time.Minute),
		LastAction:  "nudge",
	}); err != nil {
		t.Fatalf("save boot status: %v", err)
	}

	d.ensureBootRunning()

	if got := spawner.count(); got != 0 {
		t.Errorf("boot spawns = %d, want 0 (an earlier run's completion stamp must not mark a new session spent)", got)
	}
	if !strings.Contains(logs.String(), "within the turn budget") {
		t.Errorf("expected the session kept for its turn budget, got logs: %s", logs)
	}
}

// A Boot session that outlives the turn budget is wedged, not slow: the daemon
// reaps it so Boot can reach the Deacon again.
func TestEnsureBootRunning_ReapsBootPastTurnBudget(t *testing.T) {
	t.Parallel()
	d, logs, spawner := bootTestDaemon(t)
	liveBootSession(d, time.Now().Add(-time.Hour))

	d.ensureBootRunning()

	if got := spawner.count(); got != 1 {
		t.Errorf("boot spawns = %d, want 1 (a wedged Boot is replaced)", got)
	}
	if !strings.Contains(logs.String(), "past the turn budget") {
		t.Errorf("expected the turn budget to be the reason for the reap, got logs: %s", logs)
	}
}

// A session tmux cannot date has no budget to wait out, so leaving it alone
// would block triage indefinitely: the daemon reaps it instead (gt-w28o).
func TestEnsureBootRunning_ReapsUndatedBootSession(t *testing.T) {
	t.Parallel()
	d, logs, spawner := bootTestDaemon(t)
	tm := d.tmux.(*fakeTmux)
	liveBootSession(d, time.Time{})
	// tmux cannot date the session, and no spawn stamp exists to fall back on.
	tm.createdErr = errNoSessionDate

	d.ensureBootRunning()

	if got := spawner.count(); got != 1 {
		t.Errorf("boot spawns = %d, want 1 (an undated session must be replaced)", got)
	}
	if !strings.Contains(logs.String(), "undated") {
		t.Errorf("expected the missing session date to be the reason for the reap, got logs: %s", logs)
	}
}

// useAgentBootMode opts a test town into boot_mode=agent, so tests that
// assert on Boot tmux spawns keep exercising the agent path now that the
// default is mechanical (gt-fo2k).
func useAgentBootMode(t *testing.T, townRoot string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(townRoot, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "settings", "config.json"), []byte(`{"operational":{"daemon":{"boot_mode":"agent"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
}
