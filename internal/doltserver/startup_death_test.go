package doltserver

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A dolt child that exits at once is reported as dead within one poll
// interval, not after the whole readiness window. The fake's process table
// keeps the child alive, the way an unreaped zombie still answers signal(0):
// only the reaped exit channel sees the death (gt-fpunm).
func TestStart_ReportsChildDeadAtStartup(t *testing.T) {
	t.Parallel()
	f := newFakeHost().townPort(4552)
	f.startExited = true
	f.startExitErr = errors.New("exit status 1")
	var startedPID int
	f.onStart = func(pid int, _ *exec.Cmd) { startedPID = pid }
	townRoot := serverModeTown(t)

	// What the child wrote before it died, waiting where Start reads it.
	logPath := filepath.Join(townRoot, "daemon", "dolt.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		t.Fatal(err)
	}
	logBody := "starting dolt sql-server\nconfig: unknown field 'read_timeout'\ndolt: shutting down after 1 error\n"
	if err := os.WriteFile(logPath, []byte(logBody), 0600); err != nil {
		t.Fatal(err)
	}

	err := f.host().Start(townRoot)
	if err == nil || !strings.Contains(err.Error(), "died during startup") {
		t.Fatalf("Start = %v, want a died-during-startup error", err)
	}
	for _, want := range []string{"exit status 1", "config: unknown field", "dolt: shutting down after 1 error"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Start error lost the child's log tail (%q):\n%v", want, err)
		}
	}
	if f.slept != 500*time.Millisecond {
		t.Errorf("Start waited %v before reporting the death, want one poll interval (500ms)", f.slept)
	}
	if startedPID == 0 || !f.alive(startedPID) {
		t.Fatalf("the child (PID %d) is not an unreaped zombie in the fake process table: the test no longer models the bug", startedPID)
	}
}

// exited and waitErr answer the same way however often they are asked: Start
// reads both when it reports a death (gt-fpunm).
func TestStartedProcess_ExitedIsStable(t *testing.T) {
	t.Parallel()
	p := &startedProcess{pid: 1, done: make(chan struct{})}
	if p.exited() || p.waitErr() != nil {
		t.Fatal("a running process reports as exited")
	}
	p.err = errors.New("exit status 3")
	close(p.done)
	for i := 0; i < 3; i++ {
		if !p.exited() {
			t.Fatalf("exited() call %d = false after the reap", i+1)
		}
		if err := p.waitErr(); err == nil || err.Error() != "exit status 3" {
			t.Fatalf("waitErr() call %d = %v, want the exit status", i+1, err)
		}
	}
}

// A log file longer than the tail window is read from its end, and a tail
// that starts mid-line drops the partial line.
func TestReadLogTail(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "dolt.log")
	big := strings.Repeat("old line\n", 2000) // well past logTailBytes
	if err := os.WriteFile(path, []byte(big+"first\nsecond\nthird\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if got := readLogTail(path, 2); got != "second\nthird\n" {
		t.Errorf("readLogTail(2) = %q, want the last two whole lines", got)
	}
	if got := readLogTail(path, 20); !strings.HasSuffix(got, "first\nsecond\nthird\n") || len(strings.Split(strings.TrimRight(got, "\n"), "\n")) != 20 {
		t.Errorf("readLogTail(20) = %q, want 20 whole lines ending at the file's end", got)
	}
	if got := readLogTail(filepath.Join(t.TempDir(), "missing.log"), 5); got != "" {
		t.Errorf("readLogTail of a missing file = %q, want empty", got)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if got := readLogTail(path, 5); got != "" {
		t.Errorf("readLogTail of an empty file = %q, want empty", got)
	}
}
