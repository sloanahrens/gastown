package cmd

import (
	"errors"
	"fmt"
	"testing"

	"github.com/steveyegge/gastown/internal/polecat"
)

// noRepoToProtect is true only for a bead that routes to no rig. A rig root
// with no .repo.git is an unknown answer, so it must not read as nothing to
// protect and release the hook.
func TestNoRepoToProtectIsOnlyABeadThatRoutesToNoRig(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "bead routes to no rig", err: fmt.Errorf("gt-elvf4: %w", errBeadRoutesToNoRig), want: true},
		{name: "rig has no .repo.git is unknown", err: fmt.Errorf("%w under /town/gastown", polecat.ErrNoBareRepo)},
		{name: "origin unreachable is unknown", err: errors.New("origin unreachable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := noRepoToProtect(tc.err); got != tc.want {
				t.Fatalf("noRepoToProtect(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
