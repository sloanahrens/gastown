package beads

import (
	"context"
	"testing"
	"time"
)

// TestCallTimeSeamRecordsOneCall: one runBdOnce call through a fake bd records
// 300 ms while callNow steps that much per read (gt-aibxst).
//
//testpolicy:allow parallel — it swaps the package-level callNow clock, so no test may run beside it
func TestCallTimeSeamRecordsOneCall(t *testing.T) {
	resetCallTime(t)
	prior := callNow
	//testpolicy:allow no-global-swap — restores the clock seam swapped below
	t.Cleanup(func() { callNow = prior })
	reads := 0
	//testpolicy:allow no-global-swap — the stepping fake clock is the swap this test makes
	callNow = func() time.Time {
		reads++
		return time.Unix(0, 0).Add(time.Duration(reads) * 300 * time.Millisecond)
	}
	b := newRecordedBeads(t.TempDir(), newRecorder(func([]string) reply { return reply{stdout: "[]"} }))
	if _, err := b.runBdOnce(context.Background(), nil, nil, []string{"list", "--json"}); err != nil {
		t.Fatalf("runBdOnce: %v", err)
	}
	if calls, total, slowest := CallTimeSnapshotAndReset(); calls != 1 || total != 300*time.Millisecond || slowest != 300*time.Millisecond {
		t.Fatalf("snapshot = %d calls, %v total, %v slowest; want one call of 300ms", calls, total, slowest)
	}
}
