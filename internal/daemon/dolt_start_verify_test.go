package daemon

import (
	"os"
	"strings"
	"testing"
)

// verifyStartedManager returns a manager holding a started server with the
// given PID, its pid file written, and its port listener answering holder.
func verifyStartedManager(t *testing.T, pid, holder int) (*DoltServerManager, *os.Process) {
	t.Helper()
	m := newTestManager(t)
	proc := &os.Process{Pid: pid}
	m.process = proc
	if _, err := writePIDFile(m.pidFile(), pid); err != nil {
		t.Fatal(err)
	}
	m.portListenerFn = func(int) int { return holder }
	return m, proc
}

// A dolt that exited during startup is a failed start even though something
// answers on the port: the tracked process and pid file both go.
func TestVerifyStarted_ExitedDuringStartupFails(t *testing.T) {
	t.Parallel()
	m, proc := verifyStartedManager(t, 4242, 0)
	exited := make(chan struct{})
	close(exited)

	err := m.verifyStartedLocked(proc, exited)
	if err == nil || !strings.Contains(err.Error(), "exited during startup") {
		t.Fatalf("verifyStartedLocked = %v, want an 'exited during startup' error", err)
	}
	if m.process != nil {
		t.Errorf("m.process = %v, want nil for a server that never came up", m.process)
	}
	if _, statErr := os.Stat(m.pidFile()); !os.IsNotExist(statErr) {
		t.Errorf("pid file still names the dead server (stat err = %v)", statErr)
	}
}

// The listener being the process we started is the success case, and so is a
// listener lsof cannot identify: the check fails only on proof.
func TestVerifyStarted_OwnedOrUnknownListenerPasses(t *testing.T) {
	t.Parallel()
	for name, holder := range map[string]int{"we hold the port": 4242, "listener unknown": 0} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m, proc := verifyStartedManager(t, 4242, holder)
			if err := m.verifyStartedLocked(proc, make(chan struct{})); err != nil {
				t.Fatalf("verifyStartedLocked = %v, want nil", err)
			}
			if m.process != proc {
				t.Errorf("m.process = %v, want the started server tracked", m.process)
			}
			if _, statErr := os.Stat(m.pidFile()); statErr != nil {
				t.Errorf("pid file missing for a healthy start: %v", statErr)
			}
		})
	}
}
