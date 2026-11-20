package beads

import (
	"sync"
	"testing"
	"time"
)

// resetCallTime zeroes the process-wide totals around a test so its readings
// are its own.
func resetCallTime(t *testing.T) {
	t.Helper()
	CallTimeSnapshotAndReset()
	t.Cleanup(func() { CallTimeSnapshotAndReset() })
}

// TestCallTimeSnapshotSumsAndResets records 300 ms then 100 ms: 2 calls, 400 ms
// total, 300 ms slowest, and the next snapshot reads zeros.
func TestCallTimeSnapshotSumsAndResets(t *testing.T) {
	resetCallTime(t)
	CallTimeRecord(300 * time.Millisecond)
	CallTimeRecord(100 * time.Millisecond)

	if calls, total, slowest := CallTimeSnapshotAndReset(); calls != 2 || total != 400*time.Millisecond || slowest != 300*time.Millisecond {
		t.Fatalf("snapshot = %d calls, %v total, %v slowest; want 2, 400ms, 300ms", calls, total, slowest)
	}
	if calls, total, slowest := CallTimeSnapshotAndReset(); calls != 0 || total != 0 || slowest != 0 {
		t.Fatalf("second snapshot = %d calls, %v total, %v slowest; want zeros", calls, total, slowest)
	}
}

// TestCallTimeRecordIsSafeConcurrently records from 8 goroutines at once: every
// call must be counted and its time summed.
func TestCallTimeRecordIsSafeConcurrently(t *testing.T) {
	resetCallTime(t)
	const goroutines, perGoroutine, each = 8, 25, 2 * time.Millisecond

	var wg sync.WaitGroup
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range perGoroutine {
				CallTimeRecord(each)
			}
		}()
	}
	wg.Wait()

	calls, total, slowest := CallTimeSnapshotAndReset()
	if want := goroutines * perGoroutine; calls != want || total != time.Duration(want)*each || slowest != each {
		t.Fatalf("snapshot = %d calls, %v total, %v slowest; want %d, %v, %v", calls, total, slowest, want, time.Duration(want)*each, each)
	}
}
