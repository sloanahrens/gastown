package tiersweep

import (
	"regexp"
	"testing"
)

// summaryRE is a copy of the daemon's tierSweepSummaryRE
// (internal/daemon/tier_sweep.go), which parses these lines byte for byte.
const summaryRE = `^tier-sweep: (\S+) (GREEN|RED) passed=(\d+) failed=(\d+) skipped=(\d+)(?: failed:(.*?))?(?: \(logs ([^)]*)\))?(?: in (\S+))?\s*$`

// TestSummaryLines: each verdict's line byte for byte, and the daemon's regexp
// over it, whose groups are tier, verdict, passed, failed, skipped, failed
// names, log directory and duration.
func TestSummaryLines(t *testing.T) {
	t.Parallel()
	const logDir = "/tmp/tier-sweep.aB12"
	cases := []struct {
		name      string
		tier      string
		passed    int
		failed    int
		skipped   int
		names     string
		seconds   int
		want      string
		wantRed   bool
		wantGroup []string
	}{
		{
			name:      "a green tier",
			tier:      TierShell,
			passed:    5,
			seconds:   45,
			want:      "tier-sweep: shell GREEN passed=5 failed=0 skipped=0 (logs " + logDir + ") in 45s",
			wantGroup: []string{"shell", "GREEN", "5", "0", "0", "", logDir, "45s"},
		},
		{
			name:    "a red tier names its failures",
			tier:    TierShell,
			passed:  5,
			failed:  1,
			names:   "scripts/x.sh",
			seconds: 134,
			want:    "tier-sweep: shell RED passed=5 failed=1 skipped=0 failed: scripts/x.sh (logs " + logDir + ") in 2m14s",
			wantRed: true,
			// The space after "failed:" is in the capture; the daemon
			// splits it with strings.Fields.
			wantGroup: []string{"shell", "RED", "5", "1", "0", " scripts/x.sh", logDir, "2m14s"},
		},
		{
			name:      "a red tier with no names",
			tier:      TierRace,
			failed:    1,
			seconds:   1,
			want:      "tier-sweep: race RED passed=0 failed=1 skipped=0 (logs " + logDir + ") in 1s",
			wantRed:   true,
			wantGroup: []string{"race", "RED", "0", "1", "0", "", logDir, "1s"},
		},
		{
			name:      "an empty tier is red, never green (gt-tqxdd)",
			tier:      TierIntegration,
			want:      "tier-sweep: integration RED passed=0 failed=0 skipped=0 (logs " + logDir + ") in 0s",
			wantRed:   true,
			wantGroup: []string{"integration", "RED", "0", "0", "0", "", logDir, "0s"},
		},
		{
			name:      "skips alone are green",
			tier:      TierRace,
			skipped:   3,
			seconds:   2,
			want:      "tier-sweep: race GREEN passed=0 failed=0 skipped=3 (logs " + logDir + ") in 2s",
			wantGroup: []string{"race", "GREEN", "0", "0", "3", "", logDir, "2s"},
		},
		{
			name:      "several failures over an hour read as 1h1m1s",
			tier:      TierIntegration,
			passed:    2,
			failed:    2,
			skipped:   1,
			names:     "./a ./b",
			seconds:   3661,
			want:      "tier-sweep: integration RED passed=2 failed=2 skipped=1 failed: ./a ./b (logs " + logDir + ") in 1h1m1s",
			wantRed:   true,
			wantGroup: []string{"integration", "RED", "2", "2", "1", " ./a ./b", logDir, "1h1m1s"},
		},
	}
	re := regexp.MustCompile(summaryRE)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			line, red := Summary(tc.tier, logDir, tc.passed, tc.failed, tc.skipped, tc.names, tc.seconds)
			if line != tc.want {
				t.Errorf("line = %q, want %q", line, tc.want)
			}
			if red != tc.wantRed {
				t.Errorf("red = %t, want %t", red, tc.wantRed)
			}
			m := re.FindStringSubmatch(line)
			if m == nil {
				t.Fatalf("the daemon's regexp does not parse %q", line)
			}
			if len(m)-1 != len(tc.wantGroup) {
				t.Fatalf("the daemon's regexp found %d groups, want %d", len(m)-1, len(tc.wantGroup))
			}
			for i, want := range tc.wantGroup {
				if m[i+1] != want {
					t.Errorf("group %d = %q, want %q", i+1, m[i+1], want)
				}
			}
		})
	}
}

// TestFormatDurationTable: whole seconds in the daemon log's rounded style,
// with the minutes kept at one hour and beyond (1h0m0s, not 1h).
func TestFormatDurationTable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		seconds int
		want    string
	}{
		{seconds: 0, want: "0s"},
		{seconds: 45, want: "45s"},
		{seconds: 59, want: "59s"},
		{seconds: 60, want: "1m0s"},
		{seconds: 134, want: "2m14s"},
		{seconds: 3599, want: "59m59s"},
		{seconds: 3600, want: "1h0m0s"},
		{seconds: 3661, want: "1h1m1s"},
	}
	for _, tc := range cases {
		if got := FormatDuration(tc.seconds); got != tc.want {
			t.Errorf("FormatDuration(%d) = %q, want %q", tc.seconds, got, tc.want)
		}
	}
}
