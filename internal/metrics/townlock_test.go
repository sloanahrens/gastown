package metrics

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTownLockIsHeldUntilReleased(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "town-metrics.lock")
	first, err := AcquireTownLock(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	// A second open file description conflicts with the first even in this
	// process, which is what a second run is. The non-blocking flock makes it
	// return ErrLockHeld rather than wait.
	if _, err := AcquireTownLock(path); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("second acquire while held: err = %v, want ErrLockHeld", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	third, err := AcquireTownLock(path)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	if err := third.Release(); err != nil {
		t.Fatalf("release the third: %v", err)
	}
}

func TestTownLockRecordsTheHolderPid(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "town-metrics.lock")
	lock, err := AcquireTownLock(path)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lock.Release()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the lock file: %v", err)
	}
	if want := fmt.Sprintf("%d\n", os.Getpid()); string(data) != want {
		t.Errorf("lock file = %q, want the holder's pid %q", data, want)
	}
}

func TestTownLockIsNotBlockedByAPidLeftByADeadRun(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "town-metrics.lock")
	// A run the daemon killed leaves its pid in the file and no lock on it.
	if err := os.WriteFile(path, []byte("999999\n"), 0o644); err != nil {
		t.Fatalf("write the leftover: %v", err)
	}
	lock, err := AcquireTownLock(path)
	if err != nil {
		t.Fatalf("acquire over a dead run's leftover: %v", err)
	}
	defer lock.Release()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the lock file: %v", err)
	}
	if strings.Contains(string(data), "999999") {
		t.Errorf("lock file still names the dead pid: %q", data)
	}
}
