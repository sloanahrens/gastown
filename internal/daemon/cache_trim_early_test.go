package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/diskmaint/disks"
)

// cacheTrimEarlyLine is the line an early trim logs before it runs, at 76 percent
// against the default 75.
const cacheTrimEarlyLine = "disk_maintenance: cache trim early: sandbox disk 76% >= 75%"

// sandboxAt sets the sandbox disk's reading the patrol sees on its next read.
func sandboxAt(f *diskMaintFakes, used float64) {
	f.readings = []disks.Reading{{Name: diskMaintDiskColima, UsedPct: used}}
}

// TestCacheTrimEarlyRunsWhenTheDiskIsOverThePercent: with the cache trim's
// interval not yet spent, a sandbox disk at 76 percent runs the trim early once
// its last run began over fifteen minutes ago, and logs the early line. At 74
// percent the same pass does not run it.
func TestCacheTrimEarlyRunsWhenTheDiskIsOverThePercent(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	trims := countCacheTrims(d)
	now := diskMaintNow.Add(8 * time.Hour) // 12:45, outside the window: only the trim can be due
	d.diskMaint.now = func() time.Time { return now }
	sandboxAt(f, 76)

	d.runDiskMaintenance() // the trim has never run: due by its interval
	if *trims != 1 {
		t.Fatalf("trimmed %d times on the first tick, want 1: a trim that never ran is due", *trims)
	}

	now = now.Add(20 * time.Minute) // 13:05: the interval is 2h, but the disk is at 76 and the run began 20 min ago
	d.runDiskMaintenance()
	if *trims != 2 {
		t.Fatalf("trimmed %d times at 13:05 with the disk at 76%%, want 2: the early check runs the trim", *trims)
	}
	if got := strings.Count(logs.String(), cacheTrimEarlyLine); got != 1 {
		t.Errorf("early line logged %d times, want 1; log = %q", got, logs.String())
	}

	sandboxAt(f, 74)
	now = now.Add(20 * time.Minute) // 13:25: the gap is open again, but the disk is under the percent
	d.runDiskMaintenance()
	if *trims != 2 {
		t.Fatalf("trimmed %d times with the disk at 74%%, want 2: under the percent, the early check does not run", *trims)
	}
}

