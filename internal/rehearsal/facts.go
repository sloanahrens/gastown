package rehearsal

import (
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// gateRunPrefix opens the comment a failed or unrunnable local gate leaves on
// the bead (gt-13dre6.17): "GATE RUN: " then the verdict and the wall.
const gateRunPrefix = "GATE RUN: "

// GateFacts reads the gate figures recorded on one bead: the last READY TO LAND
// block's Gate line, then one gate per "GATE RUN: " comment in comments order.
// A Gate line that is not a pass with a wall in its third field, an empty one
// included, gives an unavailable gate of zero seconds. So does a Gate line in a
// block land.ParseReadyNote rejects, and a comment whose verdict is neither
// fail nor unavailable. A fail or unavailable comment without a readable wall
// keeps its verdict and zero seconds. A note with no Gate line and a comment
// without the prefix record nothing.
func GateFacts(notes string, comments []string) []Gate {
	var gates []Gate
	w, ok := land.ParseReadyNote(notes)
	switch {
	case ok && w.Gate != "":
		gates = append(gates, readyGate(w.Gate))
	case gateLinePresent(notes):
		gates = append(gates, Gate{Verdict: GateUnavailable})
	}
	for _, c := range comments {
		if rest, ok := strings.CutPrefix(c, gateRunPrefix); ok {
			gates = append(gates, runCommentGate(rest))
		}
	}
	return gates
}

// LandingFacts reads a rehearsal bead's landing: Landed when the notes hold a
// LANDING RECORD block naming a landed commit, and CIFailed when they hold a
// MERGE REJECTION block of the gate kind, even when a later landing followed it.
func LandingFacts(bead, notes string) Landing {
	landing := Landing{Bead: bead}
	if _, ok := land.ParseLandingRecord(notes); ok {
		landing.Landed = true
	}
	if len(land.RejectionsOfKind(notes, string(land.RejectGate))) > 0 {
		landing.CIFailed = true
	}
	return landing
}

// gateLinePresent reports whether the last READY TO LAND block carries a Gate
// line, even a valueless one: ParseReadyNote returns the value, not its presence.
func gateLinePresent(notes string) bool {
	idx := strings.LastIndex(notes, land.ReadyNoteMarker+"\n")
	if idx < 0 {
		return false
	}
	for _, line := range strings.Split(notes[idx+len(land.ReadyNoteMarker)+1:], "\n") {
		key, _, ok := strings.Cut(line, ":")
		if !ok {
			break
		}
		if strings.EqualFold(strings.TrimSpace(key), "gate") {
			return true
		}
	}
	return false
}

// readyGate reads a READY TO LAND Gate line, the verdict, step and wall that
// localGateNote renders: "pass presubmit 3m5s, slot wait 0s". Only a pass with
// a duration in its third field is a passing gate; every other line is
// unavailable, so none is dropped or read as a verdict it did not give.
func readyGate(line string) Gate {
	fields := strings.Fields(line)
	if len(fields) >= 3 && fields[0] == GatePass {
		if secs, ok := wallSeconds(fields[2]); ok {
			return Gate{Verdict: GatePass, Seconds: secs}
		}
	}
	return Gate{Verdict: GateUnavailable}
}

// runCommentGate reads the text after "GATE RUN: ", the verdict then the wall
// recordGateRun renders: "fail 2m3s, slot wait 0s, head abc1234". A verdict
// that is neither fail nor unavailable becomes unavailable; a missing or
// unreadable wall is zero seconds.
func runCommentGate(rest string) Gate {
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return Gate{Verdict: GateUnavailable}
	}
	if fields[0] != GateFail && fields[0] != GateUnavailable {
		return Gate{Verdict: GateUnavailable}
	}
	g := Gate{Verdict: fields[0]}
	if len(fields) >= 2 {
		if secs, ok := wallSeconds(fields[1]); ok {
			g.Seconds = secs
		}
	}
	return g
}

// wallSeconds reads a gate wall in seconds. The writers end the wall field with
// the comma of "3m5s, slot wait", so a trailing comma is trimmed first; a field
// that is not a duration, or is negative, is not readable.
func wallSeconds(field string) (float64, bool) {
	d, err := time.ParseDuration(strings.TrimSuffix(field, ","))
	if err != nil || d < 0 {
		return 0, false
	}
	return d.Seconds(), true
}
