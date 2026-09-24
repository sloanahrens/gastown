package daemon

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/gofrs/flock"
)

func idleTestDaemon(t *testing.T) *Daemon {
	return &Daemon{
		config: &Config{TownRoot: t.TempDir()},
		logger: log.New(io.Discard, "", 0),
	}
}

func TestIsIdleForUpgrade(t *testing.T) {
	cases := []struct {
		name string
		set  func(t *testing.T, d *Daemon)
		want bool
	}{
		{"nothing in flight", func(_ *testing.T, d *Daemon) {}, true},
		{"script plugin running", func(_ *testing.T, d *Daemon) {
			d.scripts = newScriptRunner()
			d.scripts.tryStart("rebuild-gt")
		}, false},
		{"script runner finished", func(_ *testing.T, d *Daemon) {
			d.scripts = newScriptRunner()
			d.scripts.tryStart("rebuild-gt")
			d.scripts.finish("rebuild-gt")
		}, true},
		{"compactor dog running", func(_ *testing.T, d *Daemon) { d.compactorDogRunning = true }, false},
		{"boot triage in flight", func(_ *testing.T, d *Daemon) { d.bootTriageInFlight.Store(true) }, false},
		{"scheduled slings running", func(_ *testing.T, d *Daemon) { d.scheduledSlingsRunning.Store(true) }, false},
		{"mayor dispatch running", func(_ *testing.T, d *Daemon) { d.mayorDispatchRunning.Store(true) }, false},
		{"patrol watchdog running", func(_ *testing.T, d *Daemon) { d.patrolWatchdogRunning.Store(true) }, false},
		{"main branch test mid-run", func(_ *testing.T, d *Daemon) { d.mainBranchTestRunning.Store(true) }, false},
		{"main branch test waiting for a slot", func(_ *testing.T, d *Daemon) {
			d.mainBranchTestRunning.Store(true)
			d.mainBranchTestWaitingSlot.Store(true)
		}, true},
		{"install lock file present, not held", func(t *testing.T, d *Daemon) {
			writeInstallLock(t, d)
		}, true},
		{"install lock held by an install", func(t *testing.T, d *Daemon) {
			holdInstallLock(t, writeInstallLock(t, d))
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := idleTestDaemon(t)
			tc.set(t, d)
			if got := d.isIdleForUpgrade(); got != tc.want {
				t.Fatalf("isIdleForUpgrade() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestScriptRunnerRunningCountNilSafe(t *testing.T) {
	var r *scriptRunner
	if got := r.runningCount(); got != 0 {
		t.Fatalf("nil runner runningCount() = %d, want 0", got)
	}
}

// writeInstallLock creates the town's install-gt.lock file and returns its path.
func writeInstallLock(t *testing.T, d *Daemon) string {
	t.Helper()
	path := installLockPath(d.config.TownRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// holdInstallLock takes an exclusive flock on path (as install-gt.sh's perl
// does) through a separate open file, released when the test ends.
func holdInstallLock(t *testing.T, path string) {
	t.Helper()
	l := flock.New(path)
	ok, err := l.TryLock()
	if err != nil || !ok {
		t.Fatalf("could not take the install lock: ok=%v err=%v", ok, err)
	}
	t.Cleanup(func() { _ = l.Unlock() })
}

func TestInstallLockPathMatchesInstallGt(t *testing.T) {
	// scripts/install-gt.sh: ${INSTALL_GT_DAEMON_DIR:-$TOWN_ROOT/daemon}/install-gt.lock
	if got, want := installLockPath("/town"), filepath.Join("/town", "daemon", "install-gt.lock"); got != want {
		t.Fatalf("installLockPath = %q, want %q", got, want)
	}
}

func TestInstallLockHeldProbe(t *testing.T) {
	t.Run("missing file is not held and is not created", func(t *testing.T) {
		d := idleTestDaemon(t)
		if d.installLockHeld() {
			t.Fatal("missing lock file reported held")
		}
		if _, err := os.Stat(installLockPath(d.config.TownRoot)); !os.IsNotExist(err) {
			t.Fatalf("probe created the lock file (stat err=%v)", err)
		}
	})
	t.Run("held lock reads held; probe releases so the holder keeps it", func(t *testing.T) {
		d := idleTestDaemon(t)
		path := writeInstallLock(t, d)
		holdInstallLock(t, path)
		if !d.installLockHeld() {
			t.Fatal("held lock reported free")
		}
		if !d.installLockHeld() {
			t.Fatal("second probe reported free")
		}
	})
	t.Run("free lock reads free and is left free", func(t *testing.T) {
		d := idleTestDaemon(t)
		path := writeInstallLock(t, d)
		if d.installLockHeld() {
			t.Fatal("free lock reported held")
		}
		// The probe released it: another taker gets it immediately.
		l := flock.New(path)
		ok, err := l.TryLock()
		if err != nil || !ok {
			t.Fatalf("probe left the lock held: ok=%v err=%v", ok, err)
		}
		_ = l.Unlock()
	})
}