// TestCacheTrimEarlyRespectsTheGapAndTheRunningJob: an early request inside
// fifteen minutes of the last start does nothing, and a request at exactly
// fifteen minutes does nothing either, since the gap is more than fifteen. A
// trim whose run concluded without a verdict, the one an earlier Job still
// holds, leaves its retry pending, and the early check does not pre-empt that
// retry or log anything new while it is pending. A trim already holding the
// guard is never joined.
func TestCacheTrimEarlyRespectsTheGapAndTheRunningJob(t *testing.T) {
	t.Parallel()

	t.Run("the gap", func(t *testing.T) {
		t.Parallel()
		d, logs, f := diskMaintTestDaemon(t)
		trims := countCacheTrims(d)
		now := diskMaintNow.Add(8 * time.Hour) // 12:45
		d.diskMaint.now = func() time.Time { return now }
		sandboxAt(f, 76)

		d.runDiskMaintenance() // 12:45: due by its interval, and it begins
		now = now.Add(20 * time.Minute)
		d.runDiskMaintenance() // 13:05: early, begins at 13:05
		if *trims != 2 {
			t.Fatalf("trimmed %d times at 13:05, want 2", *trims)
		}

		// The tick judges the gap before it reads the disk, so a tick inside the
		// gap reads nothing. The alert-only pass would be the only other reader,
		// and it has nothing to do while this fixture's town is quiet.
		reads := 0
		d.diskMaint.readDisks = func(context.Context) []disks.Reading {
			reads++
			return f.readings
		}
		now = now.Add(5 * time.Minute)
		d.runDiskMaintenance() // 13:10: five minutes after the early run began
		now = now.Add(10 * time.Minute)
		d.runDiskMaintenance() // 13:20: exactly fifteen minutes after it began, not more
		if *trims != 2 {
			t.Fatalf("trimmed %d times inside the gap, want 2: a request inside fifteen minutes does nothing", *trims)
		}
		if reads != 0 {
			t.Errorf("disk read %d times inside the gap, want 0: the tick judges the gap before it reads", reads)
		}

		now = now.Add(time.Minute)
		d.runDiskMaintenance() // 13:21: over fifteen minutes after the early run began
		if *trims != 3 {
			t.Fatalf("trimmed %d times at 13:21, want 3: over fifteen minutes after the start, the early check runs again", *trims)
		}
		if got := strings.Count(logs.String(), "cache trim early:"); got != 2 {
			t.Errorf("early line logged %d times, want 2, one per run; log = %q", got, logs.String())
		}
	})

	t.Run("an earlier Job still running", func(t *testing.T) {
		t.Parallel()
		d, logs, f := diskMaintTestDaemon(t)
		now := diskMaintNow.Add(8 * time.Hour) // 12:45
		d.diskMaint.now = func() time.Time { return now }
		runs := 0
		// A trim that runs three minutes and reaches no verdict: the earlier Job
		// is still running, so the run is left for its retry.
		d.diskMaint.trim = func(context.Context, string) bool {
			runs++
			now = now.Add(3 * time.Minute)
			return false
		}
		sandboxAt(f, 76)

		d.runDiskMaintenance() // 12:45: due, and it runs; the retry is 12:48 + 15m = 13:03
		if runs != 1 {
			t.Fatalf("ran %d times on the first tick, want 1", runs)
		}
		logged := logs.Len()

		now = diskMaintNow.Add(8*time.Hour + 16*time.Minute) // 13:01: past the gap, short of the retry
		d.runDiskMaintenance()
		if runs != 1 {
			t.Fatalf("ran %d times at 13:01, want 1: a pending retry is the trim's next run, not the early check's", runs)
		}
		if strings.Contains(logs.String()[logged:], "cache trim early:") {
			t.Errorf("logged an early line while a retry was pending: %q", logs.String()[logged:])
		}
	})

	t.Run("a trim already holding the guard", func(t *testing.T) {
		t.Parallel()
		d, logs, f := diskMaintTestDaemon(t)
		trims := countCacheTrims(d)
		now := diskMaintNow.Add(8 * time.Hour) // 12:45
		d.diskMaint.now = func() time.Time { return now }
		sandboxAt(f, 76)

		d.runDiskMaintenance() // 12:45: due, and it runs
		now = now.Add(20 * time.Minute)
		d.diskMaintTrimRunning.Store(true) // an earlier pass still runs the trim
		d.runDiskMaintenance()
		if *trims != 1 {
			t.Fatalf("trimmed %d times while a trim held the guard, want 1", *trims)
		}
		if strings.Contains(logs.String(), "cache trim early:") {
			t.Errorf("logged an early line while the guard was held: %q", logs.String())
		}

		d.diskMaintTrimRunning.Store(false)
		d.runDiskMaintenance()
		if *trims != 2 {
			t.Fatalf("trimmed %d times once the guard was free, want 2", *trims)
		}
	})
}

// TestCacheTrimEarlyJudgesTheGapAgainUnderTheGuard: the tick judges the gap
// before it reads the disk, and the trim judges it again once it holds the
// guard. A run that began between the two (here, begun by the read itself) is
// not followed by a second one.
func TestCacheTrimEarlyJudgesTheGapAgainUnderTheGuard(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	trims := countCacheTrims(d)
	now := diskMaintNow.Add(8 * time.Hour) // 12:45
	d.diskMaint.now = func() time.Time { return now }
	sandboxAt(f, 76)

	d.runDiskMaintenance() // 12:45: due, and it runs
	now = now.Add(20 * time.Minute)
	read := 0
	d.diskMaint.readDisks = func(context.Context) []disks.Reading {
		read++
		// Another run begins while this read is in flight, after the tick judged
		// the gap open.
		d.diskMaintSchedule.recordStart(diskMaintCacheTrimName, now)
		return f.readings
	}
	d.runDiskMaintenance()

	if read != 1 {
		t.Fatalf("disk read %d times, want 1: the tick reads the disk once", read)
	}
	if *trims != 1 {
		t.Errorf("trimmed %d times, want 1: a run that began during the read is not followed by an early one", *trims)
	}
	if strings.Contains(logs.String(), "cache trim early:") {
		t.Errorf("logged an early line the gap did not allow: %q", logs.String())
	}
}

