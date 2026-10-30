package dispatch

import "path/filepath"

// SpecCensusPath is where a daemon's tick writes the bd census rows it made,
// which the daemon logs once the tick returns (gt-139dp).
func SpecCensusPath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", "spec_census.txt")
}
