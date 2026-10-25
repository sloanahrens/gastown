package specdispatch

import (
	"fmt"
	"testing"
)

// The hold is the gt-t61ft/gt-wikw1 pair replayed: two beads on one defect,
// filed minutes apart, sharing the file that carries the fix and no test name.
func TestHeldOverlap(t *testing.T) {
	t.Parallel()
	const guard = "internal/daemon/overrun_guard.go"
	const seat = "gastown/polecats/capable"
	cases := []struct {
		name  string
		paths []string
		tests []string
		held  []HeldBead
		want  string
	}{
		{
			name:  "nothing held is no hold",
			paths: []string{guard},
			held:  nil,
			want:  "",
		},
		{
			name:  "a candidate naming no path cannot overlap",
			paths: nil,
			held:  []HeldBead{{ID: "gt-t61ft", Assignee: seat, Paths: []string{guard}}},
			want:  "",
		},
		{
			name:  "distinct paths do not hold",
			paths: []string{"internal/notify/cli.go"},
			held:  []HeldBead{{ID: "gt-t61ft", Assignee: seat, Paths: []string{guard}}},
			want:  "",
		},
		{
			name:  "a shared path holds and names its seat",
			paths: []string{"internal/notify/cli.go", guard},
			held:  []HeldBead{{ID: "gt-t61ft", Assignee: seat, Paths: []string{guard}}},
			want:  "gt-t61ft (held by " + seat + ") is already hooked to a working seat and names " + guard + "; the hold lifts when that bead closes",
		},
		{
			name:  "a held bead with no assignee does not hold",
			paths: []string{guard},
			held:  []HeldBead{{ID: "gt-t61ft", Paths: []string{guard}}},
			want:  "",
		},
		{
			name:  "an unassigned bead does not shadow an assigned one with a higher id",
			paths: []string{guard},
			held:  []HeldBead{{ID: "gt-a1", Paths: []string{guard}}, {ID: "gt-z9", Assignee: seat, Paths: []string{guard}}},
			want:  "gt-z9 (held by " + seat + ") is already hooked to a working seat and names " + guard + "; the hold lifts when that bead closes",
		},
		{
			name:  "an id-less entry is skipped",
			paths: []string{guard},
			held:  []HeldBead{{Paths: []string{guard}}, {ID: "gt-two", Assignee: seat, Paths: []string{guard}}},
			want:  "gt-two (held by " + seat + ") is already hooked to a working seat and names " + guard + "; the hold lifts when that bead closes",
		},
		{
			name:  "the lowest overlapping id is named, whatever the read order",
			paths: []string{guard},
			held:  []HeldBead{{ID: "gt-z9", Assignee: seat, Paths: []string{guard}}, {ID: "gt-a1", Assignee: "gastown/polecats/other", Paths: []string{guard}}},
			want:  "gt-a1 (held by gastown/polecats/other) is already hooked to a working seat and names " + guard + "; the hold lifts when that bead closes",
		},
		{
			name:  "a held bead naming other paths too does not hold",
			paths: []string{"internal/notify/cli.go"},
			held:  []HeldBead{{ID: "gt-t61ft", Assignee: seat, Paths: []string{guard, "internal/nudge/deliver/deliver.go"}}},
			want:  "",
		},
		// A shared test is the signal the sling-time guard refuses live work
		// on, so the pre-dispatch hold reads it too: the dv-nnjl/dv-613z/
		// dv-fec7.10 cluster named tests/host-rehearsal and nothing else
		// distinct enough to hold on files (gt-mqa3r).
		{
			name:  "a shared test holds",
			tests: []string{"TestHostRehearsal"},
			held:  []HeldBead{{ID: "gt-t61ft", Assignee: seat, Tests: []string{"TestHostRehearsal"}}},
			want:  "gt-t61ft (held by " + seat + ") is already hooked to a working seat and runs TestHostRehearsal; the hold lifts when that bead closes",
		},
		{
			name:  "a test family holds",
			tests: []string{"TestHostRehearsal_StaleSandbox"},
			held:  []HeldBead{{ID: "gt-t61ft", Assignee: seat, Tests: []string{"TestHostRehearsal"}}},
			want:  "gt-t61ft (held by " + seat + ") is already hooked to a working seat and runs TestHostRehearsal_StaleSandbox; the hold lifts when that bead closes",
		},
		{
			name:  "distinct tests do not hold",
			tests: []string{"TestNotifyCLI"},
			held:  []HeldBead{{ID: "gt-t61ft", Assignee: seat, Tests: []string{"TestHostRehearsal"}}},
			want:  "",
		},
		{
			name:  "a held bead with no assignee holds nothing by test either",
			tests: []string{"TestHostRehearsal"},
			held:  []HeldBead{{ID: "gt-t61ft", Tests: []string{"TestHostRehearsal"}}},
			want:  "",
		},
		{
			name:  "paths and tests are named together",
			paths: []string{guard},
			tests: []string{"TestHostRehearsal"},
			held:  []HeldBead{{ID: "gt-t61ft", Assignee: seat, Paths: []string{guard}, Tests: []string{"TestHostRehearsal"}}},
			want:  "gt-t61ft (held by " + seat + ") is already hooked to a working seat and names " + guard + " and runs TestHostRehearsal; the hold lifts when that bead closes",
		},
		{
			name:  "the lowest id wins across a path match and a test match",
			paths: []string{guard},
			tests: []string{"TestHostRehearsal"},
			held: []HeldBead{
				{ID: "gt-z9", Assignee: seat, Tests: []string{"TestHostRehearsal"}},
				{ID: "gt-a1", Assignee: "gastown/polecats/other", Paths: []string{guard}},
			},
			want: "gt-a1 (held by gastown/polecats/other) is already hooked to a working seat and names " + guard + "; the hold lifts when that bead closes",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := HeldOverlap(c.paths, c.tests, c.held); got != c.want {
				t.Errorf("HeldOverlap(%v, %v, %+v) = %q, want %q", c.paths, c.tests, c.held, got, c.want)
			}
		})
	}
}

