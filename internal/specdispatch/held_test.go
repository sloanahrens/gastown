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
			if got := HeldOverlap(c.paths, c.tests, 2, c.held).Reason; got != c.want {
				t.Errorf("HeldOverlap(%v, %v, %+v) = %q, want %q", c.paths, c.tests, c.held, got, c.want)
			}
		})
	}
}

// P0 and P1 skip the hold a P2 takes on the same overlap: an urgent bead waits
// behind no seat's work, and the tick logs the bypass naming the holder
// (gt-iydx1). A P0 that overlaps nothing stays an ordinary no-match.
func TestHeldOverlapBypassesUrgentPriorities(t *testing.T) {
	t.Parallel()
	const guard = "internal/daemon/overrun_guard.go"
	const seat = "gastown/polecats/capable"
	held := []HeldBead{{ID: "gt-t61ft", Assignee: seat, Paths: []string{guard}}}
	cases := []struct {
		name     string
		priority int
		held     []HeldBead
		want     HeldMatch
	}{
		{
			name:     "P0 bypasses the hold",
			priority: 0,
			held:     held,
			want:     HeldMatch{Bypassed: true, Holder: "gt-t61ft held by " + seat},
		},
		{
			name:     "P1 bypasses the hold",
			priority: 1,
			held:     held,
			want:     HeldMatch{Bypassed: true, Holder: "gt-t61ft held by " + seat},
		},
		{
			name:     "P2 keeps the hold",
			priority: 2,
			held:     held,
			want: HeldMatch{
				Reason: "gt-t61ft (held by " + seat + ") is already hooked to a working seat and names " + guard + "; the hold lifts when that bead closes",
				Holder: "gt-t61ft held by " + seat,
			},
		},
		{
			name:     "a P0 that overlaps nothing is unchanged",
			priority: 0,
			held:     []HeldBead{{ID: "gt-t61ft", Assignee: seat, Paths: []string{"internal/notify/cli.go"}}},
			want:     HeldMatch{},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := HeldOverlap([]string{guard}, nil, c.priority, c.held); got != c.want {
				t.Errorf("HeldOverlap(P%d) = %+v, want %+v", c.priority, got, c.want)
			}
		})
	}
}

// A bead describing an area rather than a defect shares many paths; the line
// names at most maxNamedHeldSignals of them (gt-z80p9).
func TestHeldOverlapCapsTheNamedPaths(t *testing.T) {
	t.Parallel()
	paths := []string{"a/one.go", "b/two.go", "c/three.go", "d/four.go", "e/five.go"}
	got := HeldOverlap(paths, nil, 2, []HeldBead{{ID: "gt-wide", Assignee: "gastown/polecats/capable", Paths: paths}}).Reason
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
	got := HeldOverlap(nil, tests, 2, []HeldBead{{ID: "gt-wide", Assignee: "gastown/polecats/capable", Tests: tests}}).Reason
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
	if got := HeldOverlap(candidate, nil, 2, held).Reason; got != "" {
		t.Errorf("HeldOverlap = %q, want no hold for a shared standard", got)
	}
}

// A path the bead's text marks (read-only) is one it reads, not one it changes:
// it leaves the hold's set, and the marker beats a Constraints listing, which
// otherwise makes a docs path work (gt-5v13p). A mention without the marker
// keeps the path named, and only the exact marker counts.
func TestHoldFilesReadOnlyMarker(t *testing.T) {
	t.Parallel()
	const doc = "docs/testing.md"
	const code = "internal/cmd/spec.go"
	const other = "internal/cmd/sling.go"
	cases := []struct {
		name   string
		named  []string
		listed []string
		text   []string
		want   []string
	}{
		{
			name:  "a path the text marks read-only is not a named path",
			named: []string{code, other},
			text:  []string{"Read " + code + " (read-only) first, then change " + other + "."},
			want:  []string{other},
		},
		{
			name:  "the same path without the marker is still named",
			named: []string{code},
			text:  []string{"Change " + code + "."},
			want:  []string{code},
		},
		{
			name:   "the marker drops a docs path the Constraints section lists",
			named:  []string{doc},
			listed: []string{doc},
			text:   []string{"## Constraints\nRead " + doc + " (read-only)."},
			want:   nil,
		},
		{
			name:  "one unmarked mention keeps the path named",
			named: []string{code},
			text:  []string{"Read " + code + " (read-only), then change " + code + "."},
			want:  []string{code},
		},
		{
			name:  "a marker in another field counts too",
			named: []string{code},
			text:  []string{"Read it first.", "## Constraints\nFollow " + code + " (read-only)."},
			want:  nil,
		},
		{
			name:  "a path embedded in a longer path is not a marked mention",
			named: []string{code},
			text:  []string{"Read x" + code + " (read-only)."},
			want:  []string{code},
		},
		{
			name:  "the words read-only without parentheses are not the marker",
			named: []string{code},
			text:  []string{code + " is read-only."},
			want:  []string{code},
		},
		{
			name:  "a path wrapped in backticks takes the marker",
			named: []string{code},
			text:  []string{"Peek at `" + code + "` (read-only)."},
			want:  nil,
		},
		{
			name:  "a path spelled with a leading ./ takes the marker",
			named: []string{code},
			text:  []string{"Peek at ./" + code + " (read-only)."},
			want:  nil,
		},
		{
			name:  "a path spelled with a leading ../ takes the marker",
			named: []string{code},
			text:  []string{"Peek at ../" + code + " (read-only)."},
			want:  nil,
		},
		{
			name:  "an unmarked ../ mention keeps the path named",
			named: []string{code},
			text:  []string{"Change ../" + code + "."},
			want:  []string{code},
		},
		{
			name:  "no text marks nothing",
			named: []string{code},
			want:  []string{code},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := HoldFiles(c.named, c.listed, c.text...)
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Errorf("HoldFiles(%v, %v, %v) = %v, want %v", c.named, c.listed, c.text, got, c.want)
			}
		})
	}
}

