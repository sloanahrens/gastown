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

// MangoThinHoldout is the holdout count under which a box's rate is too narrow
// to read, so the dashboard marks the row thin.
const MangoThinHoldout = 50

// The reason a target is not-yet: too few positives in the training split to
// search at all, or none in the holdout to test a box on.
const (
	ReasonFewPositives       = "few_positives"
	ReasonNoHoldoutPositives = "no_holdout_positives"
)

// How much of a series the panel carries: a trend long enough to see and short
// enough that the page draws it at a glance.
const (
	panelWeeklyMax = 26
	panelEventsMax = 16
)

// panelDateLen is how many bytes a calendar day takes, which is the whole of a
// regime event's date and the day part of a target's last positive.
const panelDateLen = 10

// PanelWeek is one week of the panel's trend: the landings that week across
// every rig and the share rejected before they landed.
type PanelWeek struct {
	Week          string  `json:"week"`
	Landings      int     `json:"landings"`
	RejectedShare float64 `json:"rejected_share"`
}

// PanelEvent is one regime event the trend is marked with: the day the regime
// changed and what changed.
type PanelEvent struct {
	Date string `json:"date"`
	What string `json:"what"`
}

// PanelMeta is the panel's context outside its targets: how many landings the
// analysis read, how many hours of snapshots it joined, the weekly series and
// the regime events. Weekly and Events read oldest first; BuildPanel keeps the
// newest of each, so a caller may hand it all of history.
type PanelMeta struct {
	Items         int
	SnapshotHours int
	Weekly        []PanelWeek
	Events        []PanelEvent
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
	Weekly        []PanelWeek   `json:"weekly"`
	Events        []PanelEvent  `json:"events"`
	Targets       []PanelTarget `json:"targets"`
}

// PanelTarget is one target's line in the panel. A skipped target carries its
// name and positives with no box, because it was never run. The box's keys are
// pointers so a target without a box, or an interval with no rows behind it,
// stays absent rather than reading as a zero the dashboard would draw. Its
// conditions are absent the same way, so a target with no box carries none.
type PanelTarget struct {
	Target           string           `json:"target"`
	Label            string           `json:"label"`
	Positives        int              `json:"positives"`
	Skipped          bool             `json:"skipped"`
	BestBox          string           `json:"best_box"`
	Conditions       []MangoCondition `json:"conditions,omitempty"`
	HoldoutPositives *int             `json:"holdout_positives,omitempty"`
	HoldoutN         int              `json:"holdout_n"`
	HoldoutRate      float64          `json:"holdout_rate"`
	CILow            *float64         `json:"ci_low,omitempty"`
	CIHigh           *float64         `json:"ci_high,omitempty"`
	Thin             *bool            `json:"thin,omitempty"`
	Baseline         float64          `json:"baseline"`
	Verdict          string           `json:"verdict"`
	Reason           string           `json:"reason,omitempty"`
	// LastPositive is the day the target's latest positive landed, so a target
	// too thin to check says how long it has been too thin; absent when the
	// split held no positive.
	LastPositive string `json:"last_positive,omitempty"`
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
// target's verdict from Verdict under MangoMinSupport, generated_at stamped
// from now in UTC, and the newest of the meta's weekly series and events.
func BuildPanel(results []TargetResult, meta PanelMeta, now time.Time) Panel {
	p := Panel{
		SchemaVersion: panelSchema,
		GeneratedAt:   now.UTC().Format(time.RFC3339),
		Items:         meta.Items,
		SnapshotHours: meta.SnapshotHours,
		Weekly:        newest(meta.Weekly, panelWeeklyMax),
		Events:        newest(meta.Events, panelEventsMax),
		Targets:       make([]PanelTarget, 0, len(results)),
	}
	for _, t := range results {
		p.Targets = append(p.Targets, panelTarget(t))
	}
	return p
}

// newest is the last max of items, a series read oldest first, and an empty
// list for a nil one: the page draws these keys as lists, never as null.
func newest[T any](items []T, max int) []T {
	if items == nil {
		return []T{}
	}
	if len(items) > max {
		return items[len(items)-max:]
	}
	return items
}

// panelTarget is one result as the panel writes it: the best box's conditions
// and holdout counts when the target was run, and the target's own holdout rate
// as the baseline the box was measured against. A not-yet target names its
// reason, and a box carries its interval and whether its count is thin.
func panelTarget(t TargetResult) PanelTarget {
	out := PanelTarget{
		Target:    t.Target,
		Label:     panelLabel(t.Target),
		Positives: t.TrainPositives,
		Skipped:   t.TrainPositives < MangoMinSupport,
		Baseline:  fraction(t.HoldoutPositives, t.HoldoutN),
		Verdict:   Verdict(t, MangoMinSupport),
	}
	if out.Verdict == VerdictNotYet {
		out.Reason = notYetReason(t)
	}
	if t.LastPositiveAt != "" {
		out.LastPositive = panelLastPositive(t.LastPositiveAt)
	}
	if t.Best != nil && !out.Skipped {
		out.BestBox = t.Best.Box
		out.Conditions = t.Conditions
		out.HoldoutN = t.Best.N
		out.HoldoutRate = t.Best.Rate
		k, thin := t.Best.K, t.Best.N < MangoThinHoldout
		out.HoldoutPositives = &k
		out.Thin = &thin
		if low, high, ok := wilsonInterval(t.Best.K, t.Best.N); ok {
			out.CILow, out.CIHigh = &low, &high
		}
	}
	return out
}

// panelLastPositive is the day a target's latest positive landed: the date
// part of the ISO timestamp Prepared records, and "" when it records none.
func panelLastPositive(at string) string {
	if len(at) <= panelDateLen {
		return at
	}
	return at[:panelDateLen]
}

// notYetReason names why a target is not-yet, as the writer derived it: the
// verdict is only not-yet when one of these two holds.
func notYetReason(t TargetResult) string {
	if t.TrainPositives < MangoMinSupport {
		return ReasonFewPositives
	}
	return ReasonNoHoldoutPositives
}

// panelLabel is the target's plain-words title. A target with no title shows
// its id, so a target added to mangoTargets still draws a row.
func panelLabel(target string) string {
	switch target {
	case "rejected_before_landing":
		return "Landings rejected before they land"
	case "reached_overrun_stop":
		return "Beads stopped by the overrun guard"
	}
	return target
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
