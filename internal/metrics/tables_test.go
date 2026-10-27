package metrics

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
)

// The inputs and expected values are scripts/test_town_metrics_tables.py's:
// synthetic, hand-checked, and the same on both sides of the port.
var (
	tablesLedger = []LedgerRow{
		{Bead: "gt-1", Rig: "gastown", LandedCommit: "c1", LandedAt: "2026-10-01T00:00:00Z", Route: "daemon", OMVerdict: "approve", GateResult: "build"},
		{Bead: "gt-2", Rig: "gastown", LandedCommit: "c2", LandedAt: "2026-10-14T00:00:00Z", Route: "overseer-manual", OMVerdict: "skipped", GateResult: "build"},
		{Bead: "gt-3", Rig: "gastown", LandedCommit: "bad", LandedAt: "2026-10-15T00:00:00Z", Route: "daemon", OMVerdict: "approve", GateResult: "build"},
	}
	tablesRework = map[string]Rework{
		"gt-1": {Attempts: 2, RejectedBeforeLanding: true},
		"gt-2": {Attempts: 1},
		"gt-3": {Attempts: 1},
	}
	tablesUsage = map[string]Usage{"gt-1": {Model: "deepseek-flash", Seat: "s", End: "x"}}
	tablesEras  = map[string][]OMEra{"gastown": {
		{Start: "2026-09-01T00:00:00Z", Threshold: 0.7},
		{Start: "2026-10-10T00:00:00Z", Threshold: 0.75},
	}}
	tablesOverrun = map[string]bool{"gt-2": true}
	tablesShaped  = map[string]bool{"gt-1": true}
)

// tablesBeads is the Python's BEADS: every landing's bead a task at priority 2
// with two labels.
func tablesBeads() map[string]BeadRow {
	priority := 2
	out := make(map[string]BeadRow, len(tablesLedger))
	for _, l := range tablesLedger {
		out[l.Bead] = BeadRow{ID: l.Bead, Type: "task", Priority: &priority, Labels: []string{"a", "b"}}
	}
	return out
}

// fakeDiffSource is the Python's FakeDiff: every commit but "bad" has the same
// stats, and "bad" is the commit whose diff cannot be read.
type fakeDiffSource struct{}

func (fakeDiffSource) Stats(_ context.Context, _, commit string) (DiffStat, error) {
	if commit == "bad" {
		return DiffStat{}, fmt.Errorf("%w: %s", ErrDiffUnavailable, commit)
	}
	return DiffStat{LinesAdded: 6, LinesDeleted: 4, FilesTouched: 2, FileEntropy: 1.0, PackageEntropy: 0.0}, nil
}

// buildTablesLandings is the Python's build helper: the fixture sources joined
// with a fresh drop log unless the test names one.
func buildTablesLandings(t *testing.T, dropped *Dropped) []Row {
	t.Helper()
	rows, err := BuildLandings(context.Background(), tablesLedger, tablesRework, fakeDiffSource{},
		tablesBeads(), tablesUsage, tablesOverrun, tablesEras, tablesShaped, dropped)
	if err != nil {
		t.Fatalf("BuildLandings: %v", err)
	}
	return rows
}

func rowsByBead(rows []Row) map[string]Row {
	out := make(map[string]Row, len(rows))
	for _, r := range rows {
		out[r["bead"]] = r
	}
	return out
}

func TestBuildLandingsOneRowPerLedgerRow(t *testing.T) {
	t.Parallel()
	var got []string
	for _, r := range buildTablesLandings(t, &Dropped{}) {
		got = append(got, r["bead"])
	}
	if want := []string{"gt-1", "gt-2", "gt-3"}; !slices.Equal(got, want) {
		t.Errorf("beads = %v, want one row per ledger row %v", got, want)
	}
}

