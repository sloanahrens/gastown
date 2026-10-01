//go:build integration

package land

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The unit tier answers ps and lsof with canned output. These run the real
// tools, and the real process start in realRun.

// TestIntegrationSnapshotProcessesCapturesTreeAndTCPPeers runs ps and lsof
// against this test process, which holds one TCP connection to itself.
func TestIntegrationSnapshotProcessesCapturesTreeAndTCPPeers(t *testing.T) {
	t.Parallel()
	for _, tool := range []string{"ps", "lsof"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	srv, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), slowCaptureBudget)
	defer cancel()
	snap := snapshotProcesses(ctx, []int{os.Getpid()}, execOutput)
	if !slices.Contains(snap.PIDs, os.Getpid()) {
		t.Errorf("snapshot pids %v lack this process (%d)", snap.PIDs, os.Getpid())
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	for _, want := range []string{"== process tree", strconv.Itoa(os.Getpid()), "== open TCP peers", "127.0.0.1:" + port} {
		if !strings.Contains(snap.Text, want) {
			t.Errorf("snapshot lacks %q:\n%s", want, snap.Text)
		}
	}
}

// TestIntegrationRealRunTracksTheStageProcess: the pid of a command realRun
// starts is on the stage's tracker while it runs and gone when it ends.
func TestIntegrationRealRunTracksTheStageProcess(t *testing.T) {
	t.Parallel()
	tracker := &pidTracker{}
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), pidTrackerKey{}, tracker))
	defer cancel()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, _ = realRun(ctx, t.TempDir(), nil, []string{"sh", "-c", "sleep 30"}, &bytes.Buffer{})
	}()
	deadline := time.Now().Add(10 * time.Second)
	for len(tracker.list()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the running command never reached the tracker")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-finished
	if left := tracker.list(); len(left) != 0 {
		t.Errorf("pids %v still tracked after the command ended", left)
	}
}
