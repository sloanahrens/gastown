package metrics

import (
	"strconv"
	"strings"
)

// A runtime era is the stretch of landings the town ran under one seat and om
// regime. The eras are cut at the two platform moves the regime log holds for
// the sandbox and the pod, so a landing's era is read from its landed_at against
// those instants (backfill.go).
const (
	eraPreSandbox = "pre-sandbox"
	eraSandbox    = "sandbox"
	eraPod        = "pod"
	// eraUnknown is a landing whose landed_at names no instant, so no era can
	// hold it. It is counted under its own name rather than with a real era.
	eraUnknown = "unknown"
)

// eraHoldoutMinLandings is the fewest landings the last era must hold before the
// holdout is that whole era. Under it, the holdout is the later 30% of the last
// era instead.
const eraHoldoutMinLandings = 300

// eraOrder is the order the era counts are written in, earliest era first.
var eraOrder = []string{eraPreSandbox, eraSandbox, eraPod, eraUnknown}

// runtimeEra is the era a landing at landedAt ran under. A landedAt that names
// no instant is eraUnknown, never a guess at its era.
func runtimeEra(landedAt string) string {
	at, err := ParseTS(landedAt)
	if err != nil {
		return eraUnknown
	}
	switch {
	case at.Before(regimeSandboxMoveAt):
		return eraPreSandbox
	case at.Before(regimePodMoveAt):
		return eraSandbox
	}
	return eraPod
}

// SplitByEra cuts rows into train and holdout so the holdout is always inside one
// runtime era, and a change of regime between the eras is not read as a failed
// pattern. The last era is the era of the latest landing. When that era holds at
// least eraHoldoutMinLandings landings and earlier landings exist, train is every
// earlier landing and holdout is the whole last era. Otherwise train is every
// earlier landing plus the earliest defaultTrainFraction of the last era, and the
// holdout is the rest of that era, so the cut stays inside one era either way.
//
// Each row's era is read from its landed_at, not from the runtime_era cell, so a
// landings table written before that column existed splits the same way.
func SplitByEra(rows []Row) (train, holdout []Row) {
	ordered := sortedByLandedAt(rows)
	if len(ordered) == 0 {
		return nil, nil
	}
	last := runtimeEra(ordered[len(ordered)-1]["landed_at"])
	start := len(ordered) - 1
	for start > 0 && runtimeEra(ordered[start-1]["landed_at"]) == last {
		start--
	}
	earlier, lastEra := ordered[:start], ordered[start:]
	if len(lastEra) >= eraHoldoutMinLandings && len(earlier) > 0 {
		return earlier, lastEra
	}
	inTrain, inHoldout := SplitByDate(lastEra, defaultTrainFraction)
	train = make([]Row, 0, len(earlier)+len(inTrain))
	train = append(append(train, earlier...), inTrain...)
	return train, inHoldout
}

// panelEras is each side of the split's era counts, cut the way the analysis
// cuts its split.
func panelEras(rows []Row) PanelEras {
	train, holdout := SplitByEra(rows)
	return PanelEras{Train: eraCounts(train), Holdout: eraCounts(holdout)}
}

// eraCounts is how many of rows fall in each runtime era, read from each row's
// landed_at. An era with no row is absent from the map.
func eraCounts(rows []Row) map[string]int {
	counts := make(map[string]int)
	for _, r := range rows {
		counts[runtimeEra(r["landed_at"])]++
	}
	return counts
}

// eraCountsText writes counts in era order, as "sandbox 120, pod 80": the words
// the digest puts beside a split's row counts. A count of zero is left out, and
// no era at all writes "none".
func eraCountsText(counts map[string]int) string {
	var parts []string
	for _, era := range eraOrder {
		if n := counts[era]; n > 0 {
			parts = append(parts, era+" "+strconv.Itoa(n))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}
