package main

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"

	"github.com/steveyegge/gastown/internal/tiersweep"
)

// progressCycle is the order the runner's Job runs the tiers after shell.
var progressCycle = []string{"integration", "race"}

// summaryLine is the line tiersweep.Summary prints for a tier, so every fixture
// is the production format rather than a copy of it.
func summaryLine(tier string, passed, failed, skipped int, names string, secs int) string {
	line, _ := tiersweep.Summary(tier, "/work/logs", passed, failed, skipped, names, secs)
	return line
}

func TestProgressNewStartsOnFirstTier(t *testing.T) {
	t.Parallel()

	p := NewProgress("gastown", testSHA, progressCycle)
	if p.Running != "integration" {
		t.Errorf("Running = %q, want %q", p.Running, "integration")
	}
	if len(p.Tiers) != 0 {
		t.Errorf("Tiers = %v, want none finished", p.Tiers)
	}
	if p.Rig != "gastown" || p.SHA != testSHA {
		t.Errorf("Rig, SHA = %q, %q; want gastown, %s", p.Rig, p.SHA, testSHA)
	}
}

func TestProgressGreenIntegrationAfterShellAdvancesToRace(t *testing.T) {
	t.Parallel()

	p := NewProgress("gastown", testSHA, progressCycle)
	if p.Feed(summaryLine("shell", 3, 0, 0, "", 2)) {
		t.Fatal("a shell line outside the cycle changed the record")
	}
	if p.Running != "integration" {
		t.Fatalf("Running = %q after shell, want it to stay on integration", p.Running)
	}
	if !p.Feed(summaryLine("integration", 12, 0, 1, "", 45)) {
		t.Fatal("the integration summary line was not recorded")
	}
	want := []ProgressTier{{Tier: "integration", Verdict: "GREEN", Passed: 12, Failed: 0, Skipped: 1, Secs: 45}}
	if !slices.Equal(p.Tiers, want) {
		t.Errorf("Tiers = %+v, want %+v", p.Tiers, want)
	}
	if p.Running != "race" {
		t.Errorf("Running = %q, want %q", p.Running, "race")
	}
}

func TestProgressRedTierRecordsVerdictAndFailedCount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
		want ProgressTier
	}{
		{
			name: "failed tier names its failures",
			line: summaryLine("integration", 10, 2, 0, "TestParse TestLoad", 1129),
			want: ProgressTier{Tier: "integration", Verdict: "RED", Passed: 10, Failed: 2, Skipped: 0, Secs: 1129},
		},
		{
			name: "tier that counted nothing is RED",
			line: summaryLine("integration", 0, 0, 0, "", 5),
			want: ProgressTier{Tier: "integration", Verdict: "RED", Passed: 0, Failed: 0, Skipped: 0, Secs: 5},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := NewProgress("gastown", testSHA, progressCycle)
			if !p.Feed(tc.line) {
				t.Fatalf("Feed(%q) reported no change", tc.line)
			}
			if !slices.Equal(p.Tiers, []ProgressTier{tc.want}) {
				t.Errorf("Tiers = %+v, want %+v", p.Tiers, []ProgressTier{tc.want})
			}
			if p.Running != "race" {
				t.Errorf("Running = %q, want %q", p.Running, "race")
			}
		})
	}
}

func TestProgressLastTierLeavesRunningEmpty(t *testing.T) {
	t.Parallel()

	p := NewProgress("gastown", testSHA, progressCycle)
	p.Feed(summaryLine("integration", 1, 0, 0, "", 1))
	p.Feed(summaryLine("race", 4, 0, 0, "", 300))
	if p.Running != "" {
		t.Errorf("Running = %q after the last tier, want empty", p.Running)
	}
	if len(p.Tiers) != 2 {
		t.Errorf("finished tiers = %d, want 2", len(p.Tiers))
	}
}

func TestProgressReadsDurationAsSeconds(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
		want float64
	}{
		{"minutes and seconds", "tier-sweep: race GREEN passed=1 failed=0 skipped=0 (logs /w) in 18m49s", 1129},
		{"whole hour", "tier-sweep: race GREEN passed=1 failed=0 skipped=0 (logs /w) in 1h0m0s", 3600},
		{"seconds only", "tier-sweep: race GREEN passed=1 failed=0 skipped=0 (logs /w) in 45s", 45},
		{"generated hour", summaryLine("race", 1, 0, 0, "", 3600), 3600},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := NewProgress("gastown", testSHA, []string{"race"})
			if !p.Feed(tc.line) {
				t.Fatalf("Feed(%q) reported no change", tc.line)
			}
			if got := p.Tiers[0].Secs; got != tc.want {
				t.Errorf("Secs = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestProgressIgnoresLinesItDoesNotRecord covers each refusal: the record is
// unchanged and Running stays where it was.
func TestProgressIgnoresLinesItDoesNotRecord(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup []string
		line  string
	}{
		{name: "blank line", line: ""},
		{name: "sweep progress output", line: "=== RUN   TestParse"},
		{name: "rig script line without the tail", line: "tier-sweep: integration GREEN passed=1"},
		{name: "unparseable duration", line: "tier-sweep: integration GREEN passed=1 failed=0 skipped=0 (logs /w) in soon"},
		{name: "count overflows", line: "tier-sweep: integration GREEN passed=99999999999999999999 failed=0 skipped=0 (logs /w) in 1s"},
		{name: "tier outside the cycle", line: summaryLine("lint", 1, 0, 0, "", 2)},
		{name: "duplicate of a finished tier", setup: []string{summaryLine("integration", 1, 0, 0, "", 2)}, line: summaryLine("integration", 0, 1, 0, "TestX", 3)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := NewProgress("gastown", testSHA, progressCycle)
			for _, l := range tc.setup {
				p.Feed(l)
			}
			before, err := p.JSON()
			if err != nil {
				t.Fatal(err)
			}
			if p.Feed(tc.line) {
				t.Fatalf("Feed(%q) reported a change", tc.line)
			}
			after, err := p.JSON()
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Errorf("record changed:\nbefore %s\nafter  %s", before, after)
			}
		})
	}
}

func TestProgressJSONRoundTrips(t *testing.T) {
	t.Parallel()

	p := NewProgress("gastown", testSHA, progressCycle)
	p.Feed(summaryLine("integration", 12, 1, 0, "TestParse", 1129))
	want, err := p.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var got Progress
	if err := json.Unmarshal(want, &got); err != nil {
		t.Fatalf("unmarshal %s: %v", want, err)
	}
	if got.Rig != p.Rig || got.SHA != p.SHA || got.Running != p.Running || !slices.Equal(got.Tiers, p.Tiers) {
		t.Errorf("decoded %+v, want %+v", got, *p)
	}
	again, err := got.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want, again) {
		t.Errorf("re-encoded %s, want %s", again, want)
	}
}
