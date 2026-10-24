package dashboard

import (
	"strings"
	"testing"
)

// The Work panel wants to sit under the tier sweeps, the landing history
// beside om review, the feed under the Work panel at the foot of the left
// column, and the tier sweeps beside the rigs they sweep, so the panes are
// placed by column in index.html rather than by data. Reading the embedded
// page is what keeps a section that drifts back to the wrong column from
// passing (gt-9bf2m, gt-fn9e6.48).
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
		{"attentionnote", "left", "", "overseernote"},
		{"overseernote", "left", "attentionnote", "costnote"},
		{"costnote", "left", "overseernote", "rigsnote"},
		{"rigsnote", "left", "costnote", "tiersweepnote"},
		{"tiersweepnote", "left", "rigsnote", "mangonote"},
		{"mangonote", "left", "tiersweepnote", "worknote"},
		{"worknote", "left", "tiersweepnote", "chips"},
		{"chips", "left", "worknote", ""},
		{"machnote", "right", "", "cloudnote"},
		{"cloudnote", "right", "machnote", "forgejonote"},
		{"forgejonote", "right", "cloudnote", "omnote"},
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
