package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeProcs scripts the processes a pidTracker sees: live maps a running PID
// to its current start token, and terminated records the PIDs sent SIGTERM.
type fakeProcs struct {
	live       map[int]string
	terminated []int
}

func (f *fakeProcs) tracker() pidTracker {
	return pidTracker{
		token: func(pid int) (string, bool) {
			start, ok := f.live[pid]
			return start, ok
		},
		terminate: func(pid int) error {
			f.terminated = append(f.terminated, pid)
			return nil
		},
		remove: os.Remove,
	}
}

func writePIDFile(t *testing.T, townRoot, name, content string) string {
	t.Helper()
	if err := os.MkdirAll(pidsDir(townRoot), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(pidsDir(townRoot), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestTrackPID_RecordsStartTime(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	procs := &fakeProcs{live: map[int]string{12345: "1700000000.5"}}

	if err := procs.tracker().track(townRoot, "gt-myrig-witness", 12345); err != nil {
		t.Fatalf("track() error = %v", err)
	}

	data, err := os.ReadFile(pidFile(townRoot, "gt-myrig-witness"))
	if err != nil {
		t.Fatalf("reading PID file: %v", err)
	}
	if got := string(data); got != "12345|1700000000.5\n" {
		t.Errorf("PID file content = %q, want start-time tracked format", got)
	}
}

// G1-21: a bare-PID record could never be verified, so track writes none.
func TestTrackPID_NoRecordWhenStartTimeUnknown(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	procs := &fakeProcs{}

	if err := procs.tracker().track(townRoot, "gt-test", 99); err == nil {
		t.Fatal("track() succeeded for a process with no readable start time")
	}
	if exists(pidFile(townRoot, "gt-test")) {
		t.Error("track() wrote an unverifiable bare-PID record")
	}
}

func TestUntrackPID_RemovesFile(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	path := writePIDFile(t, townRoot, "gt-test.pid", "111\n")

	UntrackPID(townRoot, "gt-test")
	UntrackPID(townRoot, "nonexistent")

	if exists(path) {
		t.Error("PID file should be removed after UntrackPID")
	}
}

func TestKillTrackedPIDs_NoPidsDir(t *testing.T) {
	t.Parallel()
	procs := &fakeProcs{}
	killed, errs := procs.tracker().killTracked(t.TempDir())
	if killed != 0 || len(errs) != 0 {
		t.Errorf("killTracked() = %d, %v; want 0, none", killed, errs)
	}
}

func TestKillTrackedPIDs_KillsLiveMatchingProcess(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	path := writePIDFile(t, townRoot, "gt-a.pid", "100|start-a\n")
	procs := &fakeProcs{live: map[int]string{100: "start-a"}}

	killed, errs := procs.tracker().killTracked(townRoot)
	if killed != 1 || len(errs) != 0 {
		t.Errorf("killTracked() = %d, %v; want 1, none", killed, errs)
	}
	if len(procs.terminated) != 1 || procs.terminated[0] != 100 {
		t.Errorf("terminated = %v, want PID 100", procs.terminated)
	}
	if exists(path) {
		t.Error("PID file should be removed after the kill")
	}
}

func TestKillTrackedPIDs_CleansUpWithoutKilling(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		file    string
		content string
	}{
		{"dead process", "gt-dead.pid", "4194305|start\n"},
		{"corrupt file", "gt-corrupt.pid", "not-a-number\n"},
		// The recycled pid: 300 is live, but its start token says it is
		// a later process than the one tracked.
		{"pid reused", "gt-reused.pid", "300|old-start\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			path := writePIDFile(t, townRoot, tt.file, tt.content)
			procs := &fakeProcs{live: map[int]string{300: "new-start"}}

			killed, errs := procs.tracker().killTracked(townRoot)
			if killed != 0 || len(errs) != 0 {
				t.Errorf("killTracked() = %d, %v; want 0, none", killed, errs)
			}
			if len(procs.terminated) != 0 {
				t.Errorf("terminated %v, want nothing", procs.terminated)
			}
			if exists(path) {
				t.Error("PID file should be removed")
			}
		})
	}
}

func TestKillTrackedPIDs_SkipsNonPidFiles(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	path := writePIDFile(t, townRoot, "readme.txt", "100\n")
	procs := &fakeProcs{live: map[int]string{100: ""}}

	killed, errs := procs.tracker().killTracked(townRoot)
	if killed != 0 || len(errs) != 0 {
		t.Errorf("killTracked() = %d, %v; want 0, none", killed, errs)
	}
	if !exists(path) {
		t.Error("non-.pid file should be left alone")
	}
}

// G1-21: a bare PID (written by an older gt when ps failed) whose number is
// live again is never signaled: nothing proves it is the tracked process.
func TestKillTrackedPIDs_RefusesBarePID(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	path := writePIDFile(t, townRoot, "gt-bare.pid", "200\n")
	procs := &fakeProcs{live: map[int]string{200: "start-b"}}

	killed, errs := procs.tracker().killTracked(townRoot)
	if killed != 0 || len(procs.terminated) != 0 {
		t.Errorf("killTracked() killed %d, terminated %v; want nothing signaled", killed, procs.terminated)
	}
	if len(errs) != 1 || !strings.Contains(errs[0], "no start time") {
		t.Errorf("errs = %v, want one entry naming the missing start time", errs)
	}
	if exists(path) {
		t.Error("unverifiable PID file should be removed")
	}
}

func TestPidFile_Path(t *testing.T) {
	t.Parallel()
	got := pidFile("/home/user/gt", "gt-myrig-witness")
	want := filepath.Join("/home/user/gt", ".runtime", "pids", "gt-myrig-witness.pid")
	if got != want {
		t.Errorf("pidFile() = %q, want %q", got, want)
	}
}

// TestPruneDeadTrackedPIDs covers the cadence sweep: it reads the same
// directory TrackPID writes and reclaims a record whose process is gone.
// procid's own tests cover the live/kept half with a scripted process table.
func TestPruneDeadTrackedPIDs(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	// A pid no process holds: StartToken finds nothing, so the record does not
	// name a running process.
	dead := writePIDFile(t, townRoot, "gt-old.pid", "999999999|1700000000.5")

	rep, err := PruneDeadTrackedPIDs(townRoot)
	if err != nil {
		t.Fatalf("PruneDeadTrackedPIDs: %v", err)
	}
	if rep.Removed != 1 {
		t.Errorf("Removed = %d, want 1", rep.Removed)
	}
	if exists(dead) {
		t.Error("the dead session's PID file survived the sweep")
	}
}

// A sandboxed session's pane PID comes from the sandbox's process table. Written
// with a start token read on the host it would name an unrelated host process,
// and `gt down` would SIGTERM it, so nothing is recorded for a sandboxed session
// (gt-n76g3, found auditing the daemon's liveness paths for sandbox seats).
func TestTrackSession_SandboxedSessionRecordsNothingAndDropsAStaleRecord(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := WriteSandboxRecord(townRoot, SandboxRecord{Session: "gt-gastown-box", Rig: "gastown", Polecat: "box"}); err != nil {
		t.Fatal(err)
	}
	// A host process holds the number the sandbox reports.
	procs := &fakeProcs{live: map[int]string{143: "1700000000.5"}}
	stale := writePIDFile(t, townRoot, "gt-gastown-box.pid", "999:1\n")
	asked := false
	panePID := func(string) (string, error) { asked = true; return "143", nil }

	err := procs.tracker().trackSession(townRoot, "gt-gastown-box", panePID)

	if err != nil {
		t.Fatalf("trackSession = %v, want nil: a sandboxed session simply has nothing to record", err)
	}
	if asked {
		t.Error("the sandbox's pane PID was read; it must not be")
	}
	if exists(pidFile(townRoot, "gt-gastown-box")) || exists(stale) {
		t.Error("a PID file exists for a sandboxed session")
	}
}

func TestTrackSession_HostSessionStillRecords(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	procs := &fakeProcs{live: map[int]string{4242: "1700000001.5"}}

	err := procs.tracker().trackSession(townRoot, "gt-gastown-host", func(string) (string, error) { return "4242\n", nil })

	if err != nil {
		t.Fatalf("trackSession = %v", err)
	}
	data, rerr := os.ReadFile(pidFile(townRoot, "gt-gastown-host"))
	if rerr != nil || !strings.Contains(string(data), "4242") {
		t.Fatalf("pid file = %q, %v; want the host pane PID recorded", data, rerr)
	}
}

// A registry that cannot be read leaves "is it sandboxed" unanswered: record
// nothing rather than a number that may belong to another process.
func TestTrackSession_UnreadableRegistryRecordsNothing(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	const stale = "7777\n"
	path := writePIDFile(t, townRoot, "gt-gastown-box.pid", stale)
	breakSandboxRegistry(t, townRoot)
	procs := &fakeProcs{live: map[int]string{4242: "1700000001.5"}}

	err := procs.tracker().trackSession(townRoot, "gt-gastown-box", func(string) (string, error) { return "4242", nil })

	if err == nil {
		t.Fatal("trackSession = nil with an unreadable registry")
	}
	if !strings.Contains(err.Error(), "cannot tell whether it runs in a sandbox") {
		t.Errorf("trackSession = %v; want the registry failure named, not a pane-PID or write error", err)
	}
	if data, rerr := os.ReadFile(path); rerr != nil || string(data) != stale {
		t.Errorf("PID file = %q, %v; want the stale record left untouched while the registry is unreadable", data, rerr)
	}
}

// A sandboxed session's stale record that cannot be removed is reported, not
// dropped silently: the operator must learn a record is still there.
func TestKillTrackedPIDs_UnremovableSandboxedRecordIsReported(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := WriteSandboxRecord(townRoot, SandboxRecord{Session: "gt-gastown-box", Rig: "gastown", Polecat: "box"}); err != nil {
		t.Fatal(err)
	}
	writePIDFile(t, townRoot, "gt-gastown-box.pid", "143:1700000000.5\n")
	procs := &fakeProcs{live: map[int]string{143: "1700000000.5"}}
	tracker := procs.tracker()
	tracker.remove = func(string) error { return os.ErrPermission }

	killed, errs := tracker.killTracked(townRoot)

	if killed != 0 || len(procs.terminated) != 0 {
		t.Fatalf("killed=%d terminated=%v; a sandboxed session must never be signalled", killed, procs.terminated)
	}
	if len(errs) != 1 || !strings.Contains(errs[0], "removing its stale PID record") {
		t.Errorf("errs=%v; want one error naming the failed removal", errs)
	}
}

// breakSandboxRegistry makes the sandbox registry directory unreadable as a
// directory, so a lookup fails with an error rather than "no record".
func breakSandboxRegistry(t *testing.T, townRoot string) {
	t.Helper()
	dir := SandboxRegistryDir(townRoot)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestKillTrackedPIDs_NeverSignalsASandboxedSession(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := WriteSandboxRecord(townRoot, SandboxRecord{Session: "gt-gastown-box", Rig: "gastown", Polecat: "box"}); err != nil {
		t.Fatal(err)
	}
	// The record matches a live host process exactly, as a recycled number would.
	procs := &fakeProcs{live: map[int]string{143: "1700000000.5"}}
	path := writePIDFile(t, townRoot, "gt-gastown-box.pid", "143:1700000000.5\n")

	killed, errs := procs.tracker().killTracked(townRoot)

	if killed != 0 || len(procs.terminated) != 0 {
		t.Fatalf("killed=%d terminated=%v; a sandboxed session's record must never be signalled", killed, procs.terminated)
	}
	if len(errs) != 0 || exists(path) {
		t.Errorf("errs=%v file exists=%v; want the stale record dropped quietly", errs, exists(path))
	}
}

func TestKillTrackedPIDs_UnreadableRegistrySignalsNothing(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	procs := &fakeProcs{live: map[int]string{143: "1700000000.5"}}
	writePIDFile(t, townRoot, "gt-gastown-box.pid", "143:1700000000.5\n")
	breakSandboxRegistry(t, townRoot)

	killed, errs := procs.tracker().killTracked(townRoot)

	if killed != 0 || len(procs.terminated) != 0 {
		t.Fatalf("killed=%d terminated=%v; nothing may be signalled while the registry is unreadable", killed, procs.terminated)
	}
	if len(errs) != 1 || !strings.Contains(errs[0], "gt-gastown-box") {
		t.Errorf("errs=%v, want one error naming the session", errs)
	}
}
