package metrics

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// These cases are the Python tests' cases (scripts/test_town_metrics_cli.py):
// TestTypedWeeklyRoundTripsThroughCSV is
// test_typed_weekly_round_trips_through_csv, and
// TestLoadResultRebuildsFromTheOutputDirectory is
// test_standalone_report_rebuilds_from_the_output_directory.

// loadResultRow is the row the extract's writer makes of m, for a test that
// reads it back.
func loadResultRow(t *testing.T, m WeeklyMetrics) Row {
	t.Helper()
	row := make(Row, len(WeeklyColumns))
	for _, column := range WeeklyColumns {
		cell, ok := m.Cell(column)
		if !ok {
			t.Fatalf("Cell(%q): no such column", column)
		}
		row[column] = cell
	}
	return row
}

// loadResultWeekly is the fixture's ALL row: two landings in the week of
// 2026-10-12, with a median size and a rho to read back.
func loadResultWeekly() WeeklyMetrics {
	return WeeklyMetrics{
		Week:       "2026-10-12",
		Rig:        "ALL",
		Landings:   2,
		SizeMedian: floatPtr(10),
		Rho:        floatPtr(2),
	}
}

// TestTypedWeeklyRoundTripsThroughCSV is the Python test's
// test_typed_weekly_round_trips_through_csv: the ALL row writes to weekly.csv
// and reads back with the same figures the report compares.
func TestTypedWeeklyRoundTripsThroughCSV(t *testing.T) {
	t.Parallel()
	written := loadResultWeekly()
	var buf bytes.Buffer
	if err := WriteCSV(&buf, []Row{loadResultRow(t, written)}, WeeklyColumns); err != nil {
		t.Fatalf("WriteCSV: %v", err)
	}
	rows, err := ReadCSV(&buf)
	if err != nil {
		t.Fatalf("ReadCSV: %v", err)
	}
	back, err := TypedWeekly(rows)
	if err != nil {
		t.Fatalf("TypedWeekly: %v", err)
	}
	if len(back) != 1 {
		t.Fatalf("TypedWeekly read %d rows, want 1", len(back))
	}
	b := back[0]
	if b.Week != written.Week || b.Thin != written.Thin || b.Landings != written.Landings {
		t.Errorf("week/thin/landings = %q/%v/%d, want %q/%v/%d",
			b.Week, b.Thin, b.Landings, written.Week, written.Thin, written.Landings)
	}
	if b.SizeMedian == nil || *b.SizeMedian != *written.SizeMedian {
		t.Errorf("size_median = %v, want %v", b.SizeMedian, *written.SizeMedian)
	}
	if b.Rho == nil || *b.Rho != *written.Rho {
		t.Errorf("rho = %v, want %v", b.Rho, *written.Rho)
	}
}

// TestLoadResultRebuildsFromTheOutputDirectory is the Python test's
// test_standalone_report_rebuilds_from_the_output_directory: a footer and a
// weekly.csv on disk come back as the typed result the report reads.
func TestLoadResultRebuildsFromTheOutputDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	want := Footer{
		RowsRead:          map[string]int{"ledger": 2, "reviews": 0, "beads": 2, "usage": 1},
		RowsDropped:       [][]string{{"ledger:gastown", "unparseable json"}},
		Gaps:              [][]string{{"diff:gastown", "no stats"}},
		SourcesUnreadable: []string{},
	}
	footerJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal footer: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, footerName), footerJSON, 0o644); err != nil {
		t.Fatalf("write %s: %v", footerName, err)
	}
	var buf bytes.Buffer
	if err := WriteCSV(&buf, []Row{loadResultRow(t, loadResultWeekly())}, WeeklyColumns); err != nil {
		t.Fatalf("WriteCSV: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, weeklyName), buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write %s: %v", weeklyName, err)
	}

	got, err := LoadResult(Env{ReadFile: os.ReadFile}, dir)
	if err != nil {
		t.Fatalf("LoadResult: %v", err)
	}
	if !reflect.DeepEqual(got.Footer, want) {
		t.Errorf("footer = %+v, want %+v", got.Footer, want)
	}
	if len(got.Weekly) != 1 {
		t.Errorf("LoadResult read %d weekly rows, want 1", len(got.Weekly))
	}
}

