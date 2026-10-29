package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// issueOfSpec is the bead a fixture spec stands for, in a bead store.
func issueOfSpec(s specdispatch.Spec) beads.Issue {
	return beads.Issue{
		ID: s.ID, Title: s.Title, Type: s.Type, Status: s.Status, Priority: s.Priority,
		CreatedAt: s.CreatedAt, Labels: s.Labels, Description: s.Description,
		AcceptanceCriteria: s.Acceptance, Assignee: s.Assignee,
	}
}

// specTickIssues is the sixteen ready beads the tick golden runs over: clean
// ones that dispatch or wait on the per-tick limit, unshaped ones, containers,
// a second filing of held work, a bead whose fresh read is taken, and formula
// declarations good and bad. Every one is a candidate on the board.
func specTickIssues() []beads.Issue {
	issue := issueOfSpec
	shapeless := func(id string) specdispatch.Spec {
		s := cleanSpec(id, 1, "2026-09-29T09:00:00Z")
		s.Description = strings.Replace(s.Description, "## Gate\nmake gate", "", 1)
		return s
	}
	second := cleanSpec("gt-second", 2, "2026-09-29T10:03:45Z")
	second.Description = specTestDescription + "\n\nToday internal/daemon/overrun_guard.go (~254) calls Nudge with a 30 s context."
	goodFormula := formulaSpecBead("gt-fmt", []string{"formula:mol-deep-review"}, "scope=all")
	badFormula := formulaSpecBead("gt-nofm", []string{"formula:mol-gone"}, "a=1")
	return []beads.Issue{
		issue(cleanSpec("gt-c1", 1, "2026-09-29T10:00:00Z")),
		issue(cleanSpec("gt-c2", 1, "2026-09-29T11:00:00Z")),
		issue(cleanSpec("gt-c3", 2, "2026-09-29T10:00:00Z")),
		issue(cleanSpec("gt-c4", 2, "2026-09-29T11:00:00Z")),
		issue(cleanSpec("gt-c5", 2, "2026-09-29T12:00:00Z")),
		issue(cleanSpec("gt-c6", 2, "2026-09-29T13:00:00Z")),
		issue(shapeless("gt-bad1")),
		issue(shapeless("gt-bad2")),
		issue(cleanSpec("gt-box", 1, "2026-09-29T08:00:00Z")),
		issue(cleanSpec("gt-box2", 2, "2026-09-29T08:30:00Z")),
		issue(second),
		issue(cleanSpec("gt-flip", 1, "2026-09-29T07:00:00Z")),
		issue(badFormula),
		issue(goodFormula),
		issue(cleanSpec("gt-late", 2, "2026-09-29T14:00:00Z")),
		issue(cleanSpec("gt-last", 2, "2026-09-29T15:00:00Z")),
	}
}

// specGoldenTown is the fake town the golden runs over: specTickIssues as the
// board, the container's open child, the taken bead's fresh read, and the
// formula the rig lacks.
func specGoldenTown() *fakeSpecTown {
	var specs []specdispatch.Spec
	for _, i := range specTickIssues() {
		i := i
		specs = append(specs, specFromIssue(&i))
	}
	f := newFakeSpecTown(specs...)
	f.children["gt-box"] = []specdispatch.Child{{ID: "gt-box.1", Status: "open"}}
	f.children["gt-box2"] = []specdispatch.Child{{ID: "gt-box2.1", Status: "closed"}}
	flip := f.specs["gt-flip"]
	flip.Status, flip.Assignee = "hooked", "gastown/polecats/other"
	f.showOverride = map[string]specdispatch.Spec{"gt-flip": flip}
	f.missingFormulas = map[string]bool{"mol-gone": true}
	return f
}

// renderSpecTick is what a tick decided, as text: the dispatch order first,
// then every report line by group, then the notices and errors.
func renderSpecTick(slung []string, r specDispatchReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "slung: %s\n", strings.Join(slung, " "))
	fmt.Fprintf(&b, "candidates: %d\n", r.Candidates)
	for _, group := range []struct {
		name    string
		entries []specDispatchEntry
	}{{"dispatched", r.Dispatched}, {"refused", r.Refused}, {"planning", r.Planning}, {"skipped", r.Skipped}, {"failed", r.Failed}} {
		for _, e := range group.entries {
			fmt.Fprintf(&b, "%s: %s\n", group.name, e.Line)
		}
	}
	for _, n := range r.Notices {
		fmt.Fprintf(&b, "note: %s\n", n)
	}
	for _, e := range r.Errors {
		fmt.Fprintf(&b, "error: %s\n", e)
	}
	return b.String()
}

func readSpecTickGolden(t *testing.T) string {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("testdata", "spec_dispatch_tick.golden"))
	if err != nil {
		t.Fatalf("reading the golden: %v", err)
	}
	return string(want)
}

// TestSpecDispatchTickGolden pins the skip reasons and the dispatch order of
// one tick over sixteen candidates. The golden was written before the tick's
// reads were cut, so a change to which beads dispatch, in what order, or to any
// printed reason shows here (gt-u8rbh).
func TestSpecDispatchTickGolden(t *testing.T) {
	t.Parallel()
	f := specGoldenTown()
	env := f.env()
	env.PerTick = 3
	env.ShapeGate = specdispatch.ShapeGateWarn
	r := runSpecDispatchCycle(env)
	if got, want := renderSpecTick(f.slung, r), readSpecTickGolden(t); got != want {
		t.Fatalf("tick differs from testdata/spec_dispatch_tick.golden:\n--- got\n%s--- want\n%s", got, want)
	}
}
