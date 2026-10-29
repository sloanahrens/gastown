package specdispatch

import (
	"fmt"
	"strings"
)

// The overrun-stop flag (gt-b9j3o.4): the shape of bead the overrun guard
// stopped most often on one rig, written on a matching bead as a note. The box
// is that rig's finding, not a rule for the town, so a match changes nothing
// about the dispatch — an operator deciding whether to watch a run reads it.
//
// The box is the automatic dispatcher's, so it takes the route as given rather
// than as a fact: a field the caller always fills the same way is not a check.

// OverrunNoteKey starts the note a flagged bead carries. Annotate dedupes on
// it, so a bead slung by later ticks still wears one flag.
const OverrunNoteKey = "OVERRUN RISK:"

const (
	// overrunModelFamily is the seat model family the box was measured on: the
	// smallest seat, the one the guard stopped most often.
	overrunModelFamily = "haiku"
	// overrunMinThreshold and overrunMaxThreshold bound the rig review
	// threshold the box was measured at, inclusive. A rig outside the band —
	// including one that sets no threshold, which reads as 0 — is outside the
	// box: the finding does not carry to a rig whose reviewer scores on a
	// different scale.
	overrunMinThreshold = 0.60
	overrunMaxThreshold = 0.75
)

// OverrunFacts are the facts the overrun-stop box reads, one per field of the
// box. The box itself is InOverrunBox.
type OverrunFacts struct {
	// Type and Priority are the bead's own, as the ready board reads them. A
	// priority below 0 is an unscored bead and never in the box, whatever its
	// number sorts as.
	Type     string
	Priority int
	// Shaped is the shape lint's answer for the bead (LintVerdict.Clean), the same
	// predicate the dispatcher gates a candidate on.
	Shaped bool
	// Labels is the bead's label count: the box is one label, the operator's or
	// the dispatcher's own mark, and no second one.
	Labels int
	// Model is the seat agent's model family (models.ModelFamily), read from
	// the agent the tick chose.
	Model string
	// Threshold reads the rig's review threshold (.om.json). It is a reader
	// rather than a value because it is the one fact of the box that costs a
	// read: the box asks it only after every other fact matched, so a tick
	// reads it for the few candidates the box could hold rather than for all of
	// them.
	Threshold func() (float64, error)
}

// OverrunVerdict is the box's answer to one bead's facts.
type OverrunVerdict struct {
	// Match is whether the facts are in the box.
	Match bool
	// Threshold is what the reader answered, 0 when the box left the fact
	// unasked. The note a match writes names it.
	Threshold float64
}

// InOverrunBox reports whether facts are in the overrun-stop box: a task or
// bug, priority P0 to P2, shaped, exactly one label, a seat on
// overrunModelFamily, and a rig review threshold inside the band. Every fact
// but the threshold is checked before the reader is asked. A threshold that
// cannot be read is an error, not a fact: the box does not guess, and the
// caller reports the read as it would any other failed read.
func InOverrunBox(f OverrunFacts) (OverrunVerdict, error) {
	typ := strings.ToLower(strings.TrimSpace(f.Type))
	if typ != "task" && typ != "bug" {
		return OverrunVerdict{}, nil
	}
	if f.Priority < 0 || f.Priority > 2 {
		return OverrunVerdict{}, nil
	}
	if !f.Shaped || f.Labels != 1 {
		return OverrunVerdict{}, nil
	}
	if !strings.EqualFold(strings.TrimSpace(f.Model), overrunModelFamily) {
		return OverrunVerdict{}, nil
	}
	if f.Threshold == nil {
		return OverrunVerdict{}, fmt.Errorf("the overrun box has no threshold read")
	}
	threshold, err := f.Threshold()
	if err != nil {
		return OverrunVerdict{}, err
	}
	if threshold < overrunMinThreshold || threshold > overrunMaxThreshold {
		return OverrunVerdict{Threshold: threshold}, nil
	}
	return OverrunVerdict{Match: true, Threshold: threshold}, nil
}

// OverrunNote is the line a flagged bead carries, and the tick's report repeats
// for its sling: the two facts of the box the bead itself does not show.
func OverrunNote(model string, threshold float64) string {
	return fmt.Sprintf("%s this bead is in the overrun-stop box (gt-b9j3o.4: rig threshold %.2f, %s seat). It dispatches as before; the flag is a note, not a hold.",
		OverrunNoteKey, threshold, model)
}
