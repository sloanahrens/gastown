package metrics

import (
	"testing"
	"time"
)

// bandNow is the fixed clock the band rows are measured against, the day the
// bead's live figures were taken.
var bandNow = time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)

// bandRow is a landing row holding the cells estimateBandOf reads.
func bandRow(minutes, landedAt, rejected, overrun string) Row {
	return Row{
		"sp_minutes":              minutes,
		"landed_at":               landedAt,
		"rejected_before_landing": rejected,
		"reached_overrun_stop":    overrun,
	}
}

// bandAt is a landed_at offset from bandNow, so a window case is stated in
// whole hours.
func bandAt(offset time.Duration) string {
	return bandNow.Add(offset).UTC().Format(time.RFC3339)
}

func TestEstimateBandOf(t *testing.T) {
	t.Parallel()
	t.Run("the five bands are named narrowest first", func(t *testing.T) {
		want := []string{"6 min or under", "6 to 8", "8 to 10", "10 to 12", "over 12"}
		if len(estimateBandNames) != len(want) {
			t.Fatalf("estimateBandNames has %d names, want %d", len(estimateBandNames), len(want))
		}
		for i, name := range want {
			if estimateBandNames[i] != name {
				t.Errorf("estimateBandNames[%d] = %q, want %q", i, estimateBandNames[i], name)
			}
		}
	})

	tests := []struct {
		name         string
		row          Row
		wantBand     int
		wantRejected bool
		wantOverrun  bool
		wantOK       bool
	}{
		{
			name:     "6 is the top of the first band",
			row:      bandRow("6", bandAt(-48*time.Hour), "0", "0"),
			wantBand: 0, wantOK: true,
		},
		{
			name:     "a value just over 6 is the second band",
			row:      bandRow("6.1", bandAt(-48*time.Hour), "0", "0"),
			wantBand: 1, wantOK: true,
		},
		{
			name:     "8 is the top of the second band",
			row:      bandRow("8", bandAt(-48*time.Hour), "0", "0"),
			wantBand: 1, wantOK: true,
		},
		{
			name:     "10 is the top of the third band",
			row:      bandRow("10", bandAt(-48*time.Hour), "0", "0"),
			wantBand: 2, wantOK: true,
		},
		{
			name:     "12 is the top of the fourth band",
			row:      bandRow("12", bandAt(-48*time.Hour), "0", "0"),
			wantBand: 3, wantOK: true,
		},
		{
			name:     "above 12 is the last band",
			row:      bandRow("12.1", bandAt(-48*time.Hour), "0", "0"),
			wantBand: 4, wantOK: true,
		},
		{
			name:     "a value between zero and 6 is the first band",
			row:      bandRow("0.5", bandAt(-48*time.Hour), "0", "0"),
			wantBand: 0, wantOK: true,
		},
		{name: "a blank sp_minutes is skipped", row: bandRow("", bandAt(-48*time.Hour), "0", "0")},
		{name: "a zero sp_minutes is skipped", row: bandRow("0", bandAt(-48*time.Hour), "0", "0")},
		{name: "a negative sp_minutes is skipped", row: bandRow("-3", bandAt(-48*time.Hour), "0", "0")},
		{name: "a non-numeric sp_minutes is skipped", row: bandRow("fast", bandAt(-48*time.Hour), "0", "0")},
		{name: "a blank landed_at is skipped", row: bandRow("7", "", "0", "0")},
		{name: "an unparsable landed_at is skipped", row: bandRow("7", "yesterday", "0", "0")},
		{
			name:     "27 days old is inside the window",
			row:      bandRow("7", bandAt(-27*24*time.Hour), "0", "0"),
			wantBand: 1, wantOK: true,
		},
		{
			name:     "28 days old is inside the window",
			row:      bandRow("7", bandAt(-28*24*time.Hour), "0", "0"),
			wantBand: 1, wantOK: true,
		},
		{name: "29 days old is outside the window", row: bandRow("7", bandAt(-29*24*time.Hour), "0", "0")},
		{
			name:     "a landing at now is inside the window",
			row:      bandRow("7", bandAt(0), "0", "0"),
			wantBand: 1, wantOK: true,
		},
		{name: "a future landing is skipped", row: bandRow("7", bandAt(time.Hour), "0", "0")},
		{
			name:     "a rejected cell of 1 flags rejected",
			row:      bandRow("7", bandAt(-48*time.Hour), "1", "0"),
			wantBand: 1, wantRejected: true, wantOK: true,
		},
		{
			name:     "an overrun cell of 1 flags overrun",
			row:      bandRow("7", bandAt(-48*time.Hour), "0", "1"),
			wantBand: 1, wantOverrun: true, wantOK: true,
		},
		{
			name:     "both cells of 1 flag both",
			row:      bandRow("7", bandAt(-48*time.Hour), "1", "1"),
			wantBand: 1, wantRejected: true, wantOverrun: true, wantOK: true,
		},
		{
			name:     "an outcome cell other than 1 is false",
			row:      bandRow("7", bandAt(-48*time.Hour), "true", "yes"),
			wantBand: 1, wantOK: true,
		},
		{
			name:     "blank outcome cells are false",
			row:      bandRow("7", bandAt(-48*time.Hour), "", ""),
			wantBand: 1, wantOK: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			band, rejected, overrun, ok := estimateBandOf(tt.row, bandNow)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if band != tt.wantBand {
				t.Errorf("band = %d, want %d (%q)", band, tt.wantBand, estimateBandNames[tt.wantBand])
			}
			if rejected != tt.wantRejected {
				t.Errorf("rejected = %v, want %v", rejected, tt.wantRejected)
			}
			if overrun != tt.wantOverrun {
				t.Errorf("overrun = %v, want %v", overrun, tt.wantOverrun)
			}
		})
	}
}
