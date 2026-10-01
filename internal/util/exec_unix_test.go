//go:build !windows

package util

import "testing"

// TestKillProcessGroupIDRefusesWholeSystemIDs: kill(-1) signals every process
// the caller may, and 0 is the caller's own group, so a recorded id that
// reads as either is a bug to refuse, not obey.
func TestKillProcessGroupIDRefusesWholeSystemIDs(t *testing.T) {
	t.Parallel()
	for _, pgid := range []int{-1, 0, 1} {
		if err := KillProcessGroupID(pgid); err == nil {
			t.Errorf("KillProcessGroupID(%d) = nil, want a refusal", pgid)
		}
	}
}
