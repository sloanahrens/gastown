package session

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fakeProcs scripts the processes a pidTracker sees: live maps a running PID
// to its start time, and terminated records the PIDs sent SIGTERM.
type fakeProcs struct {
	live       map[int]string
	startErr   error
	terminated []int
}

func (f *fakeProcs) tracker() pidTracker {
	return pidTracker{
		startTime: func(pid int) (string, error) {
			if f.startErr != nil {
				return "", f.startErr
			}
			start, ok := f.live[pid]
			if !ok {
				return "", os.ErrNotExist
			}
			return start, nil
		},
		alive: func(pid int) bool {
			_, ok := f.live[pid]
			return ok
		},
		terminate: func(pid int) error {
			f.terminated = append(f.terminated, pid)
			return nil
		},
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
	procs := &fakeProcs{live: map[int]string{12345: "Mon Jan  1 00:00:00 2026"}}

	if err := procs.tracker().track(townRoot, "gt-myrig-witness", 12345); err != nil {
		t.Fatalf("track() error = %v", err)
	}

	data, err := os.ReadFile(pidFile(townRoot, "gt-myrig-witness"))
	if err != nil {
		t.Fatalf("reading PID file: %v", err)
	}
	if got := string(data); got != "12345|Mon Jan  1 00:00:00 2026\n" {
		t.Errorf("PID file content = %q, want start-time tracked format", got)
	}
}

func TestTrackPID_PIDOnlyWhenStartTimeUnknown(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	procs := &fakeProcs{startErr: errors.New("ps not available")}

	if err := procs.tracker().track(townRoot, "gt-test", 99); err != nil {
		t.Fatalf("track() error = %v", err)
	}

	data, err := os.ReadFile(pidFile(townRoot, "gt-test"))
	if err != nil {
		t.Fatalf("reading PID file: %v", err)
	}
	if got := string(data); got != "99\n" {
		t.Errorf("PID file content = %q, want %q", got, "99\n")
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
	withStart := writePIDFile(t, townRoot, "gt-a.pid", "100|start-a\n")
	pidOnly := writePIDFile(t, townRoot, "gt-b.pid", "200\n")
	procs := &fakeProcs{live: map[int]string{100: "start-a", 200: "start-b"}}

	killed, errs := procs.tracker().killTracked(townRoot)
	if killed != 2 || len(errs) != 0 {
		t.Errorf("killTracked() = %d, %v; want 2, none", killed, errs)
	}
	if len(procs.terminated) != 2 {
		t.Errorf("terminated = %v, want PIDs 100 and 200", procs.terminated)
	}
	if exists(withStart) || exists(pidOnly) {
		t.Error("PID files should be removed after the kill")
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

func TestKillTrackedPIDs_PreservesFileOnLookupError(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	path := writePIDFile(t, townRoot, "gt-err-lookup.pid", "100|some-start-time\n")
	procs := &fakeProcs{live: map[int]string{100: ""}, startErr: errors.New("ps not available")}

	killed, errs := procs.tracker().killTracked(townRoot)
	if killed != 0 {
		t.Errorf("killed = %d, want 0 (lookup error should skip kill)", killed)
	}
	if len(errs) != 1 {
		t.Errorf("errs = %v, want 1 entry for lookup error", errs)
	}
	if !exists(path) {
		t.Error("PID file should be preserved when start-time lookup fails")
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
