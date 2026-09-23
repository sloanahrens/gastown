package daemon

import (
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/boot"
	"github.com/steveyegge/gastown/internal/tmux"
)

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

// Regression test for gt-1z0:
// daemon should not spawn a fresh Boot session every heartbeat when triage was just run.
func TestEnsureBootRunning_DoesNotSpawnEveryTick(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows — fake tmux requires bash")
	}
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
		config: &Config{TownRoot: townRoot},
		logger: log.New(io.Discard, "", 0),
		tmux:   tmux.NewTmux(),
	}

	// Simulate two adjacent heartbeats.
	d.ensureBootRunning()
	d.ensureBootRunning()

	data, err := os.ReadFile(tmuxLog)
	if err != nil {
		t.Fatalf("read tmux log: %v", err)
	}

	// Desired behavior (cooldown): single spawn in this short interval.
	// Current behavior: two spawns (fails here).
	spawns := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "new-session ") {
			spawns++
		}
	}
	if spawns != 1 {
		t.Fatalf("boot spawn count = %d, want 1 (avoid spawning every daemon tick)", spawns)
	}
}

// Regression test for gt-qu883c:
// daemon should suppress Boot spawns when Boot's last action was "nothing" (deacon healthy).
func TestEnsureBootRunning_SuppressesWhenDeaconHealthy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows — fake tmux requires bash")
	}
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

	// Write a boot-status.json indicating deacon was healthy ("nothing") recently.
	b := boot.New(townRoot)
	if err := b.SaveStatus(&boot.Status{
		StartedAt:   time.Now().Add(-30 * time.Second),
		CompletedAt: time.Now().Add(-20 * time.Second),
		LastAction:  "nothing",
	}); err != nil {
		t.Fatalf("save boot status: %v", err)
	}

	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: log.New(io.Discard, "", 0),
		tmux:   tmux.NewTmux(),
	}

	// Even though cooldown has expired (bootLastSpawned is zero),
	// idle suppression should prevent spawning.
	d.ensureBootRunning()

	data, err := os.ReadFile(tmuxLog)
	if err != nil {
		t.Fatalf("read tmux log: %v", err)
	}

	spawns := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "new-session ") {
			spawns++
		}
	}
	if spawns != 0 {
		t.Fatalf("boot spawn count = %d, want 0 (should suppress when deacon healthy)", spawns)
	}
}

// Test that idle suppression does NOT prevent spawning when Boot's last action was not "nothing".
func TestEnsureBootRunning_SpawnsWhenDeaconUnhealthy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows — fake tmux requires bash")
	}
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

	// Write a boot-status.json indicating Boot had to wake deacon recently.
	b := boot.New(townRoot)
	if err := b.SaveStatus(&boot.Status{
		StartedAt:   time.Now().Add(-30 * time.Second),
		CompletedAt: time.Now().Add(-20 * time.Second),
		LastAction:  "wake",
		Target:      "deacon",
	}); err != nil {
		t.Fatalf("save boot status: %v", err)
	}

	d := &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: log.New(io.Discard, "", 0),
		tmux:   tmux.NewTmux(),
	}

	// When last action was "wake" (not "nothing"), Boot should still spawn.
	d.ensureBootRunning()

	data, err := os.ReadFile(tmuxLog)
	if err != nil {
		t.Fatalf("read tmux log: %v", err)
	}

	spawns := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, "new-session ") {
			spawns++
		}
	}
	if spawns != 1 {
		t.Fatalf("boot spawn count = %d, want 1 (should spawn when deacon was unhealthy)", spawns)
	}
}

// bootTestTown returns a town root wired to a fake tmux, plus the path of the
// tmux command log.
func bootTestTown(t *testing.T) (townRoot, tmuxLog string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("skipping on Windows — fake tmux requires bash")
	}
	townRoot = t.TempDir()
	fakeBinDir := t.TempDir()
	tmuxLog = filepath.Join(t.TempDir(), "tmux.log")
	if err := os.WriteFile(tmuxLog, []byte{}, 0o644); err != nil {
		t.Fatalf("create tmux log: %v", err)
	}
	writeFakeTmux(t, fakeBinDir)
	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX_LOG", tmuxLog)
	t.Setenv("GT_DEGRADED", "false")
	return townRoot, tmuxLog
}

// countTmuxCmd counts the tmux subcommands recorded in the fake tmux log.
func countTmuxCmd(t *testing.T, tmuxLog, cmd string) int {
	t.Helper()
	data, err := os.ReadFile(tmuxLog)
	if err != nil {
		t.Fatalf("read tmux log: %v", err)
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.HasPrefix(line, cmd+" ") {
			n++
		}
	}
	return n
}

// bootTestDaemon returns an agent-mode daemon over a town with a fake tmux,
// plus its log buffer and the tmux command log.
func bootTestDaemon(t *testing.T) (d *Daemon, logs *bytes.Buffer, tmuxLog string) {
	t.Helper()
	townRoot, tmuxLog := bootTestTown(t)
	useAgentBootMode(t, townRoot)
	logs = &bytes.Buffer{}
	d = &Daemon{
		config: &Config{TownRoot: townRoot},
		logger: log.New(logs, "", 0),
		tmux:   tmux.NewTmux(),
	}
	return d, logs, tmuxLog
}

