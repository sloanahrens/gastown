package townflight

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

func TestWaitEnded(t *testing.T) {
	t.Parallel()
	t.Run("blocks until the entry is released and leaves no file", func(t *testing.T) {
		t.Parallel()
		e, err := createEntry(t.TempDir(), "gt status")
		if err != nil {
			t.Fatalf("createEntry: %v", err)
		}
		started, done := make(chan struct{}), make(chan error, 1)
		newLock := func(path string) entryLock { return &signalledLock{flock.New(path), started} }
		go func() { done <- waitEndedWith(newLock, os.Remove, e.path) }()
		guard := time.After(10 * time.Second) //testpolicy:allow no-sleep — hang guard only
		select {
		case <-started:
		case <-guard:
			t.Fatal("waitEnded did not reach the entry's lock")
		}
		select {
		case err := <-done:
			t.Fatalf("waitEnded returned %v while the entry was held", err)
		default:
		}
		if err := e.drop(); err != nil {
			t.Fatalf("drop: %v", err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("waitEnded: %v", err)
			}
		case <-guard:
			t.Fatal("waitEnded did not return after the release")
		}
		if _, err := os.Stat(e.path); !os.IsNotExist(err) {
			t.Errorf("Stat after waitEnded = %v, want not exist", err)
		}
	})
	boom := errors.New("boom")
	lock := &unlockingLock{}
	if err := waitEndedWith(func(string) entryLock { return lock }, func(string) error { return boom }, "unused"); !errors.Is(err, boom) || lock.unlocks != 1 {
		t.Errorf("removal error = %v, unlocks = %d; want the wrapped %v and one unlock", err, lock.unlocks, boom)
	}
	lock = &unlockingLock{unlockErr: boom}
	if err := waitEndedWith(func(string) entryLock { return lock }, func(string) error { return nil }, "unused"); !errors.Is(err, boom) {
		t.Errorf("unlock error = %v, want it to wrap %v", err, boom)
	}
	if err := waitEndedWith(func(string) entryLock { return &unlockingLock{} }, func(string) error { return os.ErrNotExist }, "gone"); err != nil {
		t.Errorf("waitEnded on a removed entry = %v, want nil", err)
	}
	if err := waitEnded(filepath.Join(t.TempDir(), "missing", "e.cmd")); err == nil {
		t.Error("waitEnded below a missing dir returned no error")
	}
}

type signalledLock struct {
	*flock.Flock
	started chan struct{}
}

func (s *signalledLock) Lock() error {
	close(s.started)
	return s.Flock.Lock()
}

type unlockingLock struct {
	unlockErr error
	unlocks   int
}

func (l *unlockingLock) Lock() error   { return nil }
func (l *unlockingLock) Unlock() error { l.unlocks++; return l.unlockErr }
