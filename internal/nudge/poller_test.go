package nudge

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPollerPidFile(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	session := "gt-gastown-crew-bear"

	pidFile := pollerPidFile(townRoot, session)
	expected := filepath.Join(townRoot, ".runtime", "nudge_poller", session+".pid")
	if pidFile != expected {
		t.Errorf("pollerPidFile() = %q, want %q", pidFile, expected)
	}
}

func TestPollerPidFile_SlashSanitized(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	session := "some/session"

	pidFile := pollerPidFile(townRoot, session)
	// Slashes should be replaced with underscores
	expected := filepath.Join(townRoot, ".runtime", "nudge_poller", "some_session.pid")
	if pidFile != expected {
		t.Errorf("pollerPidFile() = %q, want %q", pidFile, expected)
	}
}

// fakeProcs is a scripted process table for pollerProcs: pid -> current start
// token. A pid absent from it has no process. terminated records every pid
// the code under test tried to SIGTERM.
type fakeProcs struct {
	table      map[int]string
	terminated []int
}

func (f *fakeProcs) procs() pollerProcs {
	return pollerProcs{
		token: func(pid int) (string, bool) {
			tok, ok := f.table[pid]
			return tok, ok
		},
		terminate: func(pid int) error {
			f.terminated = append(f.terminated, pid)
			return nil
		},
	}
}

// writePollerRecord writes record as session's poller pid file.
func writePollerRecord(t *testing.T, townRoot, session, record string) string {
	t.Helper()
	if err := os.MkdirAll(pollerPidDir(townRoot), 0755); err != nil {
		t.Fatal(err)
	}
	pidPath := pollerPidFile(townRoot, session)
	if err := os.WriteFile(pidPath, []byte(record), 0644); err != nil {
		t.Fatal(err)
	}
	return pidPath
}

func TestPollerAlive_NoPidFile(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	f := &fakeProcs{}
	_, alive := f.procs().pollerAlive(townRoot, "nonexistent-session")
	if alive {
		t.Error("pollerAlive() returned true for nonexistent PID file")
	}
}

func TestPollerAlive_StalePid(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	session := "gt-gastown-crew-test"
	pidPath := writePollerRecord(t, townRoot, session, "4242|100.5")

	f := &fakeProcs{} // pid 4242 has no process
	if _, alive := f.procs().pollerAlive(townRoot, session); alive {
		t.Error("pollerAlive() returned true for dead PID")
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("stale PID file was not cleaned up")
	}
}

func TestPollerAlive_CorruptPidFile(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	session := "gt-gastown-crew-test"
	writePollerRecord(t, townRoot, session, "not-a-number")

	f := &fakeProcs{}
	if _, alive := f.procs().pollerAlive(townRoot, session); alive {
		t.Error("pollerAlive() returned true for corrupt PID file")
	}
}

func TestPollerAlive_LiveProcess(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	session := "gt-gastown-crew-test"
	writePollerRecord(t, townRoot, session, "4242|100.5")

	f := &fakeProcs{table: map[int]string{4242: "100.5"}}
	pid, alive := f.procs().pollerAlive(townRoot, session)
	if !alive || pid != 4242 {
		t.Errorf("pollerAlive() = %d, %v; want 4242, true", pid, alive)
	}
}

// G1-05: the pid in the record is live again, but it belongs to a process
// that started after the poller died. A bare kill(pid, 0) said "alive", so
// StartPoller never restarted the poller and StopPoller SIGTERMed a stranger.
func TestPollerAlive_RecycledPidIsNotAlive(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	session := "gt-gastown-crew-test"
	pidPath := writePollerRecord(t, townRoot, session, "4242|100.5")

	f := &fakeProcs{table: map[int]string{4242: "777.1"}}
	if _, alive := f.procs().pollerAlive(townRoot, session); alive {
		t.Error("pollerAlive() returned true for a recycled pid")
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("record naming a recycled pid was not removed")
	}
}

// A bare pid (the old record format) cannot be verified, so it is never
// treated as a live poller and never signaled.
func TestPollerAlive_BarePidIsNotAlive(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	session := "gt-gastown-crew-test"
	writePollerRecord(t, townRoot, session, "4242")

	f := &fakeProcs{table: map[int]string{4242: "100.5"}}
	if _, alive := f.procs().pollerAlive(townRoot, session); alive {
		t.Error("pollerAlive() returned true for a bare pid record")
	}
}

func TestStopPoller_NoPidFile(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	f := &fakeProcs{}
	// Should be a no-op, no error.
	if err := f.procs().stopPoller(townRoot, "nonexistent"); err != nil {
		t.Errorf("stopPoller() unexpected error: %v", err)
	}
	if len(f.terminated) != 0 {
		t.Errorf("stopPoller() signaled %v with no record", f.terminated)
	}
}

func TestStopPoller_StalePid(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	session := "gt-gastown-crew-test"
	pidPath := writePollerRecord(t, townRoot, session, "4242|100.5")

	f := &fakeProcs{}
	if err := f.procs().stopPoller(townRoot, session); err != nil {
		t.Errorf("stopPoller() unexpected error: %v", err)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("stopPoller did not clean up stale PID file")
	}
	if len(f.terminated) != 0 {
		t.Errorf("stopPoller() signaled dead pid: %v", f.terminated)
	}
}

