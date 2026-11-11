package beads

import "testing"

// TestRigBeadsRelPath pins the rig-relative redirect target. It is the legacy
// mayor/rig/.beads whatever the working directory holds: a redirect written
// from a directory with its own .beads/metadata.json must not point at .beads.
func TestRigBeadsRelPath(t *testing.T) {
	t.Parallel()
	const want = "mayor/rig/.beads"
	if got := rigBeadsRelPath(); got != want {
		t.Errorf("rigBeadsRelPath() = %q, want %q", got, want)
	}
}
