package specdispatch

import "testing"

// The hold is the gt-t61ft/gt-wikw1 pair replayed: two beads on one defect,
// filed minutes apart, sharing the file that carries the fix and no test name.
func TestHeldOverlap(t *testing.T) {
	t.Parallel()
	const guard = "internal/daemon/overrun_guard.go"
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
			held:  []HeldBead{{ID: "gt-t61ft", Paths: []string{guard}}},
			want:  "",
		},
		{
			name:  "distinct paths do not hold",
			paths: []string{"internal/notify/cli.go"},
			held:  []HeldBead{{ID: "gt-t61ft", Paths: []string{guard}}},
			want:  "",
		},
		{
			name:  "a shared path holds",
			paths: []string{"internal/notify/cli.go", guard},
			held:  []HeldBead{{ID: "gt-t61ft", Paths: []string{guard}}},
			want:  "gt-t61ft is already hooked to a working seat and names " + guard + "; the hold lifts when that bead closes",
		},
		{
			name:  "an id-less entry is skipped",
			paths: []string{guard},
			held:  []HeldBead{{Paths: []string{guard}}, {ID: "gt-two", Paths: []string{guard}}},
			want:  "gt-two is already hooked to a working seat and names " + guard + "; the hold lifts when that bead closes",
		},
		{
			name:  "the lowest overlapping id is named, whatever the read order",
			paths: []string{guard},
			held:  []HeldBead{{ID: "gt-z9", Paths: []string{guard}}, {ID: "gt-a1", Paths: []string{guard}}},
			want:  "gt-a1 is already hooked to a working seat and names " + guard + "; the hold lifts when that bead closes",
		},
		{
			name:  "a held bead naming other paths too does not hold",
			paths: []string{"internal/notify/cli.go"},
			held:  []HeldBead{{ID: "gt-t61ft", Paths: []string{guard, "internal/nudge/deliver/deliver.go"}}},
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
	got := HeldOverlap(paths, []HeldBead{{ID: "gt-wide", Paths: paths}})
	want := "gt-wide is already hooked to a working seat and names a/one.go, b/two.go, c/three.go (+2 more); the hold lifts when that bead closes"
	if got != want {
		t.Errorf("HeldOverlap = %q, want %q", got, want)
	}
}