// A bead describing an area rather than a defect shares many paths; the line
// names at most maxNamedHeldSignals of them (gt-z80p9).
func TestHeldOverlapCapsTheNamedPaths(t *testing.T) {
	t.Parallel()
	paths := []string{"a/one.go", "b/two.go", "c/three.go", "d/four.go", "e/five.go"}
	got := HeldOverlap(paths, nil, []HeldBead{{ID: "gt-wide", Assignee: "gastown/polecats/capable", Paths: paths}})
	want := "gt-wide (held by gastown/polecats/capable) is already hooked to a working seat and names a/one.go, b/two.go, c/three.go (+2 more); the hold lifts when that bead closes"
	if got != want {
		t.Errorf("HeldOverlap = %q, want %q", got, want)
	}
}

// The tests a hold line names are capped apart from the paths, so a bead that
// names one file and a whole failing suite still reads as one line.
func TestHeldOverlapCapsTheNamedTests(t *testing.T) {
	t.Parallel()
	tests := []string{"TestA", "TestB", "TestC", "TestD"}
	got := HeldOverlap(nil, tests, []HeldBead{{ID: "gt-wide", Assignee: "gastown/polecats/capable", Tests: tests}})
	want := "gt-wide (held by gastown/polecats/capable) is already hooked to a working seat and runs TestA, TestB, TestC (+1 more); the hold lifts when that bead closes"
	if got != want {
		t.Errorf("HeldOverlap = %q, want %q", got, want)
	}
}

