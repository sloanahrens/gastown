package omlessons

import (
	"hash/fnv"
	"slices"
	"time"
)

// The hold-out withholds the follow-up for 20 percent of beads in two low-risk
// classes for four weeks (Q37).
const (
	HoldOutPercent = 20
	HoldOutDays    = 28
)

// holdOutStart is the first instant of the window. Move it if the release slips.
var holdOutStart = time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)

// holdOutClasses are the classes whose follow-up is withheld: rule 7 and 14.
var holdOutClasses = []string{"doc-contradicts-code", "non-hermetic-test"}

// HoldOutStart returns the first instant of the hold-out window.
func HoldOutStart() time.Time { return holdOutStart }

// HoldOutClasses returns a copy of the class names, so a caller cannot change
// what the hold-out withholds.
func HoldOutClasses() []string { return slices.Clone(holdOutClasses) }

// HeldOut reports whether the follow-up for bead is withheld in classID at now.
// It reads only its arguments, so the feed and the report agree without storage.
func HeldOut(bead, classID string, now time.Time) bool {
	end := holdOutStart.AddDate(0, 0, HoldOutDays)
	if !slices.Contains(holdOutClasses, classID) || now.Before(holdOutStart) || !now.Before(end) {
		return false
	}
	return bucket(bead) < uint32(HoldOutPercent)
}

// bucket is the FNV-1a hash of the bead id modulo 100.
func bucket(bead string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(bead))
	return h.Sum32() % 100
}
