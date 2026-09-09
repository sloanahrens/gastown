package deacon

import (
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

func TestHeartbeatPollerPidFile(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-deacon"

	pidFile := heartbeatPollerPidFile(townRoot, session)
	expected := filepath.Join(townRoot, ".runtime", "deacon_heartbeat_poller", session+".pid")
	if pidFile != expected {
		t.Errorf("heartbeatPollerPidFile() = %q, want %q", pidFile, expected)
	}
}

func TestHeartbeatPollerPidFile_SlashSanitized(t *testing.T) {
	townRoot := t.TempDir()
	session := "some/session"

	pidFile := heartbeatPollerPidFile(townRoot, session)
	expected := filepath.Join(townRoot, ".runtime", "deacon_heartbeat_poller", "some_session.pid")
	if pidFile != expected {
		t.Errorf("heartbeatPollerPidFile() = %q, want %q", pidFile, expected)
	}
}

func TestHeartbeatPollerAlive_NoPidFile(t *testing.T) {
	townRoot := t.TempDir()
	_, alive := heartbeatPollerAlive(townRoot, "nonexistent-session")
	if alive {
		t.Error("heartbeatPollerAlive() returned true for nonexistent PID file")
	}
}

func TestHeartbeatPollerAlive_StalePid(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-deacon"

	pidDir := heartbeatPollerPidDir(townRoot)
	if err := os.MkdirAll(pidDir, 0755); err != nil {
		t.Fatal(err)
	}
	pidPath := heartbeatPollerPidFile(townRoot, session)
	if err := os.WriteFile(pidPath, []byte("999999999"), 0644); err != nil {
		t.Fatal(err)
	}

	_, alive := heartbeatPollerAlive(townRoot, session)
	if alive {
		t.Error("heartbeatPollerAlive() returned true for dead PID")
	}

	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("stale PID file was not cleaned up")
	}
}

func TestHeartbeatPollerAlive_CorruptPidFile(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-deacon"

	pidDir := heartbeatPollerPidDir(townRoot)
	if err := os.MkdirAll(pidDir, 0755); err != nil {
		t.Fatal(err)
	}
	pidPath := heartbeatPollerPidFile(townRoot, session)
	if err := os.WriteFile(pidPath, []byte("not-a-number"), 0644); err != nil {
		t.Fatal(err)
	}

	_, alive := heartbeatPollerAlive(townRoot, session)
	if alive {
		t.Error("heartbeatPollerAlive() returned true for corrupt PID file")
	}
}

func TestHeartbeatPollerAlive_LiveProcess(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-deacon"

	pidDir := heartbeatPollerPidDir(townRoot)
	if err := os.MkdirAll(pidDir, 0755); err != nil {
		t.Fatal(err)
	}
	pidPath := heartbeatPollerPidFile(townRoot, session)
	myPid := os.Getpid()
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(myPid)), 0644); err != nil {
		t.Fatal(err)
	}

	pid, alive := heartbeatPollerAlive(townRoot, session)
	if !alive {
		t.Error("heartbeatPollerAlive() returned false for live process")
	}
	if pid != myPid {
		t.Errorf("heartbeatPollerAlive() pid = %d, want %d", pid, myPid)
	}
}

func TestStopHeartbeatPoller_NoPidFile(t *testing.T) {
	townRoot := t.TempDir()
	if err := StopHeartbeatPoller(townRoot, "nonexistent"); err != nil {
		t.Errorf("StopHeartbeatPoller() unexpected error: %v", err)
	}
}

func TestStopHeartbeatPoller_StalePid(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-deacon"

	pidDir := heartbeatPollerPidDir(townRoot)
	if err := os.MkdirAll(pidDir, 0755); err != nil {
		t.Fatal(err)
	}
	pidPath := heartbeatPollerPidFile(townRoot, session)
	if err := os.WriteFile(pidPath, []byte("999999999"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := StopHeartbeatPoller(townRoot, session); err != nil {
		t.Errorf("StopHeartbeatPoller() unexpected error: %v", err)
	}

	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("StopHeartbeatPoller did not clean up stale PID file")
	}
}

func TestBuildHeartbeatPollerCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process group management is not supported on Windows")
	}
	townRoot := t.TempDir()
	cmd := buildHeartbeatPollerCommand("/tmp/fake-gt", townRoot, "gt-gastown-deacon")

	if got, want := cmd.Dir, townRoot; got != want {
		t.Fatalf("cmd.Dir = %q, want %q", got, want)
	}
	if got, want := cmd.Path, "/tmp/fake-gt"; got != want {
		t.Fatalf("cmd.Path = %q, want %q", got, want)
	}
	if len(cmd.Args) != 4 || cmd.Args[1] != "deacon" || cmd.Args[2] != "heartbeat-poller" || cmd.Args[3] != "gt-gastown-deacon" {
		t.Fatalf("cmd.Args = %#v, want deacon heartbeat-poller invocation", cmd.Args)
	}
	if cmd.Stdout != nil || cmd.Stderr != nil {
		t.Fatal("buildHeartbeatPollerCommand() should discard stdout/stderr")
	}
	if cmd.SysProcAttr == nil {
		t.Fatal("buildHeartbeatPollerCommand() did not configure SysProcAttr")
	}
}

func TestStartHeartbeatPoller_IdempotentWhenAlreadyRunning(t *testing.T) {
	townRoot := t.TempDir()
	session := "gt-gastown-deacon"

	// Pre-seed a PID file for our own (definitely alive) process so
	// StartHeartbeatPoller takes the "already running" short-circuit
	// instead of actually spawning a new background process.
	pidDir := heartbeatPollerPidDir(townRoot)
	if err := os.MkdirAll(pidDir, 0755); err != nil {
		t.Fatal(err)
	}
	pidPath := heartbeatPollerPidFile(townRoot, session)
	myPid := os.Getpid()
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(myPid)), 0644); err != nil {
		t.Fatal(err)
	}

	pid, err := StartHeartbeatPoller(townRoot, session)
	if err != nil {
		t.Fatalf("StartHeartbeatPoller() unexpected error: %v", err)
	}
	if pid != myPid {
		t.Fatalf("StartHeartbeatPoller() should short-circuit on an already-alive poller, got pid=%d want=%d", pid, myPid)
	}
}