// TestCacheTrimEarlyReadsTheSetting: cache_trim_early_percent moves the line the
// early check runs at, and a sandbox reading that failed never runs it.
func TestCacheTrimEarlyReadsTheSetting(t *testing.T) {
	t.Parallel()

	t.Run("the setting moves the line", func(t *testing.T) {
		t.Parallel()
		d, _, f := diskMaintTestDaemon(t)
		trims := countCacheTrims(d)
		eighty := 80
		d.patrolConfig.Patrols.DiskMaintenance.CacheTrimEarlyPercent = &eighty
		now := diskMaintNow.Add(8 * time.Hour)
		d.diskMaint.now = func() time.Time { return now }
		sandboxAt(f, 76)

		d.runDiskMaintenance() // 12:45: due
		now = now.Add(20 * time.Minute)
		d.runDiskMaintenance() // 13:05: 76 is under the 80 the setting names
		if *trims != 1 {
			t.Fatalf("trimmed %d times at 76%% under an 80%% setting, want 1", *trims)
		}

		sandboxAt(f, 80)
		d.runDiskMaintenance() // 80 is at the setting
		if *trims != 2 {
			t.Fatalf("trimmed %d times at 80%% under an 80%% setting, want 2", *trims)
		}
	})

	t.Run("an unreadable sandbox disk does not run it", func(t *testing.T) {
		t.Parallel()
		d, _, f := diskMaintTestDaemon(t)
		trims := countCacheTrims(d)
		now := diskMaintNow.Add(8 * time.Hour)
		d.diskMaint.now = func() time.Time { return now }
		sandboxAt(f, 76)

		d.runDiskMaintenance() // 12:45: due
		now = now.Add(20 * time.Minute)
		// A figure past the percent that comes with an error is not trusted: the
		// reading failed, so the early check has nothing to act on.
		f.readings = []disks.Reading{{Name: diskMaintDiskColima, UsedPct: 99, Err: errors.New("node stats unreadable")}}
		d.runDiskMaintenance()
		if *trims != 1 {
			t.Fatalf("trimmed %d times on an unreadable sandbox disk, want 1: the interval still governs", *trims)
		}
	})
}

// TestCacheTrimEarlyRefusesAPercentOutsideTheRange: a percent built in code
// outside 50 to 95 runs nothing early, whatever the disk reads, and is said
// once rather than on every tick. The interval trim is not affected.
func TestCacheTrimEarlyRefusesAPercentOutsideTheRange(t *testing.T) {
	t.Parallel()
	d, logs, f := diskMaintTestDaemon(t)
	trims := countCacheTrims(d)
	ninetySix := 96
	d.patrolConfig.Patrols.DiskMaintenance.CacheTrimEarlyPercent = &ninetySix
	now := diskMaintNow.Add(8 * time.Hour)
	d.diskMaint.now = func() time.Time { return now }
	sandboxAt(f, 99)

	d.runDiskMaintenance() // 12:45: the interval's run, which the setting does not touch
	now = now.Add(20 * time.Minute)
	d.runDiskMaintenance()
	now = now.Add(20 * time.Minute)
	d.runDiskMaintenance()

	if *trims != 1 {
		t.Errorf("trimmed %d times, want 1: a refused percent runs nothing early", *trims)
	}
	if got := strings.Count(logs.String(), "cache_trim_early_percent = 96"); got != 1 {
		t.Errorf("refusal logged %d times, want 1; log = %q", got, logs.String())
	}
}
