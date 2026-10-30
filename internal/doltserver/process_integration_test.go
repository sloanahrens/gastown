//go:build integration

package doltserver

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The unit tier runs the adapter on a fakeHost, whose process table stands in
// for ps and lsof. These run it on the real machine: what ps and lsof (or ss,
// or /proc) report for real processes, and a real dolt sql-server's life.
// Every process here is one the test started itself, on its own port and
// directories: nothing touches the town's Dolt server.

// portHolderHelperEnv is shared with testmain_integration_test.go, whose
// TestMain runs the helper body before the harness starts.
const portHolderHelperEnv = "DOLTSERVER_TEST_PORT_HOLDER"

// started is a child the test started, reaped and killed at cleanup.
type started struct {
	cmd  *exec.Cmd
	done chan struct{}
}

func (s *started) pid() int { return s.cmd.Process.Pid }

func (s *started) exited(within time.Duration) bool {
	select {
	case <-s.done:
		return true
	case <-time.After(within):
		return false
	}
}

func startChild(t *testing.T, cmd *exec.Cmd) *started {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %v: %v", cmd.Args, err)
	}
	s := &started{cmd: cmd, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(s.done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill() // our own child
		<-s.done
	})
	return s
}

// startPortHolder re-executes this test binary as a non-dolt process that
// listens on a loopback port, with its cwd at dir. Returns it and the port.
func startPortHolder(t *testing.T, dir string) (*started, int) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), portHolderHelperEnv+"=1")
	cmd.Dir = dir
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	s := startChild(t, cmd)
	portc := make(chan int, 1)
	go func() {
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			if p, ok := strings.CutPrefix(sc.Text(), "PORT="); ok {
				n, _ := strconv.Atoi(p)
				portc <- n
				return
			}
		}
		close(portc)
	}()
	select {
	case port, ok := <-portc:
		if !ok || port == 0 {
			t.Fatal("port holder did not report a port")
		}
		return s, port
	case <-time.After(30 * time.Second):
		t.Fatal("port holder did not start")
	}
	return nil, 0
}

