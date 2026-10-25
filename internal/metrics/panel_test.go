package metrics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// panelGoldenNow is the golden's generated_at, so comparing the built panel
// with the dashboard's file compares every byte.
var panelGoldenNow = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

// panelGoldenResults is the analysis the dashboard's golden panel records: one
// earning target, one with boxes that do not replicate, and one skipped for too
// few training positives.
func panelGoldenResults() []TargetResult {
	return []TargetResult{
		{
			Target: "rejected_before_landing", TrainN: 412, TrainPositives: 48,
			HoldoutN: 96, HoldoutPositives: 24, Boxes: 1, Replicating: 1,
			Best: &BoxCheck{Box: "model = opus and attempts in q4", N: 96, Rate: 0.42, Baseline: 0.25, Replicates: true},
		},
		{
			Target: "manual_rescue", TrainN: 412, TrainPositives: 37,
			HoldoutN: 100, HoldoutPositives: 9, Boxes: 2,
			Best: &BoxCheck{Box: "attempts in q3..q4 and files_touched = none", N: 54, Rate: 0.11, Baseline: 0.09},
		},
		{
			Target: "reached_overrun_stop", TrainN: 412, TrainPositives: 12,
			HoldoutN: 100, HoldoutPositives: 3,
		},
	}
}

// TestVerdict is the verdict rule: a target is not-yet while either split has
// too few positives, earning when one box replicated, and no-signal when boxes
// were tested and none did.
func TestVerdict(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		target       TargetResult
		minPositives int
		want         string
	}{
		{"too few train positives", TargetResult{TrainPositives: 29, HoldoutPositives: 10, Replicating: 1}, 30, VerdictNotYet},
		{"no holdout positives", TargetResult{TrainPositives: 40, HoldoutPositives: 0, Replicating: 1}, 30, VerdictNotYet},
		{"one replicating box", TargetResult{TrainPositives: 40, HoldoutPositives: 5, Boxes: 2, Replicating: 1}, 30, VerdictEarning},
		{"boxes but none replicate", TargetResult{TrainPositives: 40, HoldoutPositives: 5, Boxes: 3}, 30, VerdictNoSignal},
		{"no boxes with enough positives", TargetResult{TrainPositives: 40, HoldoutPositives: 5}, 30, VerdictNoSignal},
		{"the caller's minimum decides", TargetResult{TrainPositives: 12, HoldoutPositives: 5, Replicating: 1}, 10, VerdictEarning},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Verdict(tc.target, tc.minPositives); got != tc.want {
				t.Errorf("Verdict(%+v, %d) = %q, want %q", tc.target, tc.minPositives, got, tc.want)
			}
		})
	}
}

// TestBuildPanelMatchesTheDashboardGolden builds the panel the dashboard's
// testdata file records and requires the same bytes: the writer and the reader
// share one schema file rather than two copies that can drift.
func TestBuildPanelMatchesTheDashboardGolden(t *testing.T) {
	t.Parallel()

	want, err := os.ReadFile(filepath.Join("..", "dashboard", "testdata", "mango_panel.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.MarshalIndent(BuildPanel(panelGoldenResults(), PanelMeta{Items: 412, SnapshotHours: 168}, panelGoldenNow), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if string(got) != string(want) {
		t.Errorf("the built panel differs from the dashboard's golden:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestBuildPanelMarksATargetSkippedForTooFewPositives keeps a target with fewer
// than MangoMinSupport training positives in the panel as not-yet with no box:
// it was skipped, not tested and found wanting.
func TestBuildPanelMarksATargetSkippedForTooFewPositives(t *testing.T) {
	t.Parallel()

	skipped := TargetResult{
		Target: "reached_overrun_stop", TrainPositives: 12, HoldoutN: 100, HoldoutPositives: 3,
		Best: &BoxCheck{Box: "a box it cannot have", N: 40, Rate: 0.5},
	}
	p := BuildPanel([]TargetResult{skipped}, PanelMeta{}, panelGoldenNow)
	if len(p.Targets) != 1 {
		t.Fatalf("%d targets, want 1", len(p.Targets))
	}
	got := p.Targets[0]
	want := PanelTarget{Target: "reached_overrun_stop", Positives: 12, Skipped: true, Baseline: 0.03, Verdict: VerdictNotYet}
	if got != want {
		t.Errorf("a skipped target writes %+v, want %+v", got, want)
	}
}

// TestPublishAtomicReplacesTheFileAndLeavesNoTemp writes, then replaces, a
// file and requires the second content in the one file the directory holds:
// the rename is what makes a reader see a whole panel.
func TestPublishAtomicReplacesTheFileAndLeavesNoTemp(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	for _, data := range []string{"first", "second"} {
		if err := PublishAtomic(dir, "panel.json", []byte(data)); err != nil {
			t.Fatalf("publish %q: %v", data, err)
		}
		got, err := os.ReadFile(filepath.Join(dir, "panel.json"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != data {
			t.Errorf("panel.json holds %q, want %q", got, data)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "panel.json" {
		t.Errorf("the directory holds %v, want only panel.json", entries)
	}
}

// TestPublishAtomicFailsWithoutLeavingATempFile covers the two ways a publish
// stops short: a directory that cannot hold the temporary file and a target
// that cannot be renamed over. Neither leaves a temporary file behind.
func TestPublishAtomicFailsWithoutLeavingATempFile(t *testing.T) {
	t.Parallel()

	t.Run("no such directory", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "missing")
		if err := PublishAtomic(dir, "panel.json", []byte("x")); err == nil {
			t.Fatal("publishing into a missing directory succeeded")
		}
	})

	t.Run("target is a directory", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "panel.json"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := PublishAtomic(dir, "panel.json", []byte("x")); err == nil {
			t.Fatal("replacing a directory succeeded")
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "panel.json" {
			t.Errorf("a failed publish left %v, want only panel.json", entries)
		}
	})
}
