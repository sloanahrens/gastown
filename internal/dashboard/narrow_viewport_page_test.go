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

// At 470 px the Polecats table keeps its five columns in the same order, and
// its age track keeps the width its text needs instead of shrinking to nothing,
// so the Age cell is never cut off (gt-ec79s). The test reads the narrow
// template's declarations, as TestLandingsFitsANarrowViewport does.
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
	if age := tracks[4]; strings.HasPrefix(age, "minmax(0,") {
		t.Errorf("the age column at a narrow width is %q, which can shrink to nothing and cut off its time; it needs a floor at its content", age)
	}
}
