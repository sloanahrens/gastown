package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gofrs/flock"

	"github.com/steveyegge/gastown/internal/doltserver"
)

// gt-p7zy0: every stop path that signals a PID it did not just start must
// prove the PID is the process it means to stop. These tests only ever
// point a stop path at processes the test itself spawned: an unrelated
// `sleep` (must survive) or this test binary re-executed as `gt daemon run`
// / `dolt sql-server` (must be signaled).

const signalTargetHelperEnv = "GT_DAEMON_TEST_SIGNAL_TARGET"

// runSignalTargetHelper is the helper process body: wait to be signaled,
// and exit on its own after a minute so a failed test cannot leak it.
func runSignalTargetHelper() {
	time.Sleep(time.Minute)
	os.Exit(0)
}

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
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	link := filepath.Join(t.TempDir(), argv0)
	if err := os.Symlink(self, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cmd := exec.Command(link, args...)
	cmd.Env = append(os.Environ(), signalTargetHelperEnv+"=1")
	return startOwnedChild(t, cmd)
}

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("identity checks read argv via ps(1); Windows keeps the flock guard only")
	}
}

// holdDaemonLock makes a town whose daemon.lock is held (as by a running
// daemon) and whose daemon.pid names pid.
func holdDaemonLock(t *testing.T, pid int) string {
	t.Helper()
	townRoot := t.TempDir()
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
	return townRoot
}

func TestIsGTDaemonArgs(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"/Users/x/.local/bin/gt", "daemon", "run"}, true},
		{[]string{"gt", "daemon", "run", "--verbose"}, true},
		{[]string{"/Applications/Docker.app/Contents/MacOS/com.docker.backend", "services"}, false},
		{[]string{"gt", "daemon", "stop"}, false},
		{[]string{"sleep", "60"}, false},
		{nil, false},
	}
	for _, c := range cases {
		if got := isGTDaemonArgs(c.args); got != c.want {
			t.Errorf("isGTDaemonArgs(%q) = %v, want %v", c.args, got, c.want)
		}
	}
}

func TestVerifyGTDaemonPIDRefusesWhatItCannotRead(t *testing.T) {
	skipOnWindows(t)
	orig := processArgsFn
	t.Cleanup(func() { processArgsFn = orig })

	processArgsFn = func(int) []string { return nil }
	if err := verifyGTDaemonPID(4242); err == nil {
		t.Error("an unreadable command line was accepted as the daemon")
	}
	processArgsFn = func(int) []string {
		return []string{"/Applications/Docker.app/Contents/MacOS/com.docker.backend"}
	}
	if err := verifyGTDaemonPID(4242); err == nil || !strings.Contains(err.Error(), "not the gt daemon") {
		t.Errorf("Docker's backend accepted as the daemon: %v", err)
	}
	processArgsFn = func(int) []string { return []string{"gt", "daemon", "run"} }
	if err := verifyGTDaemonPID(4242); err != nil {
		t.Errorf("gt daemon run rejected: %v", err)
	}
	if err := verifyGTDaemonPID(os.Getpid()); err == nil {
		t.Error("this process accepted as the daemon")
	}
	if err := verifyGTDaemonPID(0); err == nil {
		t.Error("PID 0 accepted")
	}
}

