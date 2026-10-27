package polecatimage

import (
	"strings"
	"testing"
)

// The root chowns hand the claim to the cache user. A chown that follows a
// symlink planted in the claim re-owns the link's target, so every chown the
// pass runs as root must pass -h (gt-dv04y review round 4).
func TestSeedRootChownsDoNotFollowSymlinks(t *testing.T) {
	t.Parallel()
	h := newSeedHarness(t)
	if err := h.pass(twoPins()).Seed(); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	var chowns []string
	for _, e := range h.events {
		if strings.HasPrefix(e, "run: chown ") {
			chowns = append(chowns, e)
		}
	}
	if len(chowns) == 0 {
		t.Fatal("the seed pass ran no chown as root; the test cannot see what it hands over")
	}
	for _, e := range chowns {
		if !strings.Contains(e, " -h ") {
			t.Errorf("root chown follows symlinks: %s", e)
		}
	}
}
