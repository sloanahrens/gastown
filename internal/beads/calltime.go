package beads

import (
	"sync/atomic"
	"time"
)

// The call-time totals add up what this package spends inside bd subprocesses
// since the last reset, which the gateway's per-call log cannot (gt-9u3mvp).
var (
	callTimeCalls   atomic.Int64
	callTimeTotal   atomic.Int64 // nanoseconds
	callTimeSlowest atomic.Int64 // nanoseconds
)

// CallTimeRecord counts one call, adds d to the total and raises the slowest.
func CallTimeRecord(d time.Duration) {
	callTimeCalls.Add(1)
	callTimeTotal.Add(int64(d))
	for slowest := callTimeSlowest.Load(); int64(d) > slowest; slowest = callTimeSlowest.Load() {
		if callTimeSlowest.CompareAndSwap(slowest, int64(d)) {
			return
		}
	}
}

// CallTimeSnapshotAndReset returns the calls recorded since the last reset, the
// total time they took and the slowest of them, and zeroes all three.
func CallTimeSnapshotAndReset() (calls int, total, slowest time.Duration) {
	calls = int(callTimeCalls.Swap(0))
	total = time.Duration(callTimeTotal.Swap(0))
	slowest = time.Duration(callTimeSlowest.Swap(0))
	return calls, total, slowest
}
