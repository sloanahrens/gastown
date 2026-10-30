package templates

import (
	"strings"
	"testing"
)

// within answers "cannot tell" as inside, so the scratch root check refuses a
// pair it could not compare instead of accepting it (gt-qpeei.29).
func TestWithin_ACannotComparePairCountsAsInside(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, path, root string
		want             bool
	}{
		{"same path", "/towns/live", "/towns/live", true},
		{"under the root", "/towns/live/crew", "/towns/live", true},
		{"beside the root", "/towns/scratch", "/towns/live", false},
		{"above the root", "/towns", "/towns/live", false},
		{"same path in another case", "/Towns/Live", "/towns/live", true},
		{"relative path against an absolute root", ".", "/towns/live", true},
		{"absolute path against a relative root", "/towns/live", ".", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := within(c.path, c.root); got != c.want {
				t.Fatalf("within(%q, %q) = %v, want %v", c.path, c.root, got, c.want)
			}
		})
	}
}

// checkScratchRoot refuses a relative root on either side by name, before any
// resolution: a relative scratch root (for example "." with the working directory
// at the live root) must never read as clear.
func TestCheckScratchRoot_RefusesARelativeRoot(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		town, live   string
		wantContains string
	}{
		{"relative scratch root", ".", "/towns/live", "must both be absolute"},
		{"relative live root", "/towns/scratch", "towns/live", "must both be absolute"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := ComposeSpec{TownRoot: c.town, LiveTownRoot: c.live}.checkScratchRoot()
			if err == nil || !strings.Contains(err.Error(), c.wantContains) {
				t.Fatalf("checkScratchRoot(%q, %q) = %v, want an error containing %q", c.town, c.live, err, c.wantContains)
			}
		})
	}
}