func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// testTownRoot is a fresh temp town root carrying the routes file rig init
// reads.
func testTownRoot(t *testing.T) string {
	t.Helper()
	townRoot := resolvedTempDir(t)
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(`{"prefix":"tr-","path":"testrig/mayor/rig"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return townRoot
}

// ps and lsof (or /proc) read a real process's argv, cwd and listening port
// the way the identity checks expect, and a non-dolt port holder is never
// signaled (gt-p7zy0).
func TestIntegrationProcessIdentity(t *testing.T) {
	dir := resolvedTempDir(t)
	sleep := startChild(t, func() *exec.Cmd { c := exec.Command("sleep", "60"); c.Dir = dir; return c }())

	if got := std.ProcessArgs(sleep.pid()); !slices.Equal(got, []string{"sleep", "60"}) {
		t.Errorf("ProcessArgs = %q, want [sleep 60]", got)
	}
	if got := std.ProcessCWD(sleep.pid()); got != dir {
		t.Errorf("ProcessCWD = %q, want %q", got, dir)
	}
	if err := std.VerifyDoltSQLServerPID(sleep.pid()); !errors.Is(err, ErrNotDoltSQLServer) {
		t.Errorf("VerifyDoltSQLServerPID(sleep) = %v, want ErrNotDoltSQLServer", err)
	}

	townRoot := resolvedTempDir(t)
	holder, port := startPortHolder(t, resolvedTempDir(t))
	h := realHostOnPort(port)
	if got := h.findDoltServerOnPort(port); got != holder.pid() {
		t.Fatalf("findDoltServerOnPort(%d) = %d, want the holder %d (lsof or ss must name it)", port, got, holder.pid())
	}
	if err := h.KillImposters(townRoot); err != nil {
		t.Errorf("KillImposters = %v, want nil (skip with a warning)", err)
	}
	if got := h.evictPortSquatter(port); got != 0 {
		t.Errorf("evictPortSquatter acted on PID %d", got)
	}
	if holder.exited(700 * time.Millisecond) {
		t.Fatal("a non-dolt port holder was signaled")
	}
	if err := h.checkPortAvailable(port); err == nil || !strings.Contains(err.Error(), "held by PID "+strconv.Itoa(holder.pid())) {
		t.Errorf("checkPortAvailable on the held port = %v, want the holder named", err)
	}
}

// startProcess reaps the child it starts, so processAlive goes false as soon
// as the child exits. Without the reap the child stays a zombie, which
// signal(0) answers for as long as it exists (gt-fpunm).
func TestIntegrationStartProcessReapsExitedChild(t *testing.T) {
	proc, err := std.startProcess(exec.Command("false"))
	if err != nil {
		t.Fatalf("startProcess: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && std.processAlive(proc.pid) {
		time.Sleep(5 * time.Millisecond)
	}
	if std.processAlive(proc.pid) {
		t.Fatalf("processAlive(%d) stayed true for 1s after the child exited: an unreaped zombie", proc.pid)
	}
	// cmd.Wait reaps the pid before the reaper closes done, so the pid can read
	// as gone while the handle still reads as running. Wait for the close, with a
	// bound, before asking the handle.
	select {
	case <-proc.done:
	case <-time.After(time.Second):
		t.Fatalf("proc.done stayed open for 1s after the child exited")
	}
	// The handle answers the same way every time it is asked.
	for i := 0; i < 2; i++ {
		if !proc.exited() {
			t.Fatalf("exited() call %d = false after the reap", i+1)
		}
		if err := proc.waitErr(); err == nil {
			t.Fatalf("waitErr() call %d = nil, want the child's exit status", i+1)
		}
	}
}

// freePort returns a loopback port nothing listens on.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// freePorts is freePort as startOnFreePort's port supplier.
func freePorts(t *testing.T) func() int {
	return func() int { return freePort(t) }
}

// The retry against a port something really holds: the first start is refused
// for the port it was handed, and the start comes back on a second one
// (gt-8dbyw). The start here is the adapter's own preflight, which binds the
// port — the refusal dolt's bind triggers, without a dolt server in the
// middle.
func TestIntegrationStartOnFreePortRetriesOnABoundPort(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	heldPort := held.Addr().(*net.TCPAddr).Port

	picks := 0
	var refused error
	h, err := startOnFreePort(t,
		func() int {
			picks++
			if picks == 1 {
				return heldPort
			}
			return freePort(t)
		},
		func(h *host) error {
			err := h.checkPortAvailable(doltPort(t, h))
			if err != nil && refused == nil {
				refused = err
			}
			return err
		})
	if err != nil {
		t.Fatalf("start on a fresh port: %v", err)
	}
	if refused == nil {
		t.Fatalf("the first start on the bound port %d was not refused", heldPort)
	}
	if !portRaceErr(refused) {
		t.Errorf("the first start refused the bound port with %q, want the port-in-use answer", refused)
	}
	if got := doltPort(t, h); got == heldPort {
		t.Errorf("the start came back on the bound port %d", heldPort)
	}
}

// A real dolt sql-server started, found running, and stopped by the adapter,
// in a temporary town on its own port. InitRig on that stopped server makes
// the database and seeds issue_prefix through a temporary server.
//
// Every start here is a startOnFreePort: the port it is handed was free when
// it was picked and can be taken before dolt binds it (gt-8dbyw). A retried
// rig init gets a fresh town, so the assertions run against one from-scratch
// init either way.
func TestIntegrationDoltServerLifecycle(t *testing.T) {
	if _, err := exec.LookPath("dolt"); err != nil {
		t.Skip("dolt is not installed")
	}
	var townRoot string
	h, err := startOnFreePort(t, freePorts(t), func(h *host) error {
		town := testTownRoot(t)
		townRoot = town
		t.Cleanup(func() { _, _ = h.ReapOwnedTestServers(town) })
		// dolt init needs an identity; the harness's HOME is a fresh sandbox.
		for _, kv := range [][2]string{{"user.name", "Test"}, {"user.email", "test@example.com"}} {
			if _, err := h.doltConfigMissing(kv[0]); err != nil {
				t.Fatalf("probing dolt %s: %v", kv[0], err)
			}
			if err := h.setDoltGlobalConfig(kv[0], kv[1]); err != nil {
				t.Fatalf("setting dolt %s in the sandbox HOME: %v", kv[0], err)
			}
		}
		result, err := h.InitRig(town, "testrig")
		if err != nil {
			return fmt.Errorf("InitRig: %w", err)
		}
		if !result.Created {
			t.Fatal("InitRig created = false, want true")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("initializing the test rig: %v", err)
	}
	t.Cleanup(func() { _, _ = h.ReapOwnedTestServers(townRoot) })
	if running, _, _ := h.IsRunning(townRoot); running {
		t.Fatal("InitRig left its temporary server running")
	}
	query := exec.Command("dolt", "sql", "-q", "SELECT value FROM config WHERE `key` = 'issue_prefix'")
	query.Dir = h.RigDatabaseDir(townRoot, "testrig")
	if out, err := query.CombinedOutput(); err != nil || !strings.Contains(string(out), "tr") {
		t.Errorf("issue_prefix after InitRig: %v\n%s", err, out)
	}

	h, err = startOnFreePort(t, freePorts(t), func(h *host) error { return h.Start(townRoot) })
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	running, pid, err := h.IsRunning(townRoot)
	if err != nil || !running || pid <= 0 {
		t.Fatalf("IsRunning after Start = %v, %d, %v", running, pid, err)
	}
	if err := h.VerifyTownDoltSQLServerPID(townRoot, pid); err != nil {
		t.Errorf("the started server does not verify as this town's: %v", err)
	}
	if served, missing, err := h.VerifyDatabases(townRoot); err != nil || len(missing) != 0 || !slices.Contains(served, "testrig") {
		t.Errorf("VerifyDatabases = %v, missing %v, %v; want testrig served", served, missing, err)
	}
	// The commits-per-day meter's query against a real dolt_log: InitRig's
	// commits were all made in the last 24h.
	counts, err := h.CommitsLastDay(context.Background(), townRoot)
	if err != nil {
		t.Fatalf("CommitsLastDay: %v", err)
	}
	if i := slices.IndexFunc(counts, func(c DBCommits) bool { return c.Database == "testrig" }); i < 0 || counts[i].Err != "" || counts[i].Commits < 1 {
		t.Errorf("CommitsLastDay = %+v, want testrig with at least one commit", counts)
	}
	if err := h.Start(townRoot); err != nil {
		t.Errorf("a second Start of a running server = %v, want idempotent success", err)
	}
	if err := h.Stop(townRoot); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if running, _, _ := h.IsRunning(townRoot); running {
		t.Error("IsRunning after Stop = true")
	}
}
