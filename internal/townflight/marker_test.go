package townflight

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestDrainMarker checks the drain marker refuses Register until it is cleared.
func TestDrainMarker(t *testing.T) {
	t.Parallel()
	t.Run("StartDrain refuses a new command", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir() // no daemon directory yet
		if err := StartDrain(townRoot); err != nil {
			t.Fatalf("StartDrain: %v", err)
		}
		if _, err := os.Stat(drainMarkerPath(townRoot)); err != nil {
			t.Fatalf("marker after StartDrain: %v", err)
		}
		if err := os.WriteFile(daemonLockPath(townRoot), nil, 0o600); err != nil {
			t.Fatalf("writing daemon.lock: %v", err)
		}
		_, err := Register(townRoot, "gt status")
		if !errors.Is(err, ErrDraining) {
			t.Fatalf("Register while draining = %v, want ErrDraining", err)
		}
		if !strings.Contains(err.Error(), "gt status") {
			t.Errorf("Register error %q does not name the label", err)
		}
		left, err := os.ReadDir(inflightDir(townRoot))
		if err != nil {
			t.Fatalf("reading inflight dir: %v", err)
		}
		if len(left) != 0 {
			t.Errorf("refused Register left %d entries, want none", len(left))
		}
	})
	t.Run("ClearDrain lets a command start", func(t *testing.T) {
		t.Parallel()
		townRoot := townWithDaemon(t)
		if err := StartDrain(townRoot); err != nil {
			t.Fatalf("StartDrain: %v", err)
		}
		if err := ClearDrain(townRoot); err != nil {
			t.Fatalf("ClearDrain: %v", err)
		}
		if _, err := os.Stat(drainMarkerPath(townRoot)); !os.IsNotExist(err) {
			t.Errorf("marker after ClearDrain = %v, want not exist", err)
		}
		release, err := Register(townRoot, "gt status")
		if err != nil {
			t.Fatalf("Register after ClearDrain: %v", err)
		}
		if err := release(); err != nil {
			t.Errorf("release: %v", err)
		}
	})
	t.Run("ClearDrain without a marker succeeds", func(t *testing.T) {
		t.Parallel()
		if err := ClearDrain(t.TempDir()); err != nil {
			t.Errorf("ClearDrain with no marker: %v", err)
		}
	})
	t.Run("unreadable marker is an error", func(t *testing.T) {
		t.Parallel()
		townRoot := townWithDaemon(t)
		// A symlink to itself makes the marker unreadable with ELOOP on every
		// host, where a mode-based refusal would pass under a privileged test
		// runner.
		if err := os.Symlink("draining", drainMarkerPath(townRoot)); err != nil {
			t.Fatalf("creating loop symlink: %v", err)
		}
		_, err := Register(townRoot, "gt status")
		if !errors.Is(err, syscall.ELOOP) {
			t.Fatalf("Register with an unreadable marker = %v, want it to wrap ELOOP", err)
		}
		if errors.Is(err, ErrDraining) {
			t.Errorf("unreadable marker reported as draining: %v", err)
		}
		left, err := os.ReadDir(inflightDir(townRoot))
		if err != nil {
			t.Fatalf("reading inflight dir: %v", err)
		}
		if len(left) != 0 {
			t.Errorf("failed marker read left %d entries, want none", len(left))
		}
	})
	t.Run("StartDrain failures", func(t *testing.T) {
		t.Parallel()
		blocked := t.TempDir()
		if err := os.WriteFile(filepath.Join(blocked, "daemon"), nil, 0o600); err != nil {
			t.Fatalf("writing daemon blocker: %v", err)
		}
		if err := StartDrain(blocked); !errors.Is(err, syscall.ENOTDIR) {
			t.Errorf("StartDrain below a regular daemon file = %v, want ENOTDIR", err)
		}
		occupied := townWithDaemon(t)
		if err := os.MkdirAll(drainMarkerPath(occupied), 0o755); err != nil {
			t.Fatalf("making marker directory: %v", err)
		}
		if err := StartDrain(occupied); !errors.Is(err, syscall.EISDIR) {
			t.Errorf("StartDrain over a marker directory = %v, want EISDIR", err)
		}
	})
	t.Run("ClearDrain reports a failed remove", func(t *testing.T) {
		t.Parallel()
		blocked := t.TempDir()
		if err := os.WriteFile(filepath.Join(blocked, "daemon"), nil, 0o600); err != nil {
			t.Fatalf("writing daemon blocker: %v", err)
		}
		if err := ClearDrain(blocked); !errors.Is(err, syscall.ENOTDIR) {
			t.Errorf("ClearDrain below a regular daemon file = %v, want ENOTDIR", err)
		}
	})
}
