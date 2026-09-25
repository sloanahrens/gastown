package doltserver

import (
	"bufio"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// gt-p7zy0: a port holder or pid-file PID is only a candidate; nothing is
// signaled until its argv shows a dolt sql-server. Every test here points the
// code under test only at processes it spawned itself — this test binary
// re-executed as a port-holding helper, or `sleep` — so even with the guard
// removed a regression can only kill the test's own child, never the test
// binary or a host process.

// portHolderHelperEnv is shared with hermetic_main_test.go, whose TestMain
// runs the helper body before the harness starts.
const portHolderHelperEnv = "DOLTSERVER_TEST_PORT_HOLDER" // not GT_*: the hermetic harness scrubs those

type child struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
}

func (c *child) pid() int { return c.cmd.Process.Pid }

func (c *child) alive() bool {
	select {
	case <-c.done:
		return false
	default:
		return true
	}
}

func (c *child) exited(within time.Duration) bool {
	select {
	case <-c.done:
		return true
	case <-time.After(within):
		return false
	}
}

func watchChild(t *testing.T, cmd *exec.Cmd) *child {
	t.Helper()
	c := &child{cmd: cmd, done: make(chan struct{})}
	var once sync.Once
	go func() {
		c.err = cmd.Wait()
		once.Do(func() { close(c.done) })
	}()
	t.Cleanup(func() {
		if c.alive() {
			_ = cmd.Process.Kill() // our own child
		}
		<-c.done
	})
	return c
}

// startPortHolder re-executes this test binary as a non-dolt process that
// listens on a loopback port, with its cwd at dir ("" = inherit). Returns the
// child and the port it holds.
func startPortHolder(t *testing.T, dir string) (*child, int) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	cmd.Env = append(os.Environ(), portHolderHelperEnv+"=1")
	cmd.Dir = dir
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start port holder: %v", err)
	}
	c := watchChild(t, cmd)
	portc := make(chan int, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if p, ok := strings.CutPrefix(sc.Text(), "PORT="); ok {
				n, _ := strconv.Atoi(p)
				portc <- n
			}
		}
		close(portc)
	}()
	select {
	case port, ok := <-portc:
		if !ok || port == 0 {
			<-c.done
			t.Fatalf("port holder did not report a port: %v\n%s", c.err, stderr.String())
		}
		if findDoltServerOnPort(port) != c.pid() {
			t.Skip("neither lsof nor ss names the port holder here")
		}
		return c, port
	case <-time.After(30 * time.Second):
		t.Fatal("port holder did not start")
	}
	return nil, 0
}

func startSleep(t *testing.T) *child {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return watchChild(t, cmd)
}

// resolvedTempDir is t.TempDir with symlinks resolved, so it compares equal
// to the cwd lsof reports (/var -> /private/var on macOS).
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// closedPort returns a loopback port nothing listens on.
func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no argv check on Windows")
	}
}

func stubArgs(t *testing.T, fn func(int) []string) {
	t.Helper()
	orig := processArgsForIdentity
	t.Cleanup(func() { processArgsForIdentity = orig })
	processArgsForIdentity = fn
}

func TestIsDoltSQLServerArgsAcceptsGlobalFlagsAndRejectsDocker(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"/opt/homebrew/bin/dolt", "sql-server", "--config", "/t/.dolt-data/config.yaml"}, true},
		{[]string{"dolt", "--data-dir", "/x", "sql-server"}, true},
		{[]string{"/Applications/Docker.app/Contents/MacOS/com.docker.backend", "services"}, false},
		{[]string{"dolt", "sql"}, false},
		{[]string{"/bin/sleep", "sql-server"}, false},
		{nil, false},
	}
	for _, c := range cases {
		if got := isDoltSQLServerArgs(c.args); got != c.want {
			t.Errorf("isDoltSQLServerArgs(%q) = %v, want %v", c.args, got, c.want)
		}
	}
}

