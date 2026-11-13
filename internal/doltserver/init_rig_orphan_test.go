package doltserver

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

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
// makes no offline directory next to a server that is still running.
func TestInitRig_OwnedOrphanThatStaysUpKeepsServerRunning(t *testing.T) {
	t.Parallel()
	const port = 4562
	f := withDoltIdentity(newFakeHost().townPort(port)).on("dolt *", fakeReply{}).on("dolt init", fakeReply{})
	townRoot := testTown(t)
	pid := f.doltServer(townRoot, port)
	f.mu.Lock()
	f.procs[pid].ignoresTERM = true
	f.procs[pid].ignoresKILL = true
	f.mu.Unlock()
	h := initRigOrphanHost(f)

	result, err := h.InitRig(townRoot, "forkrig")
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
}