// Two beads that both cite one file with the (read-only) marker are not held
// against each other, and the same two beads without the marker are: the
// marker, not the citation, decides the hold (gt-5v13p).
func TestHeldOverlapReadOnlyMarkerDecidesTheHold(t *testing.T) {
	t.Parallel()
	const code = "internal/cmd/spec.go"
	const seat = "gastown/polecats/capable"
	names := func(text string) []string { return HoldFiles([]string{code}, nil, text) }
	cases := []struct {
		name      string
		held      string
		candidate string
		want      string
	}{
		{
			name:      "both beads read the file only",
			held:      "Read " + code + " (read-only).",
			candidate: "Consult " + code + " (read-only) before you start.",
			want:      "",
		},
		{
			name:      "neither bead marks the file",
			held:      "Change " + code + ".",
			candidate: "Change " + code + ".",
			want:      "gt-first (held by " + seat + ") is already hooked to a working seat and names " + code + "; the hold lifts when that bead closes",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			held := []HeldBead{{ID: "gt-first", Assignee: seat, Paths: names(c.held)}}
			if got := HeldOverlap(names(c.candidate), nil, 2, held).Reason; got != c.want {
				t.Errorf("HeldOverlap = %q, want %q", got, c.want)
			}
		})
	}
}

// A bead that marks one path read-only and names another is held on the other:
// the marker drops only the path it follows (gt-5v13p).
func TestHeldOverlapHoldsOnTheUnmarkedPath(t *testing.T) {
	t.Parallel()
	// Both paths are code the hold would otherwise count: a docs path the
	// Constraints section does not list is dropped whatever the marker says, so
	// it could not show the marker at work.
	const marked = "internal/cmd/spec.go"
	const code = "internal/specdispatch/held.go"
	const seat = "gastown/polecats/capable"
	paths := func(text string) []string { return HoldFiles([]string{marked, code}, nil, text) }
	held := []HeldBead{{ID: "gt-first", Assignee: seat, Paths: paths("Change " + marked + " and " + code + ".")}}
	candidate := paths("Read " + marked + " (read-only), then change " + code + ".")
	want := "gt-first (held by " + seat + ") is already hooked to a working seat and names " + code + "; the hold lifts when that bead closes"
	if got := HeldOverlap(candidate, nil, 2, held).Reason; got != want {
		t.Errorf("HeldOverlap = %q, want %q", got, want)
	}
}

// A docs/ path the Constraints section lists is work, so two beads changing
// the same repo doc still hold (gt-kr17g).
func TestHeldOverlapConstrainedDocsPathHolds(t *testing.T) {
	t.Parallel()
	const doc = "docs/design/foo.md"
	const seat = "gastown/polecats/capable"
	held := []HeldBead{{ID: "gt-first", Assignee: seat, Paths: HoldFiles([]string{doc}, []string{doc})}}
	got := HeldOverlap(HoldFiles([]string{doc}, []string{doc}), nil, 2, held).Reason
	want := "gt-first (held by " + seat + ") is already hooked to a working seat and names " + doc + "; the hold lifts when that bead closes"
	if got != want {
		t.Errorf("HeldOverlap = %q, want %q", got, want)
	}
}

// A seat the hold protects is a polecat's, and its bead is not a handoff note:
// a crew session's bead and a 45 KB handoff log otherwise hold every candidate
// naming a file they mention (gt-asvow).
func TestWorksSeat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		assignee string
		labels   []string
		want     bool
	}{
		{name: "a polecat seat", assignee: "gastown/polecats/capable", want: true},
		{name: "a polecat seat's work label", assignee: "gastown/polecats/capable", labels: []string{"rework", "gt:task"}, want: true},
		// The seat is read from the path's components, not its length: a
		// shape that names a polecats seat holds whatever else the path
		// carries (gt-asvow).
		{name: "a rig-relative seat", assignee: "polecats/capable", want: true},
		{name: "a seat with a trailing slash", assignee: "gastown/polecats/capable/", want: true},
		{name: "a seat path with a segment after the name", assignee: "gastown/polecats/capable/session", want: true},
		{name: "a seat path inside another directory", assignee: "gastown/crew/polecats/capable", want: true},
		{name: "a crew session", assignee: "town/crew/overseer", labels: []string{"gt:task"}, want: false},
		{name: "a crew handoff bead", assignee: "town/crew/overseer", labels: []string{"handoff"}, want: false},
		{name: "a rig crew session", assignee: "gastown/crew/sloan", labels: []string{"rework"}, want: false},
		{name: "a polecat's handoff-labeled bead is a handoff note too", assignee: "gastown/polecats/capable", labels: []string{"gt:handoff"}, want: false},
		{name: "the label's spelling does not matter", assignee: "gastown/polecats/capable", labels: []string{" HANDOFF "}, want: false},
		{name: "an operator session", assignee: "operator", want: false},
		{name: "a person", assignee: "sloan", want: false},
		{name: "no assignee", assignee: "", want: false},
		{name: "a polecats path that is not a seat", assignee: "gastown/polecat/capable", want: false},
		{name: "a seat with no name", assignee: "gastown/polecats/", want: false},
		{name: "a polecats component with no name after it", assignee: "gastown/polecats//capable", want: false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := WorksSeat(c.assignee, c.labels); got != c.want {
				t.Errorf("WorksSeat(%q, %v) = %v, want %v", c.assignee, c.labels, got, c.want)
			}
		})
	}
}