// A daemon.pid naming an unrelated live process (the gt-p7zy0 shape: a
// stale or foreign PID) must not be signaled, and nothing is cleaned up
// while the lock is held.
func TestStopDaemonDoesNotSignalUnrelatedProcess(t *testing.T) {
	skipOnWindows(t)
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
func TestStopDaemonSignalsVerifiedDaemon(t *testing.T) {
	skipOnWindows(t)
	daemonProc := startImpersonator(t, "gt", "daemon", "run")
	townRoot := holdDaemonLock(t, daemonProc.pid())

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

func newStopTestManager(t *testing.T, pid int) (*DoltServerManager, *strings.Builder) {
	t.Helper()
	var logs strings.Builder
	var mu sync.Mutex
	m := &DoltServerManager{
		config:   &DoltServerConfig{Enabled: true, Port: 13399, Host: "127.0.0.1"},
		townRoot: t.TempDir(),
		logger: func(format string, v ...interface{}) {
			mu.Lock()
			defer mu.Unlock()
			fmt.Fprintf(&logs, format+"\n", v...)
		},
		runningFn: func() (int, bool) { return pid, true },
	}
	return m, &logs
}

// The Dolt manager's stop path signals the PID isRunning reports; that PID
// comes from a pid file plus "something answers on the port". An unrelated
// process in that slot must survive.
func TestDoltStopDoesNotSignalUnrelatedProcess(t *testing.T) {
	skipOnWindows(t)
	victim := startUnrelatedSleep(t)
	m, logs := newStopTestManager(t, victim.pid())
	if err := os.MkdirAll(filepath.Join(m.townRoot, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := writePIDFile(m.pidFile(), victim.pid()); err != nil {
		t.Fatal(err)
	}

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if victim.exited(700 * time.Millisecond) {
		t.Fatalf("Dolt stop signaled an unrelated process (pid %d): %v", victim.pid(), victim.err)
	}
	if !strings.Contains(logs.String(), "Not stopping PID") {
		t.Errorf("refusal not logged: %q", logs.String())
	}
	// The pid file names a process that is provably not dolt: stale (F8).
	if _, err := os.Stat(m.pidFile()); !os.IsNotExist(err) {
		t.Errorf("stale pid file naming a non-dolt process was kept: %v", err)
	}
}

// When ps cannot read the PID's argv nothing is known about it: no signal,
// and the pid file stays for a later attempt.
func TestDoltStopKeepsPIDFileWhenIdentityUnverified(t *testing.T) {
	skipOnWindows(t)
	victim := startUnrelatedSleep(t)
	m, _ := newStopTestManager(t, victim.pid())
	if err := os.MkdirAll(filepath.Join(m.townRoot, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := writePIDFile(m.pidFile(), victim.pid()); err != nil {
		t.Fatal(err)
	}
	orig := verifyDoltSQLServerFn
	t.Cleanup(func() { verifyDoltSQLServerFn = orig })
	verifyDoltSQLServerFn = func(pid int) error {
		return fmt.Errorf("ps failed: %w", doltserver.ErrIdentityUnverified)
	}

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if victim.exited(300 * time.Millisecond) {
		t.Fatalf("unverified PID was signaled: %v", victim.err)
	}
	if _, err := os.Stat(m.pidFile()); err != nil {
		t.Errorf("pid file removed although identity was only unverified: %v", err)
	}
}

func TestDoltStopSignalsVerifiedDoltServer(t *testing.T) {
	skipOnWindows(t)
	server := startImpersonator(t, "dolt", "sql-server", "--config", "/nonexistent/config.yaml")
	m, logs := newStopTestManager(t, server.pid())

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

// The chain that SIGTERMed Docker Desktop (gt-p7zy0): an identity-check
// failure in EnsureRunning called doltserver.KillImposters, which signals
// whatever holds GT_DOLT_PORT — under the hermetic harness a Docker
// container port, held on the host by com.docker.backend. Test managers
// route that call through the seam.
func TestEnsureRunningIdentityFailureUsesKillImpostersSeam(t *testing.T) {
	m := newTestManager(t)
	m.runningFn = func() (int, bool) { return 1234, true }
	m.identityCheckFn = func() error { return errors.New("imposter") }
	calls := 0
	m.killImpostersFn = func() error { calls++; return nil }
	m.sleepFn = func(time.Duration) {}

	if err := m.EnsureRunning(); err != nil {
		t.Fatalf("EnsureRunning: %v", err)
	}
	if calls != 1 {
		t.Fatalf("killImposters seam calls = %d, want 1", calls)
	}
}

// newTestManager must never fall through to the real KillImposters.
func TestNewTestManagerStubsKillImposters(t *testing.T) {
	if newTestManager(t).killImpostersFn == nil {
		t.Fatal("newTestManager leaves killImpostersFn nil: an identity failure would reach doltserver.KillImposters")
	}
}