// TestTypedWeeklyTurnsCellsIntoValues holds the typing rules typed_weekly
// applies to one row.
func TestTypedWeeklyTurnsCellsIntoValues(t *testing.T) {
	t.Parallel()
	model := "m"
	tests := []struct {
		name string
		row  Row
		want WeeklyMetrics
	}{
		{
			"counts, a float, a True thin and a dominant model",
			Row{"week": "2026-10-12", "rig": "ALL", "thin": "True", "dominant_model": "m",
				"landings": "2", "unreadable": "0", "size_median": "10", "rho": "2"},
			WeeklyMetrics{Week: "2026-10-12", Rig: "ALL", Thin: true, Landings: 2,
				SizeMedian: floatPtr(10), Rho: floatPtr(2), DominantModel: &model},
		},
		{
			"an empty cell and an empty dominant model are values the week lacks",
			Row{"week": "2026-10-12", "rig": "gastown", "thin": "False", "dominant_model": "",
				"size_median": ""},
			WeeklyMetrics{Week: "2026-10-12", Rig: "gastown"},
		},
		{
			"a column the row does not carry is a value the week lacks",
			Row{"week": "2026-10-12", "rig": "gastown", "thin": "False", "dominant_model": ""},
			WeeklyMetrics{Week: "2026-10-12", Rig: "gastown"},
		},
		{
			"a fractional count truncates as int() does",
			Row{"week": "w", "rig": "r", "thin": "False", "dominant_model": "",
				"landings": "3.9", "rho_numerator": "-1.9"},
			WeeklyMetrics{Week: "w", Rig: "r", Landings: 3, RhoNumerator: -1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := TypedWeekly([]Row{tc.row})
			if err != nil {
				t.Fatalf("TypedWeekly: %v", err)
			}
			if !reflect.DeepEqual(got, []WeeklyMetrics{tc.want}) {
				t.Errorf("TypedWeekly = %+v, want %+v", got, []WeeklyMetrics{tc.want})
			}
		})
	}
}

// TestTypedWeeklyRejectsARowItCannotType names the cells int() and float()
// refuse in the Python.
func TestTypedWeeklyRejectsARowItCannotType(t *testing.T) {
	t.Parallel()
	row := func(cell, value string) Row {
		r := Row{"week": "w", "rig": "r", "thin": "False", "dominant_model": ""}
		r[cell] = value
		return r
	}
	tests := []struct {
		name string
		row  Row
	}{
		{"no week column", Row{"rig": "r", "thin": "False", "dominant_model": ""}},
		{"no dominant_model column", Row{"week": "w", "rig": "r", "thin": "False"}},
		{"a figure that is not a number", row("size_median", "big")},
		{"a count that is not finite", row("landings", "nan")},
		{"a count beyond int's range", row("landings", "1e300")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got, err := TypedWeekly([]Row{tc.row}); err == nil {
				t.Errorf("TypedWeekly = %+v, want an error", got)
			}
		})
	}
}

// TestLoadResultNamesTheFileItCouldNotRead keeps a missing output directory
// from looking like a week with no rows.
func TestLoadResultNamesTheFileItCouldNotRead(t *testing.T) {
	t.Parallel()
	_, err := LoadResult(Env{ReadFile: os.ReadFile}, t.TempDir())
	if err == nil {
		t.Fatal("LoadResult of an empty directory succeeded, want an error")
	}
	if want := footerName; !bytes.Contains([]byte(err.Error()), []byte(want)) {
		t.Errorf("LoadResult error = %q, want it to name %s", err, want)
	}
}
