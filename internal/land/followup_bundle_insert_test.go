package land

import (
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

// TestInsertFollowupLines: the line lands in Goal, the box in Acceptance, both
// commits stay findable, and a body without either anchor is refused unchanged.
func TestInsertFollowupLines(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	w1, res1 := bundleLanding("gt-abc", bundleFirst, 0.8)
	g1 := bundleGroupAt("internal/land", minorAt("internal/land/note.go", 230, "swap the nil check"))
	body := bundleSpec(w1, res1, day, g1).Body
	w2, res2 := bundleLanding("gt-def", bundleSecond, 0.9)
	add := bundleAddition(w2, res2, bundleGroupAt("internal/land", minorAt("internal/land/hook.go", 0, "document the hook")))
	got, ok := InsertFollowupLines(body, add)
	if !ok {
		t.Fatalf("InsertFollowupLines refused a bundle body:\n%s", body)
	}
	sections := specdispatch.Sections(got)
	if !strings.Contains(sections["goal"], add.Lines[0]) || !strings.Contains(sections["acceptance"], add.Boxes[0]) ||
		specdispatch.CountAcceptance(sections["acceptance"]) != 2 {
		t.Errorf("the append did not land in Goal and Acceptance:\n%s", got)
	}
	for _, c := range []string{res1.LandedCommit, res2.LandedCommit} {
		if !followupIDsForCommit([]ExistingFollowup{{ID: "gt-b", Description: got}}, c)["gt-b"] {
			t.Errorf("followupIDsForCommit lost %s:\n%s", c, got)
		}
	}
	for _, raw := range []string{"## Acceptance\n- [ ] box\n", "## Goal\n- line\n"} {
		if got, ok := InsertFollowupLines(raw, add); ok || got != raw {
			t.Errorf("InsertFollowupLines(%q) = (%q, %v), want the body refused unchanged", raw, got, ok)
		}
	}
}

// TestInsertFollowupLinesStopsAtMaxAcceptance: a bundle never outgrows what one
// worker's landing carries: one-box appends in a loop stop at exactly
// MaxAcceptance boxes with every step linting clean, and an addition of two more
// boxes is refused unchanged at the edge (om finding defb2e0d65b2).
func TestInsertFollowupLinesStopsAtMaxAcceptance(t *testing.T) {
	t.Parallel()
	day := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	w, res := bundleLanding("gt-abc", bundleFirst, 0.8)
	body := bundleSpec(w, res, day, bundleGroupAt("internal/land", minorAt("internal/land/n.go", 1, "first"))).Body
	more := func(n int) FollowupAddition {
		var fs []Finding
		for i := 0; i < n; i++ {
			fs = append(fs, minorAt("internal/land/m.go", 50+i, "later "+strings.Repeat("y", i+1)))
		}
		w2, res2 := bundleLanding("gt-def", bundleSecond, 0.9)
		return bundleAddition(w2, res2, bundleGroupAt("internal/land", fs...))
	}
	var atEdge string
	for boxes := 1; ; boxes++ {
		if boxes == specdispatch.MaxAcceptance-1 {
			atEdge = body
		}
		next, ok := InsertFollowupLines(body, more(1))
		if !ok {
			if boxes != specdispatch.MaxAcceptance {
				t.Fatalf("the appends stopped at %d boxes, want %d", boxes, specdispatch.MaxAcceptance)
			}
			break
		}
		if boxes >= specdispatch.MaxAcceptance {
			t.Fatalf("the bundle grew past %d boxes", specdispatch.MaxAcceptance)
		}
		if v := specdispatch.Lint(specdispatch.Spec{ID: "gt-b", Title: "t", Type: "task", Status: "open", Description: next}, specdispatch.LoadTemplate("")); !v.Clean() {
			t.Fatalf("the body at %d boxes does not lint clean: %s", boxes+1, v.Line("gt-b"))
		}
		body = next
	}
	if got, ok := InsertFollowupLines(atEdge, more(2)); ok || got != atEdge {
		t.Errorf("two more boxes on a %d-box bundle were inserted", specdispatch.MaxAcceptance-1)
	}
}
