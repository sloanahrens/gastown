package cmd

import (
	"strings"
	"testing"
	"time"
)

// The tick's cost notice carries the bd subprocess time it read at the end (gt-d2hzix).
//
//testpolicy:allow no-global-swap — the seam is the swap this test makes; the tick cannot take it as an argument
//testpolicy:allow parallel — it swaps the package-level tickCallTime seam, so no test may run beside it
func TestSpecTickBdTimeIsOnTheCostNotice(t *testing.T) {
	root := poolTestTown(t, specTickSettings, specTickDaemon)
	prior := tickCallTime
	t.Cleanup(func() {
		//testpolicy:allow no-global-swap — restores the prior seam value swapped above
		tickCallTime = prior
	})
	//testpolicy:allow no-global-swap — the fake reading is the swap this test makes
	tickCallTime = func() (int, time.Duration, time.Duration) {
		return 7, 2100 * time.Millisecond, 900 * time.Millisecond
	}
	report, err := runSpecTickForTest(t, root, "", false)
	if err != nil {
		t.Fatalf("runSpecTickForTest: %v", err)
	}
	want := "bd time summed 2.1s in 7 subprocess(es)"
	if !strings.Contains(strings.Join(report.Notices, "\n"), want) {
		t.Fatalf("notices %q do not carry %q", report.Notices, want)
	}
}
