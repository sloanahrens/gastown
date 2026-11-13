package doltserver

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// captureStderr runs fn and returns what it wrote to os.Stderr. A test that
// uses it does not run in parallel: os.Stderr is process-wide.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w //testpolicy:allow no-global-swap — os.Stderr is where the warnings are written; the test asserts them
	func() {
		defer func() { os.Stderr = old }() //testpolicy:allow no-global-swap — restores the os.Stderr swapped above
		fn()
	}()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// initRigOrphanHost is the host for an InitRig whose town has no data
// directory, with the issue_prefix seams stubbed so the rig's registration
// does not reach a real bd.
func initRigOrphanHost(f *fakeHost) *host {
	h := f.host()
	h.readIssuePrefix = func(string, string) (string, error) { return "", nil }
	h.writeIssuePrefix = func(_, _, _, _ string) error { return nil }
	return h
}

// A server reachable over TCP that gt does not own (pid 0, a container port
// forward) with no data directory is not an orphan. InitRig must not stop it,
// and must register the rig with it over SQL: an offline init would make a
// directory the live server never reads (gt-rfxs85).
func TestInitRig_UnownedServerWithoutDataDirIsNotStopped(t *testing.T) {
	t.Parallel()
	const port = 4560
	f := withDoltIdentity(newFakeHost().townPort(port)).on("dolt *", fakeReply{}).on("dolt init", fakeReply{})
	f.reachable[fmt.Sprintf("127.0.0.1:%d", port)] = true
	h := initRigOrphanHost(f)
	townRoot := testTown(t)

	result, err := h.InitRig(townRoot, "forkrig")
	if err != nil {
		t.Fatalf("InitRig: %v", err)
	}
	if !result.ServerWasRunning {
		t.Errorf("ServerWasRunning = false, want true: the reachable server is the live one")
	}
	if got := f.ranMatching("CREATE DATABASE `forkrig`"); len(got) != 1 {
		t.Errorf("CREATE DATABASE commands = %q, want one on the live server", got)
	}
	if got := f.ranMatching("dolt init"); len(got) != 0 {
		t.Errorf("offline dolt init ran: %q", got)
	}
	if len(f.signals) != 0 {
		t.Errorf("signals sent = %v, want none: gt does not own this server", f.signals)
	}
	if _, err := os.Stat(RigDatabaseDir(townRoot, "forkrig")); !os.IsNotExist(err) {
		t.Errorf("offline database directory exists (stat err %v), want it absent", err)
	}
}

// A server gt owns (a local PID) whose data directory is gone is an orphan: it
// is stopped, and the rig is initialised offline.
func TestInitRig_OwnedOrphanThatStopsGoesOffline(t *testing.T) {
	t.Parallel()
	const port = 4561
	f := withDoltIdentity(newFakeHost().townPort(port)).on("dolt *", fakeReply{}).on("dolt init", fakeReply{})
	f.onStart = func(pid int, _ *exec.Cmd) { f.listenOn(pid, port) } // the temporary server comes up on its port
	townRoot := testTown(t)
	pid := f.doltServer(townRoot, port)
	h := initRigOrphanHost(f)

	result, err := h.InitRig(townRoot, "forkrig")
	if err != nil {
		t.Fatalf("InitRig: %v", err)
	}
	if result.ServerWasRunning {
		t.Errorf("ServerWasRunning = true, want false: the orphan was stopped")
	}
	if got := f.signalsTo(pid); len(got) == 0 || got[0] != syscall.SIGTERM {
		t.Errorf("signals to the owned server = %v, want SIGTERM first", got)
	}
	if got := f.ranMatching("CREATE DATABASE"); len(got) != 0 {
		t.Errorf("CREATE DATABASE ran on a stopped server: %q", got)
	}
	if got := f.ranMatching("dolt init"); len(got) != 1 {
		t.Errorf("dolt init calls = %q, want one", got)
	}
}

// An owned orphan that survives SIGTERM and SIGKILL is still running, so it is
// not treated as stopped: InitRig keeps it and registers the rig over SQL, and
// makes no offline directory next to a server that is still running. The
// warning says why the server was kept (gt-8oc5pa).
//
//testpolicy:allow parallel — captures the process-wide os.Stderr the warning is written to
func TestInitRig_OwnedOrphanThatStaysUpKeepsServerRunning(t *testing.T) {
	const port = 4562
	f := withDoltIdentity(newFakeHost().townPort(port)).on("dolt *", fakeReply{}).on("dolt init", fakeReply{})
	townRoot := testTown(t)
	pid := f.doltServer(townRoot, port)
	f.mu.Lock()
	f.procs[pid].ignoresTERM = true
	f.procs[pid].ignoresKILL = true
	f.mu.Unlock()
	h := initRigOrphanHost(f)

	var result InitRigResult
	var err error
	stderr := captureStderr(t, func() { result, err = h.InitRig(townRoot, "forkrig") })
	if err != nil {
		t.Fatalf("InitRig: %v", err)
	}
	if !result.ServerWasRunning {
		t.Errorf("ServerWasRunning = false, want true: the server did not stop")
	}
	if got := f.ranMatching("CREATE DATABASE `forkrig`"); len(got) != 1 {
		t.Errorf("CREATE DATABASE commands = %q, want one on the live server", got)
	}
	if got := f.ranMatching("dolt init"); len(got) != 0 {
		t.Errorf("offline dolt init ran beside a live server: %q", got)
	}
	if !f.alive(pid) {
		t.Errorf("the server was reported running but is not alive")
	}
	if strings.Contains(strings.Join(f.commands(), "\n"), "docker stop") {
		t.Errorf("docker stop ran for a server gt does not run in a container")
	}
	if !strings.Contains(stderr, "did not stop; creating the database on the live server") {
		t.Errorf("stderr = %q, want the warning that the orphan did not stop", stderr)
	}
}

