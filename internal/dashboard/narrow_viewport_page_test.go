package dashboard

import (
	"strings"
	"testing"
)

// At 470 px the header's health line wraps instead of ending in an ellipsis, so
// its tick age and landed count stay readable (gt-ec79s). The harness has no
// layout engine, so the test reads the declarations the stylesheet sets at a
// narrow width; the rendered 470 px check is the overseer's, live.
func TestHealthLineWrapsAtANarrowWidth(t *testing.T) {
	t.Parallel()

	found := false
	for _, rule := range landingsConditionalRules(t) {
		if !strings.Contains(rule.At, "max-width:640px") || rule.Sel != "#healthline" {
			continue
		}
		found = true
		if got := rule.Decls["white-space"]; got != "normal" {
			t.Errorf("#healthline sets white-space:%q at a narrow width, want normal so the line wraps", got)
		}
		if got := rule.Decls["text-overflow"]; got != "clip" {
			t.Errorf("#healthline sets text-overflow:%q at a narrow width, want clip, not the ellipsis that hides the tick age", got)
		}
	}
	if !found {
		t.Error("no max-width:640px rule styles #healthline, so the header's health line ends in an ellipsis at 470 px")
	}
}

// At 470 px the Polecats table keeps its five columns in the same order, every
// column but the bead holds the width its text needs, and only the bead shrinks
// (gt-ec79s). The test reads the narrow template's declarations, as
// TestLandingsFitsANarrowViewport does.
func TestPolecatsTableFitsANarrowViewport(t *testing.T) {
	t.Parallel()

	cols := ""
	found := false
	for _, rule := range landingsConditionalRules(t) {
		if !strings.Contains(rule.At, "max-width:640px") || rule.Sel != ".wtable" {
			continue
		}
		found = true
		cols = rule.Decls["grid-template-columns"]
	}
	if !found {
		t.Fatal("no max-width:640px rule styles .wtable, so the seat table keeps its template at 470 px and the Age column is cut off")
	}

	var tracks, names []string
	for _, tok := range strings.Fields(cols) {
		if n, ok := strings.CutPrefix(tok, "["); ok {
			names = append(names, strings.TrimSuffix(n, "]"))
			continue
		}
		tracks = append(tracks, tok)
	}
	if got, want := strings.Join(names, " "), "polecat agent bead state age"; got != want {
		t.Errorf("the narrow seat table names its columns %q, want %q in the order the header draws them", got, want)
	}
	if len(tracks) != 5 {
		t.Fatalf("the narrow seat table lays out %d columns (%q), want five", len(tracks), cols)
	}
	for i, name := range names {
		track := tracks[i]
		if name == "bead" {
			if track != "minmax(0,1fr)" {
				t.Errorf("the narrow seat table gives the bead %q, want minmax(0,1fr): the bead is the one column left to shrink", track)
			}
			continue
		}
		if !strings.HasPrefix(track, "minmax(max-content,") {
			t.Errorf("the %s column at a narrow width is %q, which can shrink below its content and cut off its text; it needs a floor at its max-content", name, track)
		}
	}
}
