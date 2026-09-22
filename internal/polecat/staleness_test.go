package polecat

import "testing"

// TestAssessStalenessHonoursPauseMarker pins the pause gate for the polecat
// staleness assessor (gt-ahik, om kgx0): a polecat paused via the marker
// file must never be swept up as stale, even with a dead session and no
// agent bead at all — the exact shape a paused polecat's dead session takes
// once the bead mirror write failed or never ran.
func TestAssessStalenessHonoursPauseMarker(t *testing.T) {
	info := &StalenessInfo{
		Name:          "flint",
		CommitsBehind: 999,
		MarkerPaused:  true,
		PausedReason:  "operator hold",
		// No AgentState, no HasActiveSession — looks exactly like an
		// abandoned polecat to every other check.
	}
	isStale, reason := assessStaleness(info, 20)
	if isStale {
		t.Fatalf("assessStaleness(%+v) = stale, want protected by the pause marker (reason: %q)", info, reason)
	}
	if reason == "" {
		t.Error("assessStaleness gave no reason for skipping a marker-paused polecat")
	}
}

// TestAssessStalenessBeadMirrorAloneStillProtects confirms the bead-layer
// check still runs as a second line of defense when the marker is absent
// (e.g. an older pause, or a marker that was hand-deleted) but the bead
// mirror still reads paused.
func TestAssessStalenessBeadMirrorAloneStillProtects(t *testing.T) {
	info := &StalenessInfo{
		Name:          "flint",
		CommitsBehind: 999,
		AgentState:    "paused",
	}
	isStale, _ := assessStaleness(info, 20)
	if isStale {
		t.Fatalf("assessStaleness(%+v) = stale, want protected by the bead mirror", info)
	}
}

// TestAssessStalenessUnpausedStillGetsSwept confirms the marker check isn't
// a blanket "never clean up" — an unpaused, far-behind, sessionless polecat
// is still swept.
func TestAssessStalenessUnpausedStillGetsSwept(t *testing.T) {
	info := &StalenessInfo{
		Name:          "flint",
		CommitsBehind: 999,
	}
	isStale, _ := assessStaleness(info, 20)
	if !isStale {
		t.Fatalf("assessStaleness(%+v) = not stale, want swept (nothing protects it)", info)
	}
}
