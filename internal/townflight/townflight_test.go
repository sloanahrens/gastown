package townflight

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/gofrs/flock"
)

// townWithDaemon returns a town root holding the daemon.lock a daemon leaves when it starts.
func townWithDaemon(t *testing.T) string {
	t.Helper()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(daemonLockPath(townRoot)), 0o755); err != nil {
		t.Fatalf("making daemon dir: %v", err)
	}
	if err := os.WriteFile(daemonLockPath(townRoot), nil, 0o600); err != nil {
		t.Fatalf("writing daemon.lock: %v", err)
	}
	return townRoot
}

// TestRegister checks a registered entry is a locked, labeled .cmd file removed by release.
func TestRegister(t *testing.T) {
	t.Parallel()
	townRoot := townWithDaemon(t)
	release, err := Register(townRoot, "gt status")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	dir := inflightDir(townRoot)
	listed, err := os.ReadDir(dir)
	if err != nil || len(listed) != 1 || filepath.Ext(listed[0].Name()) != ".cmd" {
		t.Fatalf("listing = %v, %v; want one entry ending .cmd", listed, err)
	}
	path := filepath.Join(dir, listed[0].Name())
	if label, err := os.ReadFile(path); err != nil || string(label) != "gt status" {
		t.Errorf("entry = %q, %v; want %q", label, err, "gt status")
	}
	if ok, err := flock.New(path).TryLock(); err != nil || ok {
		t.Errorf("TryLock while registered = %v, %v; want the lock held", ok, err)
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("Stat after release = %v, want not exist", err)
	}
}

// TestRegisterFailures checks both failures are returned and leave no entry.
func TestRegisterFailures(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	townRoot := townWithDaemon(t)
	if _, err := register(func(string, string) error { return boom }, townRoot, "gt status"); !errors.Is(err, boom) {
		t.Fatalf("failed rename returned %v, want it to wrap %v", err, boom)
	}
	if listed, err := os.ReadDir(inflightDir(townRoot)); err != nil || len(listed) != 0 {
		t.Errorf("failed rename left %v behind (%v), want nothing", listed, err)
	}
	// The inflight directory is a regular file, so creating the entry fails.
	blocked := townWithDaemon(t)
	if err := os.WriteFile(inflightDir(blocked), nil, 0o600); err != nil {
		t.Fatalf("writing blocker: %v", err)
	}
	if _, err := Register(blocked, "gt status"); err == nil {
		t.Error("Register below a regular inflight file returned no error")
	}
}

// TestRegisterNoDaemon checks a town with no daemon.lock registers nothing and
// releases cleanly, and that a stat error other than not-exist is returned.
func TestRegisterNoDaemon(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	release, err := Register(townRoot, "gt status")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := os.Stat(filepath.Join(townRoot, "daemon")); !os.IsNotExist(err) {
		t.Errorf("Stat of daemon dir = %v, want not exist: nothing may be created", err)
	}
	if err := release(); err != nil {
		t.Errorf("release: %v", err)
	}

	// A daemon that is a regular file makes the lock stat fail with ENOTDIR.
	blocked := t.TempDir()
	if err := os.WriteFile(filepath.Join(blocked, "daemon"), nil, 0o600); err != nil {
		t.Fatalf("writing blocker: %v", err)
	}
	if _, err := Register(blocked, "gt status"); !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("Register under a failing stat = %v, want it to wrap ENOTDIR", err)
	}
}