func TestVerifyDoltSQLServerPID(t *testing.T) {
	skipOnWindows(t)
	stubArgs(t, func(int) []string {
		return []string{"/Applications/Docker.app/Contents/MacOS/com.docker.backend", "services"}
	})
	if err := VerifyDoltSQLServerPID(4242); !errors.Is(err, ErrNotDoltSQLServer) {
		t.Errorf("Docker's port forwarder: %v, want ErrNotDoltSQLServer", err)
	}
	stubArgs(t, func(int) []string { return nil })
	if err := VerifyDoltSQLServerPID(4242); !errors.Is(err, ErrIdentityUnverified) {
		t.Errorf("unreadable argv: %v, want ErrIdentityUnverified", err)
	}
	stubArgs(t, func(int) []string { return []string{"dolt", "sql-server"} })
	if err := VerifyDoltSQLServerPID(4242); err != nil {
		t.Errorf("dolt sql-server rejected: %v", err)
	}
	if err := VerifyDoltSQLServerPID(os.Getpid()); err == nil {
		t.Error("this process accepted as dolt")
	}
	if err := VerifyDoltSQLServerPID(0); err == nil {
		t.Error("PID 0 accepted")
	}
}

// A non-dolt process holding the Dolt port (com.docker.backend in the gate)
// is skipped with a warning: no signal, and no error for `gt down` to report.
func TestKillImpostersSkipsNonDoltPortHolder(t *testing.T) {
	skipOnWindows(t)
	holder, port := startPortHolder(t, resolvedTempDir(t))
	t.Setenv("GT_DOLT_PORT", strconv.Itoa(port))

	if err := KillImposters(resolvedTempDir(t)); err != nil {
		t.Errorf("KillImposters = %v, want nil (skip with a warning)", err)
	}
	if holder.exited(700 * time.Millisecond) {
		t.Fatalf("KillImposters signaled a non-dolt port holder: %v", holder.err)
	}
}

// The town's own server is recognized before any identity check, so ps being
// unable to read its argv is not an error either.
func TestKillImpostersOwnServerWithUnreadableArgvIsNotAnError(t *testing.T) {
	skipOnWindows(t)
	townRoot := resolvedTempDir(t)
	holder, port := startPortHolder(t, townRoot) // cwd = town root: ours
	t.Setenv("GT_DOLT_PORT", strconv.Itoa(port))
	stubArgs(t, func(int) []string { return nil })

	if err := KillImposters(townRoot); err != nil {
		t.Errorf("KillImposters = %v, want nil", err)
	}
	if holder.exited(300 * time.Millisecond) {
		t.Fatalf("own server was signaled: %v", holder.err)
	}
}