// A live PID whose command line cannot be read is not provably this town's
// dolt: Stop refuses it and the forced kill is skipped, so the process and its
// pid file are left alone and the rig is registered on the live server
// (gt-8oc5pa).
//
//testpolicy:allow parallel — captures the process-wide os.Stderr the warning is written to
func TestInitRig_UnverifiableOrphanKillIsRefused(t *testing.T) {
	const port = 4565
	f := withDoltIdentity(newFakeHost().townPort(port)).on("dolt *", fakeReply{}).on("dolt init", fakeReply{})
	f.reachable[fmt.Sprintf("127.0.0.1:%d", port)] = true
	townRoot := testTown(t)
	h := initRigOrphanHost(f)
	dataDir := filepath.Join(townRoot, ".dolt-data")
	holder := f.spawn(fakeProc{}) // alive, no argv and no readable cwd
	pidFile := writePIDFile(t, h, townRoot, holder)
	if err := SaveState(townRoot, &State{PID: holder, Port: port, DataDir: dataDir}); err != nil {
		t.Fatal(err)
	}

	var result InitRigResult
	var err error
	stderr := captureStderr(t, func() { result, err = h.InitRig(townRoot, "forkrig") })
	if err != nil {
		t.Fatalf("InitRig: %v", err)
	}
	if !result.ServerWasRunning {
		t.Errorf("ServerWasRunning = false, want true: the PID was never shown to be this town's dolt")
	}
	if got := f.signalsTo(holder); len(got) != 0 {
		t.Errorf("signals to the unverifiable PID = %v, want none", got)
	}
	if _, err := os.Stat(pidFile); err != nil {
		t.Errorf("pid file removed for an unverified PID: %v", err)
	}
	if got := f.ranMatching("CREATE DATABASE `forkrig`"); len(got) != 1 {
		t.Errorf("CREATE DATABASE commands = %q, want one on the live server", got)
	}
	if got := f.ranMatching("dolt init"); len(got) != 0 {
		t.Errorf("offline dolt init ran beside a live server: %q", got)
	}
	if want := fmt.Sprintf("not force-killing PID %d", holder); !strings.Contains(stderr, want) {
		t.Errorf("stderr = %q, want the refused-kill warning %q", stderr, want)
	}
}

// A running server gt owns whose data directory is gone is an orphan Start
// replaces. One that survives SIGTERM and SIGKILL still holds the endpoint, so
// Start reports that it did not stop instead of starting a second server on
// its port (gt-8oc5pa).
func TestStart_OwnedOrphanThatStaysUpDoesNotStartASecondServer(t *testing.T) {
	t.Parallel()
	const port = 4564
	f := newFakeHost().townPort(port)
	townRoot := testTown(t)
	pid := f.doltServer(townRoot, port)
	f.mu.Lock()
	f.procs[pid].ignoresTERM = true
	f.procs[pid].ignoresKILL = true
	f.mu.Unlock()

	err := f.host().Start(townRoot)
	if err == nil || !strings.Contains(err.Error(), "did not stop") {
		t.Fatalf("Start = %v, want an error saying the orphan did not stop", err)
	}
	if len(f.started) != 0 {
		t.Errorf("processes started = %d, want none beside the server that did not stop", len(f.started))
	}
	if _, err := os.Stat(filepath.Join(townRoot, ".dolt-data")); !os.IsNotExist(err) {
		t.Errorf("data directory exists (stat err %v), want it absent", err)
	}
}

// A server reachable over TCP alone (pid 0) is the town's live endpoint, not
// an orphan gt may stop: Start keeps it and starts nothing beside it
// (gt-8oc5pa).
func TestStart_UnownedServerWithoutDataDirIsNotStopped(t *testing.T) {
	t.Parallel()
	const port = 4563
	f := newFakeHost().townPort(port)
	f.reachable[fmt.Sprintf("127.0.0.1:%d", port)] = true
	townRoot := testTown(t)

	if err := f.host().Start(townRoot); err != nil {
		t.Fatalf("Start = %v, want nil: the endpoint server is the live one", err)
	}
	if len(f.signals) != 0 {
		t.Errorf("signals sent = %v, want none: gt owns no local process", f.signals)
	}
	if got := f.ranMatching("docker"); len(got) != 0 {
		t.Errorf("docker ran against a server gt does not own: %q", got)
	}
	if len(f.started) != 0 {
		t.Errorf("processes started = %d, want none beside a live endpoint", len(f.started))
	}
	if _, err := os.Stat(filepath.Join(townRoot, ".dolt-data")); !os.IsNotExist(err) {
		t.Errorf("data directory exists (stat err %v), want it absent", err)
	}
}
