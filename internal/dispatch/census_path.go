package dispatch

import (
	"fmt"
	"path/filepath"
)

// SpecCensusPath is where a daemon's tick writes the bd census rows it made,
// which the daemon logs once the tick returns (gt-139dp).
func SpecCensusPath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", "spec_census.txt")
}

// specSlingCensusPattern names one background sling's census file beside the
// tick's sink, and specSlingCensusGlob matches every one of them: each sling
// run gets its own file, so two slings never write one sink, and the daemon
// folds every file the glob matches into its census line (gt-3y319).
const (
	specSlingCensusPattern     = "spec_census_sling_%d.txt"
	specSlingCensusGlobPattern = "spec_census_sling_*.txt"
)

// SpecSlingCensusPath is where background sling number run writes the bd census
// rows it made, which the daemon folds into the tick's census line under the
// sling label (gt-3y319).
func SpecSlingCensusPath(townRoot string, run uint64) string {
	return filepath.Join(townRoot, "daemon", fmt.Sprintf(specSlingCensusPattern, run))
}

// SpecSlingCensusGlob matches the per-run census files of the background
// slings the daemon has not folded yet. The ".tmp" file WriteCensusSink writes
// before renaming does not match, so a sling caught mid-write is never read.
func SpecSlingCensusGlob(townRoot string) string {
	return filepath.Join(townRoot, "daemon", specSlingCensusGlobPattern)
}
