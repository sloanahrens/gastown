package witness

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestDefaultBdCliBoundsAWedgedSubprocess reproduces gt-7itep: a patrol scan
// (gt patrol state-collapse and friends) shelling out through
// witness.DefaultBdCli() would park in wait4 forever on a bd child that never
// returns — e.g. a git credential prompt or similar blocking grandchild bd
// spawns with no bounded timeout. DefaultBdCli's Exec/Run must return once
// the bd subprocess budget (GT_BD_TIMEOUT_SEC here) expires, not hang past
// it, and the returned error must name the timeout rather than surface as an
// unexplained "signal: killed".
func TestDefaultBdCliBoundsAWedgedSubprocess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a Unix shell script to stand in for bd")
	}
	t.Setenv("GT_BD_TIMEOUT_SEC", "1")
	installBlockingBdOnPath(t)

	bd := DefaultBdCli()
	workDir := t.TempDir()

	done := make(chan error, 1)
	go func() {
		_, err := bd.Exec(workDir, "show", "gt-7itep", "--json")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Exec should fail when bd blocks past its deadline")
		}
		if !strings.Contains(err.Error(), "context deadline exceeded") {
			t.Errorf("error should name the timeout, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Exec did not return within 10s of a 1s bd subprocess budget — the wedged child was never killed (gt-7itep)")
	}
}

// TestDefaultBdCliRunBoundsAWedgedSubprocess is TestDefaultBdCliBoundsAWedgedSubprocess
// for the Run half of BdCli (used for mutating bd calls, e.g. "bd close").
func TestDefaultBdCliRunBoundsAWedgedSubprocess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a Unix shell script to stand in for bd")
	}
	t.Setenv("GT_BD_TIMEOUT_SEC", "1")
	installBlockingBdOnPath(t)

	bd := DefaultBdCli()
	workDir := t.TempDir()

	done := make(chan error, 1)
	go func() {
		done <- bd.Run(workDir, "close", "gt-7itep", "-r", "test")
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run should fail when bd blocks past its deadline")
		}
		if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "context deadline exceeded") {
			t.Errorf("error should name the timeout, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return within 10s of a 1s bd subprocess budget — the wedged child was never killed (gt-7itep)")
	}
}

// installBlockingBdOnPath puts a fake bd on PATH that blocks forever (via
// `exec sleep`, so a deadline kill reaps the blocking process itself rather
// than orphaning a child): the same shape the witness escalation confirmed
// via `sample` on the real hang — the parent parked in __wait4_nocancel
// waiting on a child subprocess that never returns.
func installBlockingBdOnPath(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	script := "#!/bin/sh\nexec sleep 30\n"
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