func TestStopPoller_RecycledPidNotSignaled(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	session := "gt-gastown-crew-test"
	for _, record := range []string{"4242|100.5", "4242"} {
		pidPath := writePollerRecord(t, townRoot, session, record)
		f := &fakeProcs{table: map[int]string{4242: "777.1"}}
		if err := f.procs().stopPoller(townRoot, session); err != nil {
			t.Errorf("stopPoller(%q) unexpected error: %v", record, err)
		}
		if len(f.terminated) != 0 {
			t.Errorf("stopPoller(%q) SIGTERMed %v, a process that is not the poller", record, f.terminated)
		}
		if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
			t.Errorf("stopPoller(%q) left the stale record", record)
		}
	}
}

func TestStopPoller_LivePollerSignaled(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	session := "gt-gastown-crew-test"
	pidPath := writePollerRecord(t, townRoot, session, "4242|100.5")

	f := &fakeProcs{table: map[int]string{4242: "100.5"}}
	if err := f.procs().stopPoller(townRoot, session); err != nil {
		t.Errorf("stopPoller() unexpected error: %v", err)
	}
	if len(f.terminated) != 1 || f.terminated[0] != 4242 {
		t.Errorf("stopPoller() terminated %v, want [4242]", f.terminated)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Error("stopPoller left the record of the poller it stopped")
	}
}

func TestBuildPollerCommand_UsesDetachedProcessGroup(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	cmd := buildPollerCommand("/tmp/fake-gt", townRoot, "gt-gastown-crew-bear")

	if got, want := cmd.Dir, townRoot; got != want {
		t.Fatalf("cmd.Dir = %q, want %q", got, want)
	}
	if got, want := cmd.Path, "/tmp/fake-gt"; got != want {
		t.Fatalf("cmd.Path = %q, want %q", got, want)
	}
	if len(cmd.Args) != 3 || cmd.Args[1] != "nudge-poller" || cmd.Args[2] != "gt-gastown-crew-bear" {
		t.Fatalf("cmd.Args = %#v, want poller invocation", cmd.Args)
	}
	if cmd.Cancel != nil {
		t.Fatal("buildPollerCommand() installed cmd.Cancel; detached pollers must leave it nil")
	}
	if cmd.Stdout != nil || cmd.Stderr != nil {
		t.Fatal("buildPollerCommand() should discard stdout/stderr")
	}
	if cmd.SysProcAttr == nil {
		t.Fatal("buildPollerCommand() did not configure SysProcAttr")
	}
}

// Under `go test` os.Executable() is the test binary. Exec'ing it as the
// nudge-poller runs the whole suite again, detached, and every StartPoller
// site inside that run spawns another — the same recursion the daemon's
// boot-triage guard exists for (gt-0mbw found it through a fake tmux that
// made a Deacon look alive). StartPoller must refuse before Start.
func TestStartPoller_RefusesTestBinary(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"daemon.test", "daemon.test.exe"} {
		exe := filepath.Join(t.TempDir(), name)
		townRoot := t.TempDir()
		_, err := (&fakeProcs{}).procs().startPoller(townRoot, "gt-deacon", func() (string, error) { return exe, nil })
		if err == nil || !strings.Contains(err.Error(), "test binary") {
			t.Fatalf("StartPoller with %s: err=%v, want a refusal naming the test binary", name, err)
		}
		if _, statErr := os.Stat(pollerPidFile(townRoot, "gt-deacon")); statErr == nil {
			t.Fatalf("StartPoller wrote a pid file although it refused to spawn %s", name)
		}
	}
}

// TestPruneDeadPollerPIDFiles covers the cadence sweep: pollerAlive reclaims a
// stale record only for a session something still asks about, so the record of
// a session that is itself gone needs this to ever be reclaimed.
func TestPruneDeadPollerPIDFiles(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	dead := writePollerRecord(t, townRoot, "gt-old", "999999999|100.5")

	rep, err := PruneDeadPollerPIDFiles(townRoot)
	if err != nil {
		t.Fatalf("PruneDeadPollerPIDFiles: %v", err)
	}
	if rep.Removed != 1 {
		t.Errorf("Removed = %d, want 1", rep.Removed)
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Error("the dead session's poller record survived the sweep")
	}
}

// TestPruneDeadPollerPIDFilesKeepsTheRest is the safety half: the sweep reads
// the poller directory and nothing else.
func TestPruneDeadPollerPIDFilesKeepsTheRest(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	dead := writePollerRecord(t, townRoot, "gt-old", "999999999|100.5")
	other := filepath.Join(townRoot, ".runtime", "nudge_poller", "notes.txt")
	if err := os.WriteFile(other, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := PruneDeadPollerPIDFiles(townRoot); err != nil {
		t.Fatalf("PruneDeadPollerPIDFiles: %v", err)
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Error("the dead session's poller record survived the sweep")
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("the sweep removed a file that is not a pid record: %v", err)
	}
}