// liveBootSession makes the fake tmux report a live Boot session created at
// created.
func liveBootSession(t *testing.T, created time.Time) {
	t.Helper()
	t.Setenv("TMUX_FAKE_SESSION", "alive")
	t.Setenv("TMUX_FAKE_SESSION_CREATED", strconv.FormatInt(created.Unix(), 10))
}

// Regression test for gt-w28o: the daemon must leave a Boot session that is
// still working alone. It used to kill the session and spawn a fresh Boot on
// the next heartbeat, paying a new ~23k-token prefill every ~4 minutes.
func TestEnsureBootRunning_LeavesWorkingBootAlive(t *testing.T) {
	d, logs, tmuxLog := bootTestDaemon(t)
	liveBootSession(t, time.Now().Add(-time.Minute))

	// Two heartbeats inside one Boot turn.
	d.ensureBootRunning()
	d.ensureBootRunning()

	if got := countTmuxCmd(t, tmuxLog, "new-session"); got != 0 {
		t.Errorf("boot spawns = %d, want 0 (a working Boot must survive the next heartbeat)", got)
	}
	if got := countTmuxCmd(t, tmuxLog, "kill-session"); got != 0 {
		t.Errorf("boot kills = %d, want 0 (killing mid-turn is the prefill tax)", got)
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
	d, logs, tmuxLog := bootTestDaemon(t)
	liveBootSession(t, time.Now().Add(-2*time.Minute))
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

	if got := countTmuxCmd(t, tmuxLog, "new-session"); got != 1 {
		t.Errorf("boot spawns = %d, want 1 (a spent Boot session must be replaced)", got)
	}
	// At least one, not exactly one: the spawn that follows reaps the stale
	// session on its own too.
	if got := countTmuxCmd(t, tmuxLog, "kill-session"); got < 1 {
		t.Errorf("boot kills = %d, want at least 1 (the spent session must be reaped)", got)
	}
	if !strings.Contains(logs.String(), "finished its triage run") {
		t.Errorf("expected the finished run to be the reason for the reap, got logs: %s", logs)
	}
}

// A triage run that completed before this session started is the previous
// run's stamp, not this one's: the session is still working and keeps its turn
// budget. Reading the comparison the other way would respawn Boot forever.
func TestEnsureBootRunning_KeepsBootAfterEarlierCompletion(t *testing.T) {
	d, logs, tmuxLog := bootTestDaemon(t)
	liveBootSession(t, time.Now().Add(-30*time.Second))
	if err := boot.New(d.config.TownRoot).SaveStatus(&boot.Status{
		StartedAt:   time.Now().Add(-6 * time.Minute),
		CompletedAt: time.Now().Add(-5 * time.Minute),
		LastAction:  "nudge",
	}); err != nil {
		t.Fatalf("save boot status: %v", err)
	}

	d.ensureBootRunning()

	if got := countTmuxCmd(t, tmuxLog, "new-session"); got != 0 {
		t.Errorf("boot spawns = %d, want 0 (an earlier run's completion stamp must not mark a new session spent)", got)
	}
	if !strings.Contains(logs.String(), "within the turn budget") {
		t.Errorf("expected the session kept for its turn budget, got logs: %s", logs)
	}
}

// A Boot session that outlives the turn budget is wedged, not slow: the daemon
// reaps it so Boot can reach the Deacon again.
func TestEnsureBootRunning_ReapsBootPastTurnBudget(t *testing.T) {
	d, logs, tmuxLog := bootTestDaemon(t)
	liveBootSession(t, time.Now().Add(-time.Hour))

	d.ensureBootRunning()

	if got := countTmuxCmd(t, tmuxLog, "new-session"); got != 1 {
		t.Errorf("boot spawns = %d, want 1 (a wedged Boot is replaced)", got)
	}
	// At least one, not exactly one: the spawn that follows reaps the stale
	// session on its own too.
	if got := countTmuxCmd(t, tmuxLog, "kill-session"); got < 1 {
		t.Errorf("boot kills = %d, want at least 1 (the wedged session must be reaped)", got)
	}
	if !strings.Contains(logs.String(), "past the turn budget") {
		t.Errorf("expected the turn budget to be the reason for the reap, got logs: %s", logs)
	}
}

// A session tmux cannot date has no budget to wait out, so leaving it alone
// would block triage indefinitely: the daemon reaps it instead (gt-w28o).
func TestEnsureBootRunning_ReapsUndatedBootSession(t *testing.T) {
	d, logs, tmuxLog := bootTestDaemon(t)
	t.Setenv("TMUX_FAKE_SESSION", "alive")
	// TMUX_FAKE_SESSION_CREATED is left unset, so the fake tmux cannot date the
	// session and no spawn stamp exists to fall back on.

	d.ensureBootRunning()

	if got := countTmuxCmd(t, tmuxLog, "new-session"); got != 1 {
		t.Errorf("boot spawns = %d, want 1 (an undated session must be replaced)", got)
	}
	if got := countTmuxCmd(t, tmuxLog, "kill-session"); got < 1 {
		t.Errorf("boot kills = %d, want at least 1 (an undated session must be reaped)", got)
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
