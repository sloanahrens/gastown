package cmd

import "testing"

// TestDoneLandingFlagsAreGone: gt done has no landing modes and no polecat
// gate bypass (ADR 0004). --pre-verified exists for crew only and the
// polecat path refuses it (runDone).
func TestDoneLandingFlagsAreGone(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"skip-tests", "skip-verify", "merge", "resume", "priority"} {
		if doneCmd.Flags().Lookup(name) != nil {
			t.Errorf("gt done still has --%s", name)
		}
	}
}
