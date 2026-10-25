package metrics

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The panel's verdict for a target: earning replicated on the holdout, not-yet
// has too few positives to test, and no-signal tested and found nothing.
const (
	VerdictEarning  = "earning"
	VerdictNoSignal = "no-signal"
	VerdictNotYet   = "not-yet"
)

// panelSchema is the shape of the panel file. A changed field set is a new
// version, because the dashboard refuses a schema it does not know.
const panelSchema = 1

// PanelMeta is the panel's context outside its targets: how many landings the
// analysis read and how many hours of snapshots it joined.
type PanelMeta struct {
	Items         int
	SnapshotHours int
}

// Panel is the panel.json the dashboard reads. Its keys are the dashboard
// reader's contract (internal/dashboard/mango.go); a test compares the marshal
// of BuildPanel's output with internal/dashboard/testdata/mango_panel.json, so
// the two files stay one schema.
type Panel struct {
	SchemaVersion int           `json:"schema_version"`
	GeneratedAt   string        `json:"generated_at"`
	Items         int           `json:"items"`
	SnapshotHours int           `json:"snapshot_hours"`
	Targets       []PanelTarget `json:"targets"`
}

// PanelTarget is one target's line in the panel. A skipped target carries its
// name and positives with no box, because it was never run.
type PanelTarget struct {
	Target      string  `json:"target"`
	Positives   int     `json:"positives"`
	Skipped     bool    `json:"skipped"`
	BestBox     string  `json:"best_box"`
	HoldoutN    int     `json:"holdout_n"`
	HoldoutRate float64 `json:"holdout_rate"`
	Baseline    float64 `json:"baseline"`
	Verdict     string  `json:"verdict"`
}

// Verdict returns the panel's verdict for a target: not-yet while the training
// split holds fewer than minPositives positives or the holdout holds none,
// earning when at least one box replicated, and no-signal otherwise.
func Verdict(t TargetResult, minPositives int) string {
	if t.TrainPositives < minPositives || t.HoldoutPositives == 0 {
		return VerdictNotYet
	}
	if t.Replicating > 0 {
		return VerdictEarning
	}
	return VerdictNoSignal
}

// BuildPanel builds the panel for results in the order given, with each
// target's verdict from Verdict under MangoMinSupport and generated_at stamped
// from now in UTC.
func BuildPanel(results []TargetResult, meta PanelMeta, now time.Time) Panel {
	p := Panel{
		SchemaVersion: panelSchema,
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Items:         meta.Items,
		SnapshotHours: meta.SnapshotHours,
		Targets:       make([]PanelTarget, 0, len(results)),
	}
	for _, t := range results {
		p.Targets = append(p.Targets, panelTarget(t))
	}
	return p
}

// panelTarget is one result as the panel writes it: the best box's holdout
// counts when the target was run, and the target's own holdout rate as the
// baseline the box was measured against.
func panelTarget(t TargetResult) PanelTarget {
	out := PanelTarget{
		Target:    t.Target,
		Positives: t.TrainPositives,
		Skipped:   t.TrainPositives < MangoMinSupport,
		Baseline:  fraction(t.HoldoutPositives, t.HoldoutN),
		Verdict:   Verdict(t, MangoMinSupport),
	}
	if t.Best != nil && !out.Skipped {
		out.BestBox = t.Best.Box
		out.HoldoutN = t.Best.N
		out.HoldoutRate = t.Best.Rate
	}
	return out
}

// fraction is k/n, and 0 when n is zero.
func fraction(k, n int) float64 {
	if n == 0 {
		return 0
	}
	return float64(k) / float64(n)
}

// PublishAtomic replaces dir/name with data by writing a temporary file in dir
// and renaming it over the target, so a reader sees the old panel or the new
// one and never a partial write. A failed publish removes its temporary file.
func PublishAtomic(dir, name string, data []byte) error {
	path := filepath.Join(dir, name)
	f, err := os.CreateTemp(dir, "."+name+".tmp*")
	if err != nil {
		return fmt.Errorf("panel: temp file in %s: %w", dir, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	fail := func(err error) error {
		_ = f.Close()
		return fmt.Errorf("panel: write %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if err := f.Chmod(0o644); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("panel: write %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("panel: replace %s: %w", path, err)
	}
	return nil
}
