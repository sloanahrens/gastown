package townflight

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fakeProbeLock is a probe lock whose TryLock and Unlock results are set by the test.
type fakeProbeLock struct {
	acquired  bool
	unlockErr error
	unlocks   int
}

func (f *fakeProbeLock) Lock() error            { return nil }
func (f *fakeProbeLock) TryLock() (bool, error) { return f.acquired, nil }

func (f *fakeProbeLock) Unlock() error {
	f.unlocks++
	return f.unlockErr
}

// TestProbe checks a held entry reports running with its label, and a free entry is removed.
func TestProbe(t *testing.T) {
	t.Parallel()
	t.Run("held entry is running with its label", func(t *testing.T) {
		t.Parallel()
		e, err := createEntry(t.TempDir(), "gt status")
		if err != nil {
			t.Fatalf("createEntry: %v", err)
		}
		defer e.drop() //nolint:errcheck // best-effort cleanup
		label, running, err := probe(e.path)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if !running || label != "gt status" {
			t.Errorf("probe = %q, %v; want %q, true", label, running, "gt status")
		}
		if _, err := os.Stat(e.path); err != nil {
			t.Errorf("held entry was removed: %v", err)
		}
	})
	t.Run("free entry is removed and not running", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "inflight-free.cmd")
		if err := os.WriteFile(path, []byte("gt status"), 0o600); err != nil {
			t.Fatalf("writing entry: %v", err)
		}
		label, running, err := probe(path)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if running || label != "" {
			t.Errorf("probe = %q, %v; want %q, false", label, running, "")
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("Stat after probe = %v, want not exist", err)
		}
	})
	t.Run("path in a missing dir is a lock error", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "missing", "inflight.cmd")
		label, running, err := probe(path)
		if err == nil {
			t.Fatal("probe below a missing dir returned no error")
		}
		if running || label != "" {
			t.Errorf("probe = %q, %v; want %q, false", label, running, "")
		}
	})
}

// TestProbeFailures checks each failure a probe refuses is returned to the caller.
func TestProbeFailures(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	fixed := func(lock *fakeProbeLock) func(string) probeLock {
		return func(string) probeLock { return lock }
	}
	t.Run("failed removal is returned and the lock released", func(t *testing.T) {
		t.Parallel()
		lock := &fakeProbeLock{acquired: true}
		missing := filepath.Join(t.TempDir(), "gone.cmd")
		_, running, err := probeWith(fixed(lock), missing)
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("probe error = %v, want it to wrap %v", err, os.ErrNotExist)
		}
		if running {
			t.Error("probe reported running after a failed removal")
		}
		if lock.unlocks != 1 {
			t.Errorf("Unlock called %d times, want 1", lock.unlocks)
		}
	})
	t.Run("failed unlock is returned", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "inflight-free.cmd")
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("writing entry: %v", err)
		}
		lock := &fakeProbeLock{acquired: true, unlockErr: boom}
		_, _, err := probeWith(fixed(lock), path)
		if !errors.Is(err, boom) {
			t.Fatalf("probe error = %v, want it to wrap %v", err, boom)
		}
	})
}
