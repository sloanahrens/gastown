package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/templates"
)

// fakeSupervisor answers DaemonCheck's supervisor questions in process.
type fakeSupervisor struct {
	kind    string // the provisioned job's kind; "" when none is
	missing bool   // the service manager does not know the job
}

func (f fakeSupervisor) JobMissing(string) (string, bool) {
	if f.kind == "" || !f.missing {
		return "", false
	}
	return f.kind, true
}

func (f fakeSupervisor) StatusLine(_ string, lockPID int) string {
	if f.kind == "" {
		return "none"
	}
	return templates.SupervisorState{Kind: f.kind, Loaded: !f.missing, LastExit: -1}.StatusLine(lockPID)
}

// A provisioned supervisor job the service manager does not know is an error
// even when a daemon is up: nothing restarts that daemon if it dies
// (gt-4k3fj.11).
func TestDaemonCheck_Run_FailsWhenTheProvisionedJobIsNotLoaded(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	check := NewDaemonCheck()
	check.supervisor = fakeSupervisor{kind: "launchd", missing: true}
	result := check.Run(&CheckContext{TownRoot: townRoot})
	if result.Status != StatusError || !strings.Contains(result.Message, "not loaded") {
		t.Errorf("Run() = (%v, %q), want an error naming the unloaded job", result.Status, result.Message)
	}

	check.supervisor = fakeSupervisor{kind: "launchd"}
	if result := check.Run(&CheckContext{TownRoot: townRoot}); strings.Contains(result.Message, "not loaded") {
		t.Errorf("Run() = %q for a loaded job, want no unloaded-job failure", result.Message)
	}
}

// TestDaemonCheck_Run_SurfacesSupervisedStatus verifies the daemon check's
// details include a "Supervised: <value>" line, matching what
// 'gt daemon status' reports, whether or not the daemon is running.
func TestDaemonCheck_Run_SurfacesSupervisedStatus(t *testing.T) {
	t.Parallel()
	check := NewDaemonCheck()
	check.supervisor = fakeSupervisor{}
	result := check.Run(&CheckContext{TownRoot: t.TempDir()})

	found := false
	for _, d := range result.Details {
		if strings.HasPrefix(d, "Supervised: ") {
			found = true
			if d != "Supervised: none" {
				t.Errorf("Details supervised line = %q, want %q", d, "Supervised: none")
			}
		}
	}
	if !found {
		t.Errorf("DaemonCheck.Run() details = %v, want a 'Supervised: ' line", result.Details)
	}
}

// TestDaemonCheck_Run_HeartbeatsAreDecimal pins the heartbeat detail to the
// decimal count. string(rune(n)) printed the code point instead — 65 as "A" —
// so a healthy run surfaced a glyph where the count belongs (gt-abbr).
func TestDaemonCheck_Run_HeartbeatsAreDecimal(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()

	// IsRunning reports a daemon only while the lock is held, so the test
	// plays the daemon and holds it (gt-utuk).
	if err := os.MkdirAll(filepath.Join(townRoot, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(filepath.Join(townRoot, "daemon", "daemon.lock"))
	if err := lock.Lock(); err != nil {
		t.Fatalf("holding daemon lock: %v", err)
	}
	t.Cleanup(func() { _ = lock.Unlock() })

	state := &daemon.State{
		Running:        true,
		PID:            os.Getpid(),
		StartedAt:      time.Now().Add(-time.Hour),
		HeartbeatCount: 65,
	}
	if err := daemon.SaveState(townRoot, state); err != nil {
		t.Fatalf("SaveState: %v", err)
	}

	check := NewDaemonCheck()
	check.supervisor = fakeSupervisor{kind: "launchd"}
	result := check.Run(&CheckContext{TownRoot: townRoot})
	if result.Status != StatusOK {
		t.Fatalf("Run() status = %v (%s), want StatusOK", result.Status, result.Message)
	}

	want := "Heartbeats: 65"
	for _, d := range result.Details {
		if d == want {
			return
		}
	}
	t.Errorf("Run() details = %v, want %q", result.Details, want)
}
