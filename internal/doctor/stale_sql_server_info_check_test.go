package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// sqlServerInfoCheck is a StaleSQLServerInfoCheck on a machine where only the
// given PIDs are running.
func sqlServerInfoCheck(live ...int) *StaleSQLServerInfoCheck {
	c := NewStaleSQLServerInfoCheck()
	c.alive = func(pid int) bool {
		for _, p := range live {
			if p == pid {
				return true
			}
		}
		return false
	}
	return c
}

func TestStaleSQLServerInfoCheck_NoFiles(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	check := sqlServerInfoCheck(4242)
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("expected OK with no sql-server.info files, got %s: %s", result.Status, result.Message)
	}
}

func TestStaleSQLServerInfoCheck_DetectsStaleFile(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create a .dolt directory with a sql-server.info file referencing a dead PID
	doltDir := filepath.Join(tmpDir, "myrig", ".beads", "dolt", ".dolt")
	if err := os.MkdirAll(doltDir, 0755); err != nil {
		t.Fatal(err)
	}

	// The PID it names is not running.
	infoPath := filepath.Join(doltDir, "sql-server.info")
	if err := os.WriteFile(infoPath, []byte("999999999:3307:some-uuid"), 0644); err != nil {
		t.Fatal(err)
	}

	check := sqlServerInfoCheck(4242)
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)
	if result.Status != StatusWarning {
		t.Errorf("expected Warning for stale sql-server.info, got %s: %s", result.Status, result.Message)
	}
	if len(check.staleFiles) != 1 {
		t.Errorf("expected 1 stale file, got %d", len(check.staleFiles))
	}
}

func TestStaleSQLServerInfoCheck_FixIsNoOp(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create stale sql-server.info files in two rigs
	for _, rig := range []string{"rig1", "rig2"} {
		doltDir := filepath.Join(tmpDir, rig, ".beads", "dolt", ".dolt")
		if err := os.MkdirAll(doltDir, 0755); err != nil {
			t.Fatal(err)
		}
		infoPath := filepath.Join(doltDir, "sql-server.info")
		if err := os.WriteFile(infoPath, []byte("999999999:3307:uuid"), 0644); err != nil {
			t.Fatal(err)
		}
	}

	check := sqlServerInfoCheck(4242)
	ctx := &CheckContext{TownRoot: tmpDir}

	// Run to detect
	result := check.Run(ctx)
	if result.Status != StatusWarning {
		t.Fatalf("expected Warning, got %s", result.Status)
	}
	if len(check.staleFiles) != 2 {
		t.Fatalf("expected 2 stale files, got %d", len(check.staleFiles))
	}

	// Fix is a no-op — sql-server.info is a Dolt-internal file
	if err := check.Fix(ctx); err != nil {
		t.Fatalf("Fix failed: %v", err)
	}

	// Verify files are NOT removed (Fix no longer deletes Dolt-internal files)
	for _, path := range check.staleFiles {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			t.Errorf("expected %s to still exist after no-op fix", path)
		}
	}
}

func TestStaleSQLServerInfoCheck_SkipsLiveProcess(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()

	// Create a sql-server.info naming a live PID
	doltDir := filepath.Join(tmpDir, "myrig", ".beads", "dolt", ".dolt")
	if err := os.MkdirAll(doltDir, 0755); err != nil {
		t.Fatal(err)
	}

	infoPath := filepath.Join(doltDir, "sql-server.info")
	content := "4242:3307:some-uuid"
	if err := os.WriteFile(infoPath, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	check := sqlServerInfoCheck(4242)
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)
	if result.Status != StatusOK {
		t.Errorf("expected OK for live process, got %s: %s", result.Status, result.Message)
	}
}

func TestStaleSQLServerInfoCheck_EmptyFile(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	doltDir := filepath.Join(tmpDir, "myrig", ".beads", "dolt", ".dolt")
	if err := os.MkdirAll(doltDir, 0755); err != nil {
		t.Fatal(err)
	}

	infoPath := filepath.Join(doltDir, "sql-server.info")
	if err := os.WriteFile(infoPath, []byte(""), 0644); err != nil {
		t.Fatal(err)
	}

	check := sqlServerInfoCheck(4242)
	ctx := &CheckContext{TownRoot: tmpDir}

	result := check.Run(ctx)
	if result.Status != StatusWarning {
		t.Errorf("expected Warning for empty sql-server.info, got %s: %s", result.Status, result.Message)
	}
}

// TestSignalZeroAliveEPERM injects the signal error so a live server owned by
// another uid is testable without root.
func TestSignalZeroAliveEPERM(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"probe succeeded", nil, true},
		{"no such process", syscall.ESRCH, false},
		{"no such process, wrapped", fmt.Errorf("probe: %w", syscall.ESRCH), false},
		{"os.Process reports ESRCH as done", os.ErrProcessDone, false},
		{"reaped child", &os.SyscallError{Syscall: "kill", Err: syscall.ESRCH}, false},
		{"exists under another uid", syscall.EPERM, true},
		{"exists under another uid, via os.Process", &os.SyscallError{Syscall: "kill", Err: syscall.EPERM}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := signalZeroAliveAfterError(tc.err); got != tc.want {
				t.Errorf("signalZeroAliveAfterError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestSignalZeroAliveNonPositivePID(t *testing.T) {
	t.Parallel()
	for _, pid := range []int{0, -1} {
		if signalZeroAlive(pid) {
			t.Errorf("signalZeroAlive(%d) = true, want false", pid)
		}
	}
}
