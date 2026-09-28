//go:build !windows

package tmux

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"
)

func TestAcquireFlockLockTimesOutOnFakeClock(t *testing.T) {
	t.Parallel()
	lock := filepath.Join(t.TempDir(), "n.lock")
	release, err := acquireFlockLock(clockwork.NewRealClock(), lock, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	clk := clockwork.NewFakeClock()
	done := make(chan error, 1)
	go func() {
		_, err := acquireFlockLock(clk, lock, 5*time.Second)
		done <- err
	}()
	err = driveClock(t, clk, 100*time.Millisecond, done)
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("err = %v, want timeout", err)
	}
}
