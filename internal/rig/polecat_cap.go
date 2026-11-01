package rig

// The polecat directory cap is one number a rig holds against, read by two
// checks with different units: the sling's admission check counts the rig's live
// seats against it (internal/cmd's reuseOrRefuseAtDirCap), and the daemon counts
// the rig's polecat directories against it before trimming an idle sandbox seat
// (internal/daemon's patrolScanHost.TrimmableSeats). Both read it from here so
// neither can drift from the other.

// MinPolecatDirsPerRig is the floor of a rig's polecat directory cap: a rig
// configured with no cap, or one below this, still holds this many. It is a
// floor rather than a default so a rig's max_polecats key can only raise the
// cap (649b832b7).
const MinPolecatDirsPerRig = 30

// EffectivePolecatDirCap is the cap a rig holds against: configured floored at
// MinPolecatDirsPerRig. An unset or negative configured value is the floor.
func EffectivePolecatDirCap(configured int) int {
	if configured < MinPolecatDirsPerRig {
		return MinPolecatDirsPerRig
	}
	return configured
}
