package dashboard

import (
	"strings"
	"testing"
)

// The queue wants to sit beside Polecats and Dispatcher, the landing history
// beside om review, and the tier sweeps beside the rigs they sweep, so the
// panes are placed by column in index.html rather than by data. Reading the
// embedded page is what keeps a section that drifts back to the wrong column
// from passing (gt-9bf2m, gt-fn9e6.48).
func TestPaneColumns(t *testing.T) {
	t.Parallel()

	cols := strings.Split(string(indexHTML), `<div class="col">`)
	if len(cols) != 3 {
		t.Fatalf("index.html has %d columns, want 2", len(cols)-1)
	}
	left, right := cols[1], cols[2]

	for _, tc := range []struct {
		pane, col, before, after string
	}{
		{"escalationnote", "left", "", "alertsnote"},
		{"alertsnote", "left", "escalationnote", "spendnote"},
		{"spendnote", "left", "alertsnote", "rigsnote"},
		{"rigsnote", "left", "spendnote", "tiersweepnote"},
		{"tiersweepnote", "left", "rigsnote", "seatsnote"},
		{"seatsnote", "left", "tiersweepnote", "dispnote"},
		{"dispnote", "left", "seatsnote", "queuenote"},
		{"queuenote", "left", "dispnote", ""},
		{"machnote", "right", "", "forgejonote"},
		{"forgejonote", "right", "machnote", "omnote"},
		{"omnote", "right", "forgejonote", "trendsec"},
		{"trendsec", "right", "omnote", ""},
	} {
		got := left
		other := right
		if tc.col == "right" {
			got, other = right, left
		}
		at := strings.Index(got, `id="`+tc.pane+`"`)
		if at < 0 {
			t.Errorf("%s is not in the %s column", tc.pane, tc.col)
			continue
		}
		if strings.Contains(other, `id="`+tc.pane+`"`) {
			t.Errorf("%s is also in the other column", tc.pane)
		}
		if tc.before != "" && strings.Index(got, `id="`+tc.before+`"`) > at {
			t.Errorf("%s does not follow %s in the %s column", tc.pane, tc.before, tc.col)
		}
		if tc.after != "" && at > strings.Index(got, `id="`+tc.after+`"`) {
			t.Errorf("%s does not precede %s in the %s column", tc.pane, tc.after, tc.col)
		}
	}

	if !strings.Contains(left+right, `<section id="trendsec" hidden>`) {
		t.Error("the Landings section lost its hidden attribute")
	}
}
