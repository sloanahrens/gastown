package metrics

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

// A runtime era is the stretch of landings the town ran under one seat and om
// regime. The eras are cut at the two platform moves: the sandbox move and the
// pod move. Each cut is read from the regime log (readEraCuts), and the backfill
// constants answer for a move the log does not hold.
const (
	eraPreSandbox = "pre-sandbox"
	eraSandbox    = "sandbox"
	eraPod        = "pod"
	// eraUnknown is a landing whose landed_at names no instant, so no era can
	// hold it. It is counted under its own name rather than with a real era.
	eraUnknown = "unknown"
)

// Where a cut came from: the regime log's platform event, or the backfill
// constants.
const (
	eraCutFromLog       = "log"
	eraCutFromConstants = "constants"
)

// eraHoldoutMinLandings is the fewest landings the last era must hold before the
// holdout is that whole era; under it the holdout is the later 30% of the last
// era. The figure is the floor the bead's Constraints set (gt-t9b5e4). Nothing in
// the tree measures it, so it is a chosen floor, not a derived one.
const eraHoldoutMinLandings = 300

// eraOrder is the order the era counts are written in, earliest era first.
var eraOrder = []string{eraPreSandbox, eraSandbox, eraPod, eraUnknown}

// EraCuts are the instants the runtime eras begin at: the sandbox era at the
// sandbox move and the pod era at the pod move. From names where each cut came
// from, and Note says why the log did not answer when it could not be read.
type EraCuts struct {
	Sandbox, Pod         time.Time
	SandboxFrom, PodFrom string
	Note                 string
}

// defaultEraCuts are the cuts the backfill constants give, with no regime log.
func defaultEraCuts() EraCuts {
	return EraCuts{
		Sandbox:     regimeSandboxMoveAt,
		Pod:         regimePodMoveAt,
		SandboxFrom: eraCutFromConstants,
		PodFrom:     eraCutFromConstants,
	}
}

// readEraCuts reads the two moves from the town's regime log. A platform event
// cuts an era only when its reason is one of the two backfill moves' reasons; any
// other platform event (a resize, a failed cutover) is no era boundary. When the
// earliest such event is logged, its instant is the cut; otherwise the backfill
// constant is.
//
// The live log holds neither move until someone runs gt regime backfill, so
// until then both cuts come from the constants. A log that cannot be read also
// falls back to the constants, and Note says so, so the verdict and the panel
// name the fallback rather than hide it.
func readEraCuts(env Env) EraCuts {
	cuts := defaultEraCuts()
	records, err := readRegimeRecords(env)
	if err != nil {
		cuts.Note = "regime log unreadable, the backfill constants answer: " + err.Error()
		return cuts
	}
	earliest := make(map[string]time.Time)
	for _, rec := range records {
		if rec.Kind != "platform" {
			continue
		}
		reason := strings.TrimSpace(rec.Reason)
		if at, ok := earliest[reason]; !ok || rec.TS.Before(at) {
			earliest[reason] = rec.TS
		}
	}
	if at, ok := earliest[regimeSandboxMoveReason]; ok {
		cuts.Sandbox, cuts.SandboxFrom = at, eraCutFromLog
	}
	if at, ok := earliest[regimePodMoveReason]; ok {
		cuts.Pod, cuts.PodFrom = at, eraCutFromLog
	}
	return cuts
}

// describe writes the cuts as the verdict names them: each instant and the
// source that answered, then the reason the log did not answer, if it did not.
func (c EraCuts) describe() string {
	s := "sandbox " + c.Sandbox.UTC().Format(time.RFC3339) + " (" + c.SandboxFrom + "), " +
		"pod " + c.Pod.UTC().Format(time.RFC3339) + " (" + c.PodFrom + ")"
	if c.Note != "" {
		s += "; " + c.Note
	}
	return s
}

// runtimeEra is the era a landing at landedAt ran under. A landedAt that names
// no instant is eraUnknown, never a guess at its era.
func runtimeEra(cuts EraCuts, landedAt string) string {
	at, err := ParseTS(landedAt)
	if err != nil {
		return eraUnknown
	}
	return eraAt(cuts, at)
}

// eraAt is the era an instant falls in under cuts.
func eraAt(cuts EraCuts, at time.Time) string {
	switch {
	case at.Before(cuts.Sandbox):
		return eraPreSandbox
	case at.Before(cuts.Pod):
		return eraSandbox
	}
	return eraPod
}

// EraSplit is the landings cut by runtime era: train and holdout, the count of
// landings whose landed_at names no instant (which sit on neither side), and the
// cuts the split was made under.
type EraSplit struct {
	Train, Holdout []Row
	Unknown        int
	Cuts           EraCuts
}

// SplitByEra cuts rows into train and holdout so the holdout is always inside one
// runtime era, and a change of regime between the eras is not read as a failed
// pattern. Rows are ordered by their parsed landed_at, not its text, so two
// spellings of a zone order by the instant they name. A row whose landed_at names
// no instant is counted in Unknown and sits on neither side, so it is never the
// last landing.
//
// The last era is the era of the latest landing. When that era holds at least
// eraHoldoutMinLandings landings and earlier landings exist, train is every
// earlier landing and holdout is the whole last era. Otherwise train is every
// earlier landing plus the earliest defaultTrainFraction of the last era, and the
// holdout is the rest of that era, so the cut stays inside one era either way.
//
// Each row's era is read from its landed_at, not from the runtime_era cell, so a
// landings table written before that column existed splits the same way.
func SplitByEra(rows []Row, cuts EraCuts) EraSplit {
	split := EraSplit{Cuts: cuts}
	type dated struct {
		at  time.Time
		row Row
	}
	landed := make([]dated, 0, len(rows))
	for _, r := range rows {
		at, err := ParseTS(r["landed_at"])
		if err != nil {
			split.Unknown++
			continue
		}
		landed = append(landed, dated{at: at, row: r})
	}
	if len(landed) == 0 {
		return split
	}
	sort.SliceStable(landed, func(i, j int) bool { return landed[i].at.Before(landed[j].at) })
	ordered := make([]Row, len(landed))
	for i, d := range landed {
		ordered[i] = d.row
	}
	last := eraAt(cuts, landed[len(landed)-1].at)
	start := len(landed) - 1
	for start > 0 && eraAt(cuts, landed[start-1].at) == last {
		start--
	}
	earlier, lastEra := ordered[:start], ordered[start:]
	if len(lastEra) >= eraHoldoutMinLandings && len(earlier) > 0 {
		split.Train, split.Holdout = earlier, lastEra
		return split
	}
	inTrain, inHoldout := cutAt(lastEra, defaultTrainFraction)
	train := make([]Row, 0, len(earlier)+len(inTrain))
	split.Train = append(append(train, earlier...), inTrain...)
	split.Holdout = inHoldout
	return split
}

// panelEras is each side of the split's era counts, cut the way the analysis
// cut its split.
func panelEras(split EraSplit) PanelEras {
	return PanelEras{
		Train:   eraCounts(split.Train, split.Cuts),
		Holdout: eraCounts(split.Holdout, split.Cuts),
		Unknown: split.Unknown,
		Cuts:    split.Cuts.describe(),
	}
}

// eraCounts is how many of rows fall in each runtime era under cuts, read from
// each row's landed_at. An era with no row is absent from the map.
func eraCounts(rows []Row, cuts EraCuts) map[string]int {
	counts := make(map[string]int)
	for _, r := range rows {
		counts[runtimeEra(cuts, r["landed_at"])]++
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
