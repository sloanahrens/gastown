package metrics

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestMetricsStoryPointsRefusesAnUnreadableTable: a landings table the reader
// cannot attribute stops the step with an error and prints no report, so a bad
// table never reads as an empty one.
func TestMetricsStoryPointsRefusesAnUnreadableTable(t *testing.T) {
	t.Parallel()
	town := newCommandTown(t)
	if _, err := town.run(t, CommandExtract); err != nil {
		t.Fatalf("extract: %v", err)
	}
	ragged := []byte("bead,rig,sp,sp_actual\r\ngt-1,gastown\r\n")
	if err := os.WriteFile(filepath.Join(town.outDir(), "landings.csv"), ragged, 0o644); err != nil {
		t.Fatalf("write ragged table: %v", err)
	}
	town.out.Reset()
	if _, err := town.run(t, CommandStoryPoints); err == nil {
		t.Fatalf("storypoints returned no error for a ragged table; output %q", town.out.String())
	}
	if got := town.out.String(); got != "" {
		t.Errorf("out = %q, want no report for an unreadable table", got)
	}
}

// TestBootstrapSpearmanRefusesWhatHasNoInterval: a length mismatch and fewer than
// two pairs have no interval, and the bootstrap says so rather than returning one.
func TestBootstrapSpearmanRefusesWhatHasNoInterval(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		x, y []float64
	}{
		{name: "length mismatch", x: []float64{1, 2, 3}, y: []float64{1, 2}},
		{name: "one pair", x: []float64{1}, y: []float64{1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := BootstrapSpearman(tc.x, tc.y, bootstrapResamples, bootstrapSeed); err == nil {
				t.Error("BootstrapSpearman returned no error")
			}
		})
	}
}

// TestShadowReportRefusesAMalformedMinutesCell: an estimated-minutes cell that is
// not a positive number stops the report, since the trigger table over it would
// count a bead wrongly.
func TestShadowReportRefusesAMalformedMinutesCell(t *testing.T) {
	t.Parallel()
	for _, cell := range []string{"0", "-3", "soon"} {
		rows := append(measuredRows(60), spLandingRow(97, "5", "5", "haiku", "all"))
		rows[len(rows)-1]["sp_minutes"] = cell
		var out bytes.Buffer
		if err := WriteStoryPointsReport(&out, rows); err == nil {
			t.Errorf("sp_minutes %q: WriteStoryPointsReport returned no error; output:\n%s", cell, out.String())
		}
	}
}

// TestShadowReportRefusesAMalformedMeasureCell: a measured-points cell that is
// not a point on the scale stops the report, the same as a bad estimate.
func TestShadowReportRefusesAMalformedMeasureCell(t *testing.T) {
	t.Parallel()
	rows := append(measuredRows(60), spLandingRow(96, "5", strconv.Itoa(4), "haiku", "all"))
	var out bytes.Buffer
	err := WriteStoryPointsReport(&out, rows)
	if err == nil || !strings.Contains(err.Error(), "sp_actual") {
		t.Errorf("err = %v, want an sp_actual refusal", err)
	}
}
