package battery

import (
	"fmt"
	"path/filepath"
	"testing"
)

// TestSampledIsDeterministicAndTwoPercent: a key and version sample the same
// way every time, a changed version resamples, and the share sampled is the
// two percent the operator approved.
func TestSampledIsDeterministicAndTwoPercent(t *testing.T) {
	t.Parallel()
	key := "<time> landing_worker: <id>: merge failed at <hash> after <dur>"
	first := Sampled(key, 1)
	for range 100 {
		if got := Sampled(key, 1); got != first {
			t.Fatalf("Sampled(%q, 1) changed its answer between calls", key)
		}
	}

	resampled := false
	for v := 2; v <= 50; v++ {
		if Sampled(key, v) != first {
			resampled = true
			break
		}
	}
	if !resampled {
		t.Errorf("Sampled(%q, version) never changed with the version, so a new question set would keep the old sample", key)
	}

	// The operator approved 2 percent, so 5,000 keys hold 60 to 140 of them.
	const keys = 5000
	sampled := 0
	for i := range keys {
		key := fmt.Sprintf("<time> spec_dispatch: tick: <n> candidate(s), <n> refused, <n> planning #%d", i)
		if Sampled(key, 1) {
			sampled++
		}
	}
	if sampled < 60 || sampled > 140 {
		t.Errorf("sampled %d of %d keys = %.2f%%, want 1.2 to 2.8 percent for an approved 2", sampled, keys, 100*float64(sampled)/keys)
	}
}

// TestReaskPathSitsBesideTheBatteryLog: the repeat log carries the battery
// log's own name and directory, told apart by .reask.
func TestReaskPathSitsBesideTheBatteryLog(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		filepath.Join("/town/.runtime/jev", "logtriage.jsonl"): filepath.Join("/town/.runtime/jev", "logtriage.reask.jsonl"),
		filepath.Join("/town/.runtime/jev", "dedupe.jsonl"):    filepath.Join("/town/.runtime/jev", "dedupe.reask.jsonl"),
	}
	for logPath, want := range cases {
		if got := ReaskPath(logPath); got != want {
			t.Errorf("ReaskPath(%q) = %q, want %q", logPath, got, want)
		}
	}
}
