package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gofrs/flock"
)

// gt-fsgb1: daemon.pid is what names the daemon, and nothing rewrites it after
// startup. A stop that removes it while the daemon still runs — or removes one
// a later daemon wrote — leaves IsRunning answering (true, 0) and every stop
// refusing with "pid is not known yet".

// lockTown makes a town a daemon has run in and left: the daemon.lock file is
// there, nothing holds it.
func lockTown(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(daemonLockPath(townRoot), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return townRoot
}

// daemonLockPath is the flock Run holds for its whole life.
func daemonLockPath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", "daemon.lock")
}

// pidTown makes a town whose daemon.pid names pid.
func pidTown(t *testing.T, pid int) (townRoot, pidFile string) {
	t.Helper()
	townRoot = t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	pidFile = filepath.Join(townRoot, "daemon", "daemon.pid")
	if _, err := writePIDFile(pidFile, pid); err != nil {
		t.Fatal(err)
	}
	return townRoot, pidFile
}

func statPIDFile(t *testing.T, pidFile string) error {
	t.Helper()
	_, err := os.Stat(pidFile)
	return err
}

// A daemon that outlived its stop keeps its pid file: removing it loses the
// PID, and nothing writes it again until that daemon restarts.
func TestFinishStopDaemonKeepsPIDFileWhileTheDaemonRuns(t *testing.T) {
	t.Parallel()
	townRoot, pidFile := pidTown(t, 4242)

	err := finishStopDaemon(townRoot, 4242, false)
	if err == nil {
		t.Fatal("finishStopDaemon reported success for a daemon that is still running")
	}
	if statErr := statPIDFile(t, pidFile); statErr != nil {
		t.Errorf("daemon.pid was removed while its daemon still ran: %v", statErr)
	}
}

// A stop that finished clears the pid file it owned.
func TestFinishStopDaemonRemovesTheStoppedDaemonsPIDFile(t *testing.T) {
	t.Parallel()
	townRoot, pidFile := pidTown(t, 4242)

	if err := finishStopDaemon(townRoot, 4242, true); err != nil {
		t.Fatalf("finishStopDaemon: %v", err)
	}
	if statErr := statPIDFile(t, pidFile); !os.IsNotExist(statErr) {
		t.Errorf("daemon.pid survived a finished stop: %v", statErr)
	}
}

// A pid file written by a daemon that started the moment the stopped one let go
// of the lock stays: that daemon holds daemon.lock, whatever PID the file names.
func TestFinishStopDaemonKeepsAFileWrittenByADaemonHoldingTheLock(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		filedPID int
	}{
		{"new daemon's own pid", 7777},
		{"pid reused by the new daemon", 4242},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			townRoot, pidFile := pidTown(t, tc.filedPID)
			lock := flock.New(daemonLockPath(townRoot))
			locked, err := lock.TryLock()
			if err != nil || !locked {
				t.Fatalf("taking daemon.lock as the new daemon: locked=%v err=%v", locked, err)
			}
			defer func() { _ = lock.Unlock() }()

			if err := finishStopDaemon(townRoot, 4242, true); err != nil {
				t.Fatalf("finishStopDaemon: %v", err)
			}
			if statErr := statPIDFile(t, pidFile); statErr != nil {
				t.Errorf("the pid file of a daemon holding daemon.lock was removed: %v", statErr)
			}
			if pid, _, err := readPIDFile(pidFile); err != nil || pid != tc.filedPID {
				t.Errorf("daemon.pid = (%d, %v), want the PID %d it named", pid, err, tc.filedPID)
			}
		})
	}
}

// With nothing holding daemon.lock, the stopped daemon's file goes even where
// the lock file exists.
func TestFinishStopDaemonRemovesTheFileOfAFreeLock(t *testing.T) {
	t.Parallel()
	townRoot := lockTown(t)
	pidFile := filepath.Join(townRoot, "daemon", "daemon.pid")
	if _, err := writePIDFile(pidFile, 4242); err != nil {
		t.Fatal(err)
	}

	if err := finishStopDaemon(townRoot, 4242, true); err != nil {
		t.Fatalf("finishStopDaemon: %v", err)
	}
	if statErr := statPIDFile(t, pidFile); !os.IsNotExist(statErr) {
		t.Errorf("daemon.pid survived a finished stop on a free lock: %v", statErr)
	}
}

// The probe lock must be held across the removal: released first, the window
// between the two lets a starting daemon write a pid file this then deletes.
func TestFreeDaemonLockSettlesThePIDFileBeforeItReleases(t *testing.T) {
	t.Parallel()
	_, pidFile := pidTown(t, 4242)

	released := false
	settleFreeDaemonLock(pidFile, func() error {
		released = true
		if statErr := statPIDFile(t, pidFile); !os.IsNotExist(statErr) {
			t.Error("the probe lock was released before daemon.pid was removed; a daemon starting in that window loses its PID")
		}
		return nil
	})

	if !released {
		t.Fatal("settleFreeDaemonLock never released the probe lock")
	}
	if statErr := statPIDFile(t, pidFile); !os.IsNotExist(statErr) {
		t.Errorf("the stale pid file survived: %v", statErr)
	}
}

// IsRunning still clears the pid file a town with no daemon is left holding.
func TestIsRunningClearsAStalePIDFile(t *testing.T) {
	t.Parallel()
	townRoot := lockTown(t)
	pidFile := filepath.Join(townRoot, "daemon", "daemon.pid")
	if _, err := writePIDFile(pidFile, 4242); err != nil {
		t.Fatal(err)
	}

	running, pid, err := IsRunning(townRoot)
	if err != nil {
		t.Fatalf("IsRunning: %v", err)
	}
	if running || pid != 0 {
		t.Errorf("IsRunning = (%v, %d), want no daemon", running, pid)
	}
	if statErr := statPIDFile(t, pidFile); !os.IsNotExist(statErr) {
		t.Errorf("the stale pid file survived IsRunning: %v", statErr)
	}
}
