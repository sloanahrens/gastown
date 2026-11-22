package cmd

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/land"
)

// TestLandOmStatsClassesJSONAgreesWithTheTable: --classes --json carries the
// same rows as the text table for two ISO weeks, and every other field is the
// one a run without the flag prints (gt-9ofql6).
func TestLandOmStatsClassesJSONAgreesWithTheTable(t *testing.T) {
	t.Parallel()
	records := strings.Join([]string{
		`{"bead":"gt-a","attempt":1,"verdict":"request_changes","at":"2026-11-10T10:00:00Z","finding_text":[{"severity":"major","title":"the change reaches outside the scope"}]}`,
		`{"bead":"gt-b","attempt":1,"verdict":"request_changes","at":"2026-11-17T10:00:00Z","finding_text":[{"severity":"minor","title":"the comment names a line number"}]}`,
	}, "\n") + "\n"
	run := func(asJSON, classes bool) string {
		fake := &landOmStatsFakeFS{files: map[string]string{"/rec": records, "/log": ""}}
		var out strings.Builder
		if err := landOmStatsReportFrom(fake, omStatsNoTown(t), &out, "", "/rec", "/log", asJSON, classes); err != nil {
			t.Fatalf("landOmStatsReportFrom(json %v, classes %v): %v", asJSON, classes, err)
		}
		return out.String()
	}
	text := run(false, true)
	var plain land.OmStats
	if err := json.Unmarshal([]byte(run(true, false)), &plain); err != nil {
		t.Fatalf("decoding the report without --classes: %v", err)
	}
	var doc landOmStatsClassesReport
	if err := json.Unmarshal([]byte(run(true, true)), &doc); err != nil {
		t.Fatalf("decoding the report with --classes: %v", err)
	}
	if !reflect.DeepEqual(doc.OmStats, plain) {
		t.Errorf("--classes changed the report's other fields: %+v, want %+v", doc.OmStats, plain)
	}
	if len(doc.Classes) != 2 || landOmStatsClassCountFor(t, doc.Classes[0], "scope-widened").Majors != 1 {
		t.Fatalf("classes = %+v, want one row per ISO week, the first with its major", doc.Classes)
	}
	rows := strings.Split(text, "\n")
	headerAt := -1
	for i, row := range rows {
		if strings.HasPrefix(row, "Week ") {
			headerAt = i
		}
	}
	if headerAt < 0 {
		t.Fatalf("the text report carries no class table:\n%s", text)
	}
	header := strings.Fields(rows[headerAt])
	if len(header) != 3+len(doc.Classes[0].Classes) {
		t.Fatalf("table header = %q, want three counts and a cell per class", rows[headerAt])
	}
	for i, week := range doc.Classes {
		cells := strings.Fields(rows[headerAt+1+i])
		prefix := fmt.Sprintf("%d-W%02d %d %d", week.ISOYear, week.ISOWeek, week.Reviews, week.NoFindingText)
		if len(cells) != len(header) || strings.Join(cells[:3], " ") != prefix {
			t.Fatalf("row %d = %q, want %q and a cell per class", i+1, rows[headerAt+1+i], prefix)
		}
		for j, class := range week.Classes {
			want := fmt.Sprintf("%.1f/%.1f", week.ratePer100(class.Majors), week.ratePer100(class.Minors))
			if cells[3+j] != want || header[3+j] != class.ID {
				t.Errorf("%d-W%02d class %s: JSON row has cell %q under %q, want %q under %q", week.ISOYear, week.ISOWeek, class.ID, cells[3+j], header[3+j], want, class.ID)
			}
		}
	}
}

// TestLandOmStatsClassesJSONReturnsTheWriteError: a failing writer's error is
// returned, not read as a written report.
func TestLandOmStatsClassesJSONReturnsTheWriteError(t *testing.T) {
	t.Parallel()
	if err := writeLandOmStatsClassesJSON(failingWriter{}, landOmStatsClassesReport{}); err == nil {
		t.Fatal("a failed write returned no error")
	}
}
