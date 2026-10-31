package dispatch

import (
	"path/filepath"
)

// SpecCensusPath is where a daemon's tick writes the bd census rows it made,
// which the daemon logs once the tick returns (gt-139dp).
func SpecCensusPath(townRoot string) string {
	return filepath.Join(townRoot, "daemon", "spec_census.txt")
}

// specSlingCensusPrefix and specSlingCensusSuffix name one background sling's
// census file beside the tick's sink: each sling run gets its own file, so two
// slings never write one sink, and the daemon folds every matching file into
// its census line (gt-3y319).
const (
	specSlingCensusPrefix = "spec_census_sling_"
	specSlingCensusSuffix = ".txt"
)

// SpecSlingCensusPath is where the background sling named run writes the bd
// census rows it made, which the daemon folds into the tick's census line under
// the sling label (gt-3y319).
func SpecSlingCensusPath(townRoot, run string) string {
	return filepath.Join(townRoot, "daemon", specSlingCensusPrefix+run+specSlingCensusSuffix)
}

// SpecSlingCensusGlob matches the per-run census files of the background
// slings the daemon has not folded yet. The ".tmp" file WriteCensusSink writes
// before renaming does not match, so a sling caught mid-write is never read.
func SpecSlingCensusGlob(townRoot string) string {
	return filepath.Join(townRoot, "daemon", specSlingCensusPrefix+"*"+specSlingCensusSuffix)
}