// A bead reads the standards it cites without changing them, so those paths
// are not held; a docs/ path the Constraints section lists is work and stays,
// while a claude/docs path never stays because no polecat edits ~/.claude
// (gt-6t44f, gt-kr17g).
func TestHoldFiles(t *testing.T) {
	t.Parallel()
	const standard = "claude/docs/standards/test-engineering.md"
	cases := []struct {
		name   string
		named  []string
		listed []string
		want   []string
	}{
		{
			name:  "a bead that only reads a standard names no hold path",
			named: []string{standard},
			want:  nil,
		},
		{
			name:  "a repo docs path is reference-only too",
			named: []string{"docs/reference.md", "internal/notify/cli.go"},
			want:  []string{"internal/notify/cli.go"},
		},
		{
			name:  "a home-relative standards path is reference-only",
			named: []string{"~/.claude/docs/agents/spec-template.md"},
			want:  nil,
		},
		{
			name:  "an absolute path into the standards tree is reference-only",
			named: []string{"Users/me/.claude/docs/standards/test-engineering.md"},
			want:  nil,
		},
		{
			name:  "a path that merely embeds claude/docs mid-segment stays",
			named: []string{"internal/myclaude/docs/render.go"},
			want:  []string{"internal/myclaude/docs/render.go"},
		},
		{
			name:   "a standard the Constraints section lists still drops",
			named:  []string{standard, "internal/notify/cli.go"},
			listed: []string{standard},
			want:   []string{"internal/notify/cli.go"},
		},
		{
			name:   "a docs path the Constraints section lists stays",
			named:  []string{"docs/design/foo.md", "internal/notify/cli.go"},
			listed: []string{"docs/design/foo.md"},
			want:   []string{"docs/design/foo.md", "internal/notify/cli.go"},
		},
		{
			name:  "an absolute path into the standards tree is reference-only",
			named: []string{"/home/me/.claude/docs/standards/test-engineering.md"},
			want:  nil,
		},
		{
			name:  "an absolute path under another claude/docs tree stays",
			named: []string{"/home/me/claude/docs/standards/test-engineering.md"},
			want:  []string{"/home/me/claude/docs/standards/test-engineering.md"},
		},
		{
			name:  "a repo path with a claude/docs directory inside it stays",
			named: []string{"internal/x/claude/docs/render.go"},
			want:  []string{"internal/x/claude/docs/render.go"},
		},
		{
			name:  "a source file is never reference-only",
			named: []string{"internal/cmd/spec.go"},
			want:  []string{"internal/cmd/spec.go"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := HoldFiles(c.named, c.listed)
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("HoldFiles(%v, %v) = %v, want %v", c.named, c.listed, got, c.want)
			}
		})
	}
}

// Two beads citing the test-engineering standard under Constraints both name a
// file neither can change, so neither holds the other (gt-kr17g).
func TestHeldOverlapStandardUnderConstraintsDoesNotHold(t *testing.T) {
	t.Parallel()
	const standard = "claude/docs/standards/test-engineering.md"
	held := []HeldBead{{ID: "gt-first", Assignee: "gastown/polecats/capable",
		Paths: HoldFiles([]string{standard}, []string{standard})}}
	candidate := HoldFiles([]string{standard}, []string{standard})
	if got := HeldOverlap(candidate, nil, held); got != "" {
		t.Errorf("HeldOverlap = %q, want no hold for a shared standard", got)
	}
}

// A docs/ path the Constraints section lists is work, so two beads changing
// the same repo doc still hold (gt-kr17g).
func TestHeldOverlapConstrainedDocsPathHolds(t *testing.T) {
	t.Parallel()
	const doc = "docs/design/foo.md"
	const seat = "gastown/polecats/capable"
	held := []HeldBead{{ID: "gt-first", Assignee: seat, Paths: HoldFiles([]string{doc}, []string{doc})}}
	got := HeldOverlap(HoldFiles([]string{doc}, []string{doc}), nil, held)
	want := "gt-first (held by " + seat + ") is already hooked to a working seat and names " + doc + "; the hold lifts when that bead closes"
	if got != want {
		t.Errorf("HeldOverlap = %q, want %q", got, want)
	}
}
