package forgejoprobe

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestSystemFSRefusesAMissingDirectory holds the operating-system adapter's failure
// path: a temporary file cannot be made in a directory that is not there, so the
// adapter returns the error and no file. The directory is never created.
func TestSystemFSRefusesAMissingDirectory(t *testing.T) {
	t.Parallel()
	f, err := osFS{}.CreateTemp(filepath.Join(t.TempDir(), "absent"), ".probe-rig.*")
	if err == nil {
		_ = f.Close()
		t.Fatalf("CreateTemp in a missing directory = nil error, want one")
	}
	if f != nil {
		t.Errorf("CreateTemp returned a file alongside its error")
	}
}

// TestSystemClockSleepEndsWhenCanceled holds the production clock to the Clock
// contract: a sleep under a context that is already done returns without waiting
// out its duration. The bound stops a regression from hanging the run.
func TestSystemClockSleepEndsWhenCanceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		systemClock{}.Sleep(ctx, time.Hour)
		close(done)
	}()
	bound, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()
	select {
	case <-done:
	case <-bound.Done():
		t.Fatal("a sleep of an hour under a canceled context did not return")
	}
}