// Stop refuses a pid-file PID that is not dolt, even when IsRunning claims
// it for the town (here: cwd is the town root and it answers on the port).
func TestStopRefusesNonDoltPID(t *testing.T) {
	skipOnWindows(t)
	townRoot := resolvedTempDir(t)
	holder, port := startPortHolder(t, townRoot)
	t.Setenv("GT_DOLT_PORT", strconv.Itoa(port))
	cfg := DefaultConfig(townRoot)
	if err := os.MkdirAll(filepath.Dir(cfg.PidFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.PidFile, []byte(strconv.Itoa(holder.pid())), 0o644); err != nil {
		t.Fatal(err)
	}
	if running, pid, _ := IsRunning(townRoot); !running || pid != holder.pid() {
		t.Skipf("IsRunning = %v/%d; setup did not reach the pid-file branch", running, pid)
	}

	err := Stop(townRoot)
	if !errors.Is(err, ErrNotDoltSQLServer) {
		t.Errorf("Stop = %v, want a refusal wrapping ErrNotDoltSQLServer", err)
	}
	if holder.exited(700 * time.Millisecond) {
		t.Fatalf("Stop signaled a non-dolt PID: %v", holder.err)
	}
}

// Stop refuses when the server is only reachable over TCP with no local
// process it can name (a Docker port forward).
func TestStopRefusesReachableServerWithoutLocalPID(t *testing.T) {
	skipOnWindows(t)
	holder, port := startPortHolder(t, resolvedTempDir(t)) // not the town's
	t.Setenv("GT_DOLT_PORT", strconv.Itoa(port))
	townRoot := resolvedTempDir(t)
	if running, pid, _ := IsRunning(townRoot); !running || pid != 0 {
		t.Skipf("IsRunning = %v/%d; setup did not reach the TCP-only branch", running, pid)
	}

	err := Stop(townRoot)
	if err == nil || !strings.Contains(err.Error(), "no verifiable local process") {
		t.Errorf("Stop = %v, want a refusal", err)
	}
	if holder.exited(300 * time.Millisecond) {
		t.Fatalf("Stop signaled the port holder: %v", holder.err)
	}
}

// F1: the orphaned-server fallback after a failed Stop force-kills only a
// verified dolt. The PID here is a live non-dolt port holder that IsRunning
// claims for the town (cwd = town root, answers on the port), so Stop refuses
// without touching the pid file and IsRunning keeps it: the pid file removal
// asserted below can only come from stopOrphanedServer itself.
func TestStopOrphanedServerNeverKillsNonDolt(t *testing.T) {
	skipOnWindows(t)
	townRoot := resolvedTempDir(t)
	holder, port := startPortHolder(t, townRoot)
	t.Setenv("GT_DOLT_PORT", strconv.Itoa(port))
	cfg := DefaultConfig(townRoot)
	if err := os.MkdirAll(filepath.Dir(cfg.PidFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.PidFile, []byte(strconv.Itoa(holder.pid())), 0o644); err != nil {
		t.Fatal(err)
	}
	if running, pid, _ := IsRunning(townRoot); !running || pid != holder.pid() {
		t.Fatalf("IsRunning = %v/%d; setup did not reach the pid-file branch", running, pid)
	}
	if err := Stop(townRoot); !errors.Is(err, ErrNotDoltSQLServer) {
		t.Fatalf("Stop = %v, want a refusal", err)
	}
	if _, err := os.Stat(cfg.PidFile); err != nil {
		t.Fatalf("precondition: Stop's refusal must leave the pid file: %v", err)
	}

	stopOrphanedServer(townRoot, holder.pid())

	if holder.exited(500 * time.Millisecond) {
		t.Fatalf("orphan fallback killed a non-dolt PID: %v", holder.err)
	}
	if _, err := os.Stat(cfg.PidFile); !os.IsNotExist(err) {
		t.Errorf("stopOrphanedServer kept a pid file naming a non-dolt process: %v", err)
	}
}

func TestStopOrphanedServerKillsVerifiedDolt(t *testing.T) {
	skipOnWindows(t)
	t.Setenv("GT_DOLT_PORT", strconv.Itoa(closedPort(t)))
	townRoot := resolvedTempDir(t)
	victim := startSleep(t)
	stubArgs(t, func(pid int) []string {
		if pid == victim.pid() {
			return []string{"dolt", "sql-server"}
		}
		return nil
	})

	stopOrphanedServer(townRoot, victim.pid())

	if !victim.exited(5 * time.Second) {
		t.Fatal("verified orphaned dolt was not killed")
	}
	var exitErr *exec.ExitError
	if !errors.As(victim.err, &exitErr) || exitErr.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Errorf("orphan ended by %v, want SIGKILL", victim.err)
	}
}

// F9: Start's squatter eviction leaves a non-dolt port holder alone.
func TestEvictPortSquatterLeavesNonDoltHolder(t *testing.T) {
	skipOnWindows(t)
	holder, port := startPortHolder(t, resolvedTempDir(t))

	if got := evictPortSquatter(port); got != 0 {
		t.Errorf("evictPortSquatter acted on PID %d, want 0", got)
	}
	if holder.exited(500 * time.Millisecond) {
		t.Fatalf("squatter eviction killed a non-dolt holder: %v", holder.err)
	}
}

func TestEvictPortSquatterKillsVerifiedDolt(t *testing.T) {
	skipOnWindows(t)
	holder, port := startPortHolder(t, resolvedTempDir(t))
	stubArgs(t, func(pid int) []string {
		if pid == holder.pid() {
			return []string{"dolt", "sql-server"}
		}
		return nil
	})

	if got := evictPortSquatter(port); got != holder.pid() {
		t.Errorf("evictPortSquatter = %d, want %d", got, holder.pid())
	}
	if !holder.exited(5 * time.Second) {
		t.Fatal("verified dolt squatter was not killed")
	}
}
