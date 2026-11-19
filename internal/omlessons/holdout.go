package omlessons

import (
	"hash/fnv"
	"slices"
	"time"
)

// The hold-out withholds the follow-up for 20 percent of beads in two low-risk
// classes for four weeks (Q37). Move HoldOutStart if the release slips.
var (
	HoldOutPercent = 20
	HoldOutDays    = 28
	HoldOutStart   = time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)         // 00:00 UTC
	HoldOutClasses = []string{"doc-contradicts-code", "non-hermetic-test"} // rule 7 and 14
)

// HeldOut reports whether the follow-up for bead is withheld in classID at now.
// It reads only its arguments, so the feed and the report agree without storage.
func HeldOut(bead, classID string, now time.Time) bool {
	end := HoldOutStart.AddDate(0, 0, HoldOutDays)
	if !slices.Contains(HoldOutClasses, classID) || now.Before(HoldOutStart) || !now.Before(end) {
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
