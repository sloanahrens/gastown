//go:build integration

package daemon

import (
	"bytes"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/deacon"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
)

// Wiring guards whose production collaborator is a real process: each leaves
// one seam nil and proves the collaborator behind it ran. The unit tier's
// guards that need no process are in wiring_guard_test.go.

// TestIntegrationHostLoadMeasuresTheRealHost guards hostLoad's nil path: the reading is
// the host's own (its CPU count), not a zero value.
func TestIntegrationHostLoadMeasuresTheRealHost(t *testing.T) {
	t.Parallel()
	d := &Daemon{}
	if got := d.hostLoad().NumCPU; got != runtime.NumCPU() {
		t.Errorf("hostLoad().NumCPU = %d, want this host's %d: the nil seam must measure the real host", got, runtime.NumCPU())
	}
}

// TestIntegrationEnsureDeaconRunning_StartsThroughTheManager guards startDeacon's nil
// path: ensureDeaconRunning really asks deacon.Manager to create the session.
// The bash tmux on PATH refuses the create, so the manager's error must reach
// the log; a startDeacon that did nothing would log a successful start.
// Serial: it sets PATH.
func TestIntegrationEnsureDeaconRunning_StartsThroughTheManager(t *testing.T) {
	binDir := t.TempDir()
	tmuxLog := filepath.Join(t.TempDir(), "tmux.log")
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> '" + tmuxLog + "'\n" +
		"case \"$*\" in\n" +
		// A missing session answers the way tmux does; a bare exit 1
		// would be an unknown answer, which starts nothing.
		"  *has-session*) echo \"can't find session: hq-deacon\" >&2; exit 1;;\n" +
		"  *new-session*) echo 'create refused by test' >&2; exit 1;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var logs bytes.Buffer
	d := &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(&logs, "", 0),
		tmux:   tmux.NewTmux(),
	}
	d.ensureDeaconRunning()

	data, _ := os.ReadFile(tmuxLog)
	if !strings.Contains(string(data), "new-session") || !strings.Contains(string(data), deacon.SessionName()) {
		t.Errorf("deacon.Manager never tried to create %s; tmux calls:\n%s", deacon.SessionName(), data)
	}
	if !strings.Contains(logs.String(), "Error starting Deacon") {
		t.Errorf("the manager's refused create must be logged as a failed start, got:\n%s", logs.String())
	}
}

// TestIntegrationEnsureBootRunning_SpawnsThroughBoot is the wiring guard for spawnBoot's
// production path: every other ensureBootRunning test replaces the spawn
// (spawnBootFn), so this one leaves it nil and proves boot.Boot.Spawn really
// ran, by the new-session a bash tmux on PATH records. Serial: it sets PATH.
func TestIntegrationEnsureBootRunning_SpawnsThroughBoot(t *testing.T) {
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

// TestIntegrationListOriginBranchesReadsTheRigOrigin guards listOriginBranches' nil
// path: it lists the polecat branches on the rig's real origin remote.
func TestIntegrationListOriginBranchesReadsTheRigOrigin(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	origin := filepath.Join(t.TempDir(), "origin.git")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	wireGit(t, origin, "init", "--bare")

	clone := filepath.Join(townRoot, "gt", "mayor", "rig")
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	const branch = "polecat/basalt/gt-issue1+abc123"
	wireGit(t, clone, "init")
	wireGit(t, clone, "config", "user.email", "test@test.com")
	wireGit(t, clone, "config", "user.name", "Test")
	wireGit(t, clone, "remote", "add", "origin", origin)
	wireGit(t, clone, "checkout", "-b", branch)
	wireGit(t, clone, "commit", "--allow-empty", "-m", "work")
	wireGit(t, clone, "push", "origin", branch)

	m := NewConvoyManager(townRoot, func(string, ...interface{}) {}, "gt", 10*time.Minute, nil, nil, nil)
	got, err := m.listOriginBranches(filepath.Join(townRoot, "gt"))
	if err != nil {
		t.Fatalf("listOriginBranches: %v", err)
	}
	if want := []string{branch}; !reflect.DeepEqual(got, want) {
		t.Errorf("listOriginBranches = %v, want %v from the rig's origin", got, want)
	}
}

// wireGit runs git in dir for a wiring guard's fixture, isolated from the
// host's git config.
func wireGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
}
