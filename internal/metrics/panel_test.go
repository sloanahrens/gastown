package metrics

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// panelGoldenNow is the golden's generated_at, so comparing the built panel
// with the dashboard's file compares every byte.
var panelGoldenNow = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

// panelGoldenResults is the analysis the dashboard's golden panel records: one
// earning target, one with a box that does not replicate, and one skipped for
// too few training positives. Each box's K and N are what its rate is, and K
// stays within the target's own holdout positives.
func panelGoldenResults() []TargetResult {
	return []TargetResult{
		{
			Target: "rejected_before_landing", TrainN: 412, TrainPositives: 48,
			HoldoutN: 96, HoldoutPositives: 24, Boxes: 1, Replicating: 1,
			Best: &BoxCheck{Box: "model = opus and attempts in q4", N: 50, K: 21, Rate: 0.42, Baseline: 0.25, Replicates: true},
		},
		{
			Target: "manual_rescue", TrainN: 412, TrainPositives: 37,
			HoldoutN: 100, HoldoutPositives: 9, Boxes: 2,
			Best: &BoxCheck{Box: "attempts in q3..q4 and files_touched = none", N: 49, K: 5, Rate: 5.0 / 49.0, Baseline: 0.09},
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
	want := PanelTarget{
		Target: "reached_overrun_stop", Label: "Beads stopped by the overrun guard",
		Positives: 12, Skipped: true, Baseline: 0.03, Verdict: VerdictNotYet, Reason: ReasonFewPositives,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("a skipped target writes %+v, want %+v", got, want)
	}
}

// TestPanelLabel is the plain-words title per target, and the id for a target
// the panel has no title for.
func TestPanelLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		target string
		want   string
	}{
		{"rejected_before_landing", "Landings rejected before they land"},
		{"reached_overrun_stop", "Beads stopped by the overrun guard"},
		{"manual_rescue", "Landings that needed a manual rescue"},
		{"a_future_target", "a_future_target"},
	}
	for _, tc := range tests {
		t.Run(tc.target, func(t *testing.T) {
			t.Parallel()
			if got := panelLabel(tc.target); got != tc.want {
				t.Errorf("panelLabel(%q) = %q, want %q", tc.target, got, tc.want)
			}
		})
	}
}

// TestPanelTargetThin is the thin flag's boundary: a box under MangoThinHoldout
// holdout rows is thin, and one at the constant is not.
func TestPanelTargetThin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		n    int
		want bool
	}{
		{"one under the boundary", MangoThinHoldout - 1, true},
		{"at the boundary", MangoThinHoldout, false},
		{"one over the boundary", MangoThinHoldout + 1, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := panelTarget(TargetResult{
				Target: "manual_rescue", TrainPositives: 40, HoldoutN: 100, HoldoutPositives: 10,
				Best: &BoxCheck{Box: "a box", N: tc.n, K: 1, Rate: fraction(1, tc.n)},
			})
			if got.Thin == nil {
				t.Fatal("a target with a box writes no thin flag")
			}
			if *got.Thin != tc.want {
				t.Errorf("thin for N=%d is %v, want %v", tc.n, *got.Thin, tc.want)
			}
		})
	}
}

// TestPanelTargetReason names why a target is not-yet, and writes no reason for
// a target whose verdict was reached.
func TestPanelTargetReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		target TargetResult
		want   string
	}{
		{"too few training positives", TargetResult{Target: "reached_overrun_stop", TrainPositives: MangoMinSupport - 1, HoldoutN: 100, HoldoutPositives: 5}, ReasonFewPositives},
		{"no holdout positives", TargetResult{Target: "manual_rescue", TrainPositives: MangoMinSupport, HoldoutN: 100}, ReasonNoHoldoutPositives},
		{"earning", TargetResult{Target: "rejected_before_landing", TrainPositives: 40, HoldoutN: 100, HoldoutPositives: 10, Replicating: 1}, ""},
		{"no-signal", TargetResult{Target: "manual_rescue", TrainPositives: 40, HoldoutN: 100, HoldoutPositives: 10, Boxes: 1}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := panelTarget(tc.target); got.Reason != tc.want {
				t.Errorf("reason for %+v = %q, want %q", tc.target, got.Reason, tc.want)
			}
		})
	}
}

// TestPanelTargetBoxKeys writes the best box's positives, its interval and its
// thin flag, and leaves the interval absent when the box matched no holdout
// rows and there is nothing to estimate.
func TestPanelTargetBoxKeys(t *testing.T) {
	t.Parallel()

	box := func(n, k int) *BoxCheck {
		return &BoxCheck{Box: "a box", N: n, K: k, Rate: fraction(k, n)}
	}
	withRows := panelTarget(TargetResult{
		Target: "manual_rescue", TrainPositives: 40, HoldoutN: 100, HoldoutPositives: 10,
		Best: box(36, 15),
	})
	if withRows.HoldoutPositives == nil || *withRows.HoldoutPositives != 15 {
		t.Errorf("holdout positives = %v, want 15", withRows.HoldoutPositives)
	}
	wantLow, wantHigh, ok := wilsonInterval(15, 36)
	if !ok {
		t.Fatal("wilsonInterval(15, 36) has no interval")
	}
	if withRows.CILow == nil || withRows.CIHigh == nil {
		t.Fatal("a box with holdout rows writes no interval")
	}
	if *withRows.CILow != wantLow || *withRows.CIHigh != wantHigh {
		t.Errorf("interval = %v..%v, want %v..%v", *withRows.CILow, *withRows.CIHigh, wantLow, wantHigh)
	}

	noRows := panelTarget(TargetResult{
		Target: "manual_rescue", TrainPositives: 40, HoldoutN: 100, HoldoutPositives: 10,
		Best: box(0, 0),
	})
	if noRows.CILow != nil || noRows.CIHigh != nil {
		t.Errorf("a box with no holdout rows writes the interval %v..%v, want none", noRows.CILow, noRows.CIHigh)
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
