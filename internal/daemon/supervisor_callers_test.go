package daemon

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/agentpause"
	"github.com/steveyegge/gastown/internal/estop"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/supervisor"
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
