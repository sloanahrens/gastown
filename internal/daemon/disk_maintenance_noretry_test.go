package daemon

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestCacheTrimNoRetryAfterAFailedTrim pins the rule this bead removed the
// 15-minute retry for: a trim that returns false records its conclusion at the
// full interval like one that reached a verdict. No retry time is left behind,
// nothing is due at the old retry instant, and the trim next runs one interval
// after it failed (gt-wkjdql).
func TestCacheTrimNoRetryAfterAFailedTrim(t *testing.T) {
	t.Parallel()
	d, _, _ := diskMaintTestDaemon(t)
	var runs atomic.Int32
	d.diskMaint.trim = func(context.Context, string) bool {
		runs.Add(1)
		return false
	}
	now := diskMaintNow
	d.diskMaint.now = func() time.Time { return now }

	d.runDiskMaintenance() // 04:45: the trim fails
	if got := runs.Load(); got != 1 {
		t.Fatalf("trimmed %d times at the first tick, want 1", got)
	}
	if got := d.diskMaintSchedule.retryAtOf(diskMaintCacheTrimName); !got.IsZero() {
		t.Fatalf("cache trim retry at = %v, want none: a failed trim asks for no retry", got)
	}
	if got, want := d.diskMaintSchedule.lastRunAt(diskMaintCacheTrimName), diskMaintNow; !got.Equal(want) {
		t.Fatalf("cache trim last run = %v, want the failed run's conclusion %v: a failed trim records the full-interval verdict", got, want)
	}

	now = diskMaintNow.Add(cacheTrimRetryInterval) // 05:00: the retry the failure used to ask for
	d.runDiskMaintenance()
	if got := runs.Load(); got != 1 {
		t.Errorf("trimmed %d times at the old retry instant, want 1: a failed trim is not due before the interval", got)
	}

	now = diskMaintNow.Add(2*time.Hour - time.Minute) // 06:44
	d.runDiskMaintenance()
	if got := runs.Load(); got != 1 {
		t.Errorf("trimmed %d times a minute short of the interval, want 1", got)
	}

	now = diskMaintNow.Add(2 * time.Hour) // 06:45: one interval after the failed run concluded
	d.runDiskMaintenance()
	if got := runs.Load(); got != 2 {
		t.Errorf("trimmed %d times one interval after the failed run, want 2: the full interval decides", got)
	}
}