func TestBuildLandingsFlagsAndRegime(t *testing.T) {
	t.Parallel()
	rows := rowsByBead(buildTablesLandings(t, &Dropped{}))
	tests := []struct {
		bead   string
		column string
		want   string
	}{
		{"gt-1", "rejected_before_landing", "1"},
		{"gt-2", "manual_rescue", "1"},
		{"gt-1", "manual_rescue", "0"},
		{"gt-2", "reached_overrun_stop", "1"},
		{"gt-1", "om_threshold", "0.7"},
		{"gt-2", "om_threshold", "0.75"},
		{"gt-1", "dominant_model", "deepseek-flash"},
		{"gt-1", "week", "2026-09-28"},
		{"gt-1", "shaped", "1"},
		{"gt-1", "lines_total", "10"},
	}
	for _, tc := range tests {
		t.Run(tc.bead+" "+tc.column, func(t *testing.T) {
			t.Parallel()
			if got := rows[tc.bead][tc.column]; got != tc.want {
				t.Errorf("%s[%s] = %q, want %q", tc.bead, tc.column, got, tc.want)
			}
		})
	}
}

func TestBuildLandingsUnreadableCommitIsFlaggedNotZeroed(t *testing.T) {
	t.Parallel()
	dropped := &Dropped{}
	r := rowsByBead(buildTablesLandings(t, dropped))["gt-3"]
	if got := r["unreadable"]; got != "1" {
		t.Errorf("gt-3[unreadable] = %q, want 1", got)
	}
	if got := r["lines_total"]; got != "" {
		t.Errorf("gt-3[lines_total] = %q, want an empty cell: an unreadable diff is not zero lines", got)
	}
	if dropped.Count() != 1 {
		t.Fatalf("dropped.Count() = %d, want 1: the row is kept and named in the footer", dropped.Count())
	}
	want := DroppedItem{Source: "git:gastown", Reason: "unreadable diff for gt-3"}
	if dropped.Items[0] != want {
		t.Errorf("dropped.Items[0] = %+v, want %+v", dropped.Items[0], want)
	}
}

func TestExplainOnlyContainsOnlyPostHocColumns(t *testing.T) {
	t.Parallel()
	for _, column := range explainOnlyColumns {
		if !slices.Contains(landingColumns, column) {
			t.Errorf("EXPLAIN_ONLY names %q, which LANDING_COLUMNS does not carry", column)
		}
	}
	predictors := []string{"om_threshold", "dominant_model", "shaped", "type", "priority", "label_count", "model"}
	for _, column := range predictors {
		if slices.Contains(explainOnlyColumns, column) {
			t.Errorf("EXPLAIN_ONLY names %q, which is known before the landing", column)
		}
	}
}

// TestExplainOnlyColumnsMatchThePythonExtract checks the sidecar's keys are the
// Python extract's EXPLAIN_ONLY set: the mango stage drops exactly those
// columns from its predictors.
func TestExplainOnlyColumnsMatchThePythonExtract(t *testing.T) {
	t.Parallel()
	want := pythonStringList(t, "tables.py", "EXPLAIN_ONLY")
	if len(want) == 0 {
		t.Fatal("read no columns out of tables.py")
	}
	slices.Sort(want)
	got := slices.Clone(explainOnlyColumns)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("explainOnlyColumns = %v, want the extract's %v", explainOnlyColumns, want)
	}
}

func TestWriteLandingSidecarListsExplainOnly(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	if err := WriteLandingSidecar(&buf); err != nil {
		t.Fatalf("WriteLandingSidecar: %v", err)
	}
	var sidecar struct {
		ExplainOnly []string `json:"explain_only"`
	}
	if err := json.Unmarshal(buf.Bytes(), &sidecar); err != nil {
		t.Fatalf("decode the sidecar: %v", err)
	}
	want := slices.Clone(explainOnlyColumns)
	slices.Sort(want)
	if !slices.Equal(sidecar.ExplainOnly, want) {
		t.Errorf("sidecar explain_only = %v, want %v", sidecar.ExplainOnly, want)
	}
}
