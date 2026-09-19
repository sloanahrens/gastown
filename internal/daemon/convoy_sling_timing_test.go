package daemon

import (
	"reflect"
	"testing"
)

// The convoy feeder runs `gt sling` as a subprocess and, until gt-llg8, threw
// its stderr away on success. slingTimingLines keeps only the per-step timing
// lines so the feeder can log them without echoing every warning sling prints.
func TestSlingTimingLinesKeepsOnlyStepLines(t *testing.T) {
	stderr := "⚠ gt binary is 2 commits behind origin/main\n" +
		"[sling] step admission took 1.2s (total 1.2s)\n" +
		"some other diagnostic\n" +
		"[sling] step session took 4m1s (total 11m15s)\n"
	got := slingTimingLines(stderr)
	want := []string{
		"[sling] step admission took 1.2s (total 1.2s)",
		"[sling] step session took 4m1s (total 11m15s)",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSlingTimingLinesEmptyWhenAbsent(t *testing.T) {
	if got := slingTimingLines("nothing timed here\n"); len(got) != 0 {
		t.Fatalf("got %q, want none", got)
	}
}
