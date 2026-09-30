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

func TestIsGTDaemonArgs(t *testing.T) {
	t.Parallel()
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
	townRoot := t.TempDir()
	origArgs, origCWD := processArgsFn, processCWDFn
	t.Cleanup(func() { processArgsFn, processCWDFn = origArgs, origCWD })
	processCWDFn = func(int) string { return townRoot }

	processArgsFn = func(int) []string { return nil }
	if err := verifyGTDaemonPID(townRoot, 4242); err == nil {
		t.Error("an unreadable command line was accepted as the daemon")
	}
	processArgsFn = func(int) []string {
		return []string{"/Applications/Docker.app/Contents/MacOS/com.docker.backend"}
	}
	if err := verifyGTDaemonPID(townRoot, 4242); err == nil || !strings.Contains(err.Error(), "not the gt daemon") {
		t.Errorf("Docker's backend accepted as the daemon: %v", err)
	}
	processArgsFn = func(int) []string { return []string{"gt", "daemon", "run"} }
	if err := verifyGTDaemonPID(townRoot, 4242); err != nil {
		t.Errorf("gt daemon run rejected: %v", err)
	}
	if err := verifyGTDaemonPID(townRoot, os.Getpid()); err == nil {
		t.Error("this process accepted as the daemon")
	}
	if err := verifyGTDaemonPID(townRoot, 0); err == nil {
		t.Error("PID 0 accepted")
	}
}

// gt-l9s6f: `gt daemon run` names no town in its argv; the town is the
// daemon's working directory. A daemon in another town — or one whose cwd
// cannot be read — is not this town's to signal.
func TestVerifyGTDaemonPIDIsTownScoped(t *testing.T) {
	skipOnWindows(t)
	parent := t.TempDir()
	townRoot := filepath.Join(parent, "town")
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A sibling whose name extends the town's: a string-prefix check would
	// call it inside the town.
	prefixSibling := townRoot + "-two"
	link := filepath.Join(parent, "link")
	if err := os.MkdirAll(prefixSibling, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(townRoot, link); err != nil {
		t.Fatal(err)
	}

	origArgs, origCWD := processArgsFn, processCWDFn
	t.Cleanup(func() { processArgsFn, processCWDFn = origArgs, origCWD })
	processArgsFn = func(int) []string { return []string{"gt", "daemon", "run"} }

	cases := []struct {
		name     string
		townRoot string
		cwd      string
		wantErr  string // "" = accepted
	}{
		{"town root", townRoot, townRoot, ""},
		{"below the town root", townRoot, filepath.Join(townRoot, "mayor"), ""},
		{"town root spelled through a symlink", link, townRoot, ""},
		{"another town", townRoot, t.TempDir(), "another town"},
		{"prefix sibling", townRoot, prefixSibling, "another town"},
		{"parent of the town", townRoot, parent, "another town"},
		{"cwd unreadable", townRoot, "", "unreadable"},
	}
	for _, c := range cases {
		processCWDFn = func(int) string { return c.cwd }
		err := verifyGTDaemonPID(c.townRoot, 4242)
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: rejected: %v", c.name, err)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("%s: err = %v, want one mentioning %q", c.name, err, c.wantErr)
		}
	}
}

// A live `gt daemon run` in ANOTHER town, named by this town's daemon.pid (a
// reused PID), must survive StopDaemon.
func TestStopDaemonDoesNotSignalOtherTownsDaemon(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)
	foreign := startImpersonatorIn(t, t.TempDir(), "gt", "daemon", "run")
	townRoot := holdDaemonLock(t, foreign.pid())

	err := StopDaemon(townRoot)
	if err == nil || !strings.Contains(err.Error(), "another town") {
		t.Fatalf("StopDaemon = %v, want a refusal naming another town", err)
	}
	if foreign.exited(700 * time.Millisecond) {
		t.Fatalf("StopDaemon signaled another town's daemon (pid %d): %v", foreign.pid(), foreign.err)
	}
	if _, statErr := os.Stat(filepath.Join(townRoot, "daemon", "daemon.pid")); statErr != nil {
		t.Errorf("refusal removed daemon.pid: %v", statErr)
	}
}

// A daemon.pid naming an unrelated live process (the gt-p7zy0 shape: a
// stale or foreign PID) must not be signaled, and nothing is cleaned up
// while the lock is held.
func TestStopDaemonDoesNotSignalUnrelatedProcess(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	skipOnWindows(t)
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
	t.Parallel()
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
	verifyDoltSQLServerFn = func(string, int) error {
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
	t.Parallel()
	skipOnWindows(t)
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

// gt-l9s6f: a dolt sql-server serving ANOTHER town's data dir passes the
// "is dolt" check, but a pid file naming it is stale for this town: the
// manager must not signal it, and drops the pid file.
func TestDoltStopDoesNotSignalOtherTownsDolt(t *testing.T) {
	t.Parallel()
	skipOnWindows(t)
	m, logs := newStopTestManager(t, 0)
	otherTown := t.TempDir()
	foreign := startImpersonator(t, "dolt", "sql-server", "--config", townDoltConfig(otherTown))
	m.runningFn = func() (int, bool) { return foreign.pid(), true }
	if err := os.MkdirAll(filepath.Join(m.townRoot, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := writePIDFile(m.pidFile(), foreign.pid()); err != nil {
		t.Fatal(err)
	}

	if err := m.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if foreign.exited(700 * time.Millisecond) {
		t.Fatalf("Dolt stop signaled another town's dolt (pid %d): %v", foreign.pid(), foreign.err)
	}
	if !strings.Contains(logs.String(), "Not stopping PID") {
		t.Errorf("refusal not logged: %q", logs.String())
	}
	if _, err := os.Stat(m.pidFile()); !os.IsNotExist(err) {
		t.Errorf("stale pid file naming another town's dolt was kept: %v", err)
	}
}

// The chain that SIGTERMed Docker Desktop (gt-p7zy0): an identity-check
// failure in EnsureRunning called doltserver.KillImposters, which signals
// whatever holds GT_DOLT_PORT — under the hermetic harness a Docker
// container port, held on the host by com.docker.backend. Test managers
// route that call through the seam.
func TestEnsureRunningIdentityFailureUsesKillImpostersSeam(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	if newTestManager(t).killImpostersFn == nil {
		t.Fatal("newTestManager leaves killImpostersFn nil: an identity failure would reach doltserver.KillImposters")
	}
}
