package daemon

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"sync/atomic"
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
	t.Parallel()
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
		{"scheduled slings running", func(_ *testing.T, d *Daemon) { d.scheduledSlingsRunning.Store(true) }, false},
		// gt-u641b: a restart mid-pass killed the merged-tree gate of an
		// in-flight landing, twice in a row.
		{"landing pass in flight", func(_ *testing.T, d *Daemon) { d.landingPasses.Add(1) }, false},
		{"landing pass finished", func(_ *testing.T, d *Daemon) {
			d.landingPasses.Add(1)
			d.landingPasses.Add(-1)
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

func TestScriptRunnerInFlightNilSafe(t *testing.T) {
	t.Parallel()
	var r *scriptRunner
	if got := r.inFlight(); len(got) != 0 {
		t.Fatalf("nil runner inFlight() = %v, want none", got)
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

// holdInstallLock takes an exclusive flock on path (as install-gt.sh's
// install-lock helper does) through a separate open file, released when the
// test ends.
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
	t.Parallel()
	// scripts/install-gt.sh: ${INSTALL_GT_DAEMON_DIR:-$TOWN_ROOT/daemon}/install-gt.lock
	if got, want := installLockPath("/town"), filepath.Join("/town", "daemon", "install-gt.lock"); got != want {
		t.Fatalf("installLockPath = %q, want %q", got, want)
	}
}

func TestInstallLockHeldProbe(t *testing.T) {
	t.Parallel()
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

// TestDaemonWorkHoldsIncludesARunningJevDedupePass covers gt-0h4i0: an upgrade
// restart killed the Jev call a jev_dedupe pass had in flight before its line
// was recorded, and the next pass asked — and billed — that finding again. The
// pass is soft work: it reruns from the new daemon, so it holds the restart only
// up to housekeepingRestartCap.
func TestDaemonWorkHoldsIncludesARunningJevDedupePass(t *testing.T) {
	t.Parallel()
	d := idleTestDaemon(t)
	d.jevDedupeRunning.Store(true)

	hold := d.daemonWorkHold()
	if hold == nil || hold.name != "a jev_dedupe pass" {
		t.Fatalf("daemonWorkHold() = %+v, want the jev_dedupe pass", hold)
	}
	if hold.hard {
		t.Error("a jev_dedupe pass is soft work: the next daemon's pass re-asks its findings")
	}
	if got := d.daemonWorkHolds(); len(got) != 1 || got[0].name != "a jev_dedupe pass" {
		t.Errorf("daemonWorkHolds() = %+v, want just the jev_dedupe pass", got)
	}

	d.jevDedupeRunning.Store(false)
	if hold := d.daemonWorkHold(); hold != nil {
		t.Errorf("daemonWorkHold() = %+v, want none once the pass has ended", hold)
	}
}

// TestDaemonWorkHoldsIncludesTheOtherJevPasses covers gt-tevxz: an upgrade
// restart killed the Jev call a jev_attention, jev_landing, jev_submission or
// jev_dispatch pass had in flight before its line was recorded, and the next
// pass asked — and billed — that item again; gt-dlnkz adds jev_drift,
// jev_logtriage and jev_progress. Each is soft work, as jev_dedupe's is, and
// holds the restart only up to housekeepingRestartCap.
func TestDaemonWorkHoldsIncludesTheOtherJevPasses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		flag func(*Daemon) *atomic.Bool
	}{
		{"jev_attention", func(d *Daemon) *atomic.Bool { return &d.jevAttentionRunning }},
		{"jev_landing", func(d *Daemon) *atomic.Bool { return &d.jevLandingRunning }},
		{"jev_submission", func(d *Daemon) *atomic.Bool { return &d.jevSubmissionRunning }},
		{"jev_dispatch", func(d *Daemon) *atomic.Bool { return &d.jevDispatchRunning }},
		{"jev_drift", func(d *Daemon) *atomic.Bool { return &d.jevDriftRunning }},
		{"jev_logtriage", func(d *Daemon) *atomic.Bool { return &d.jevLogtriageRunning }},
		{"jev_progress", func(d *Daemon) *atomic.Bool { return &d.jevProgressRunning }},
		{"jev_notes", func(d *Daemon) *atomic.Bool { return &d.jevNotesRunning }},
		{"jev_stale", func(d *Daemon) *atomic.Bool { return &d.jevStaleRunning }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := idleTestDaemon(t)
			want := "a " + tc.name + " pass"
			tc.flag(d).Store(true)

			hold := d.daemonWorkHold()
			if hold == nil || hold.name != want {
				t.Fatalf("daemonWorkHold() = %+v, want %q", hold, want)
			}
			if hold.hard {
				t.Errorf("%s is soft work: the next daemon's pass re-asks its items", tc.name)
			}
			if got := d.daemonWorkHolds(); len(got) != 1 || got[0].name != want {
				t.Errorf("daemonWorkHolds() = %+v, want just %q", got, want)
			}

			tc.flag(d).Store(false)
			if hold := d.daemonWorkHold(); hold != nil {
				t.Errorf("daemonWorkHold() = %+v, want none once the pass has ended", hold)
			}
		})
	}
}
