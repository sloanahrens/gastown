//go:build integration && !windows

package daemon

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/steveyegge/gastown/internal/doltserver"
)

// gt-p7zy0: every stop path that signals a PID it did not just start must
// prove the PID is the process it means to stop. These run the real chain —
// ps reading a live process's argv, then the signal — and only ever point a
// stop path at processes the test itself spawned: an unrelated `sleep` (must
// survive) or this test binary re-executed as `gt daemon run` / `dolt
// sql-server` (must be signaled). The identity decisions themselves are
// unit-tested in pid_identity_test.go on the processArgsFn/processCWDFn and
// verifyDoltSQLServerFn seams.

// ownedChild is a process this test started. wait reports how it ended.
type ownedChild struct {
	cmd  *exec.Cmd
	done chan struct{}
	once sync.Once
	err  error
}

func startOwnedChild(t *testing.T, cmd *exec.Cmd) *ownedChild {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %v: %v", cmd.Args, err)
	}
	c := &ownedChild{cmd: cmd, done: make(chan struct{})}
	// Reap promptly so a signaled child does not linger as a zombie that
	// still answers signal 0.
	go func() {
		c.err = cmd.Wait()
		close(c.done)
	}()
	t.Cleanup(func() {
		select {
		case <-c.done:
		default:
			_ = cmd.Process.Kill() // our own child
			<-c.done
		}
	})
	return c
}

func (c *ownedChild) pid() int { return c.cmd.Process.Pid }

func (c *ownedChild) exited(within time.Duration) bool {
	select {
	case <-c.done:
		return true
	case <-time.After(within):
		return false
	}
}

// terminatedBySignal reports whether the child died of sig.
func (c *ownedChild) terminatedBySignal(sig syscall.Signal) bool {
	var exitErr *exec.ExitError
	if !errors.As(c.err, &exitErr) {
		return false
	}
	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == sig
}

// startUnrelatedSleep starts `sleep 60`: a live process that is neither the
// gt daemon nor a dolt server, standing in for Docker Desktop.
func startUnrelatedSleep(t *testing.T) *ownedChild {
	t.Helper()
	return startOwnedChild(t, exec.Command("sleep", "60"))
}

// startImpersonator re-executes this test binary through a symlink named
// argv0, with args, so ps shows exactly `<dir>/argv0 args...`.
func startImpersonator(t *testing.T, argv0 string, args ...string) *ownedChild {
	t.Helper()
	return startImpersonatorIn(t, "", argv0, args...)
}

// startImpersonatorIn is startImpersonator with the child's working
// directory set to dir ("" inherits ours): the daemon's town is its cwd.
func startImpersonatorIn(t *testing.T, dir, argv0 string, args ...string) *ownedChild {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	link := filepath.Join(t.TempDir(), argv0)
	if err := os.Symlink(self, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cmd := exec.Command(link, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), signalTargetHelperEnv+"=1")
	return startOwnedChild(t, cmd)
}

// holdDaemonLock makes a town whose daemon.lock is held (as by a running
// daemon) and whose daemon.pid names pid.
func holdDaemonLock(t *testing.T, pid int) string {
	t.Helper()
	townRoot := t.TempDir()
	holdDaemonLockIn(t, townRoot, pid)
	return townRoot
}

// holdDaemonLockIn is holdDaemonLock for a town that already exists, so a
// daemon impersonator can be started with that town as its cwd first.
func holdDaemonLockIn(t *testing.T, townRoot string, pid int) {
	t.Helper()
	daemonDir := filepath.Join(townRoot, "daemon")
	if err := os.MkdirAll(daemonDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(filepath.Join(daemonDir, "daemon.lock"))
	locked, err := lock.TryLock()
	if err != nil || !locked {
		t.Fatalf("hold daemon.lock: locked=%v err=%v", locked, err)
	}
	t.Cleanup(func() { _ = lock.Unlock() })
	if _, err := writePIDFile(filepath.Join(daemonDir, "daemon.pid"), pid); err != nil {
		t.Fatal(err)
	}
}

// A daemon.pid naming an unrelated live process (the gt-p7zy0 shape: a
// stale or foreign PID) must not be signaled, and nothing is cleaned up
// while the lock is held.
func TestIntegrationStopDaemonDoesNotSignalUnrelatedProcess(t *testing.T) {
	t.Parallel()
	victim := startUnrelatedSleep(t)
	townRoot := holdDaemonLock(t, victim.pid())

	err := StopDaemon(townRoot)
	if err == nil || !strings.Contains(err.Error(), "refusing to signal") {
		t.Fatalf("StopDaemon = %v, want a refusal", err)
	}
	if victim.exited(700 * time.Millisecond) {
		t.Fatalf("StopDaemon signaled an unrelated process (pid %d): %v", victim.pid(), victim.err)
	}
	if _, statErr := os.Stat(filepath.Join(townRoot, "daemon", "daemon.pid")); statErr != nil {
		t.Errorf("refusal removed daemon.pid: %v", statErr)
	}
}

// The real daemon — a process whose argv is `gt daemon run` — is stopped.
func TestIntegrationStopDaemonSignalsVerifiedDaemon(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	daemonProc := startImpersonatorIn(t, townRoot, "gt", "daemon", "run")
	holdDaemonLockIn(t, townRoot, daemonProc.pid())

	if err := StopDaemon(townRoot); err != nil {
		t.Fatalf("StopDaemon: %v", err)
	}
	if !daemonProc.exited(5 * time.Second) {
		t.Fatal("the verified daemon was not stopped")
	}
	if !daemonProc.terminatedBySignal(syscall.SIGTERM) && !daemonProc.terminatedBySignal(syscall.SIGKILL) {
		t.Errorf("daemon ended by %v, want a signal", daemonProc.err)
	}
}

func TestIntegrationDoltStopSignalsVerifiedDoltServer(t *testing.T) {
	t.Parallel()
	m, logs := newStopTestManager(t, 0)
	server := startImpersonator(t, "dolt", "sql-server", "--config", townDoltConfig(m.townRoot))
	m.runningFn = func() (int, bool) { return server.pid(), true }

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !server.exited(5 * time.Second) {
		t.Fatalf("verified dolt sql-server not stopped; log: %q", logs.String())
	}
	if !server.terminatedBySignal(syscall.SIGTERM) {
		t.Errorf("dolt sql-server ended by %v, want SIGTERM", server.err)
	}
}

// townDoltConfig is the --config path Gas Town starts townRoot's dolt with.
func townDoltConfig(townRoot string) string {
	return filepath.Join(doltserver.DefaultConfig(townRoot).DataDir, "config.yaml")
}
