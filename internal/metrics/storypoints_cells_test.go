package metrics

import (
	"context"
	"testing"
)

// TestStoryPointCellsReadTheLabels: the sp and sp_rung cells come from the
// bead's sp: and sp-rung: labels, and a label of another kind is not one of
// them. A bead with no story-point label keeps both cells empty.
func TestStoryPointCellsReadTheLabels(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		labels []string
		sp     string
		rung   string
	}{
		{name: "estimate and rung", labels: []string{"bird-filed", "sp:8", "sp-rung:shape-jev", "sp-actual:5"}, sp: "8", rung: "shape-jev"},
		{name: "rung none", labels: []string{"sp:3", "sp-rung:none"}, sp: "3", rung: "none"},
		{name: "repeated identical label", labels: []string{"sp:21", "sp:21"}, sp: "21", rung: ""},
		{name: "no story-point label", labels: []string{"bird-filed"}, sp: "", rung: ""},
		{name: "no labels", labels: nil, sp: "", rung: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var dropped Dropped
			sp, rung := storyPointCells("gt-sp1", tc.labels, &dropped)
			if sp != tc.sp || rung != tc.rung {
				t.Errorf("cells = (%q, %q), want (%q, %q)", sp, rung, tc.sp, tc.rung)
			}
			if len(dropped.Items) != 0 {
				t.Errorf("dropped = %v, want none for a readable bead", dropped.Items)
			}
		})
	}
}

// TestStoryPointCellsNameUnreadableLabels: a sp: or sp-rung: label that does not
// parse, and two labels of one kind that disagree, leave the cell empty and are
// named in the footer, so the bead is unknown rather than read as unlabelled.
func TestStoryPointCellsNameUnreadableLabels(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		labels []string
		sp     string
		rung   string
		drops  int
	}{
		{name: "sp not on the scale", labels: []string{"sp:4"}, drops: 1},
		{name: "sp with leading zero", labels: []string{"sp:05"}, drops: 1},
		{name: "unknown rung", labels: []string{"sp-rung:gut"}, drops: 1},
		{name: "two estimates disagree", labels: []string{"sp:8", "sp:13"}, drops: 1},
		{name: "two rungs disagree", labels: []string{"sp-rung:all", "sp-rung:shape"}, drops: 1},
		{name: "readable sp beside an unreadable rung", labels: []string{"sp:8", "sp-rung:gut"}, sp: "8", drops: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var dropped Dropped
			sp, rung := storyPointCells("gt-sp2", tc.labels, &dropped)
			if sp != tc.sp || rung != tc.rung {
				t.Errorf("cells = (%q, %q), want (%q, %q)", sp, rung, tc.sp, tc.rung)
			}
			if len(dropped.Items) != tc.drops {
				t.Errorf("dropped = %v, want %d items", dropped.Items, tc.drops)
			}
		})
	}
}

// TestLandingRowCarriesTheStoryPointCells: the landings table the extract
// writes carries the sp and sp_rung cells read from the bead's labels, through
// BuildLandings, so a bead's labels reach the report.
func TestLandingRowCarriesTheStoryPointCells(t *testing.T) {
	t.Parallel()
	ledger := LoadLedger(map[string][]string{"gastown": {polecatLedgerLine(`"polecat_secs":600,"agent":"sbx-haiku"`)}}, &Dropped{})
	beads := map[string]BeadRow{"gt-p1": {Labels: []string{"sp:5", "sp-rung:all"}}}
	var dropped Dropped
	rows, err := BuildLandings(context.Background(), ledger, nil, fakeDiffSource{},
		beads, nil, nil, nil, nil, StageStamps{}, nil, &dropped)
	if err != nil {
		t.Fatalf("BuildLandings: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want the one landing", len(rows))
	}
	if got := rows[0]["sp"]; got != "5" {
		t.Errorf("sp = %q, want 5", got)
	}
	if got := rows[0]["sp_rung"]; got != "all" {
		t.Errorf("sp_rung = %q, want all", got)
	}
	if got := rows[0]["sp_actual"]; got != "8" {
		t.Errorf("sp_actual = %q, want 8 (600 seconds is 10 minutes)", got)
	}
}
