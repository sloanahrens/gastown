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
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := HeldOverlap(c.paths, c.held); got != c.want {
				t.Errorf("HeldOverlap(%v, %+v) = %q, want %q", c.paths, c.held, got, c.want)
			}
		})
	}
}

// A bead describing an area rather than a defect shares many paths; the line
// names at most maxNamedHeldPaths of them (gt-z80p9).
func TestHeldOverlapCapsTheNamedPaths(t *testing.T) {
	t.Parallel()
	paths := []string{"a/one.go", "b/two.go", "c/three.go", "d/four.go", "e/five.go"}
	got := HeldOverlap(paths, []HeldBead{{ID: "gt-wide", Assignee: "gastown/polecats/capable", Paths: paths}})
	want := "gt-wide (held by gastown/polecats/capable) is already hooked to a working seat and names a/one.go, b/two.go, c/three.go (+2 more); the hold lifts when that bead closes"
	if got != want {
		t.Errorf("HeldOverlap = %q, want %q", got, want)
	}
}

// A bead reads the standards it cites without changing them, so those paths
// are not held; a path the Constraints section lists is work and stays
// (gt-6t44f).
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
			name:   "a reference doc the Constraints section lists stays",
			named:  []string{standard, "internal/notify/cli.go"},
			listed: []string{standard},
			want:   []string{standard, "internal/notify/cli.go"},
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
