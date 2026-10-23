package specdispatch

// The skip-reason prefixes of a bead the dispatcher cannot start until a
// person or a seat of its class changes. The code that writes a skip reason
// and the daemon's dispatch.idle check that matches it both name these, so a
// reworded reason cannot silently turn a standoff into a "waiting" count.
const (
	SkipUnshaped                 = "unshaped"
	SkipNotEligible              = "not eligible"
	SkipUnknownFormula           = "unknown formula"
	SkipBadFormulaVars           = "bad formula vars"
	SkipConflictingFormulaLabels = "conflicting formula labels"
	SkipBadFormulaLabel          = "bad formula label"
	// SkipHeld starts a hold reason; SkipContainer starts the one OpenChildHold
	// writes after it.
	SkipHeld      = "held: "
	SkipContainer = "container:"
	SkipNoSeat    = "no seat:"
	SkipRigHold   = "rig hold:"
	// SkipPerTickLimit is a clean bead left for a later tick by the per-tick
	// limit; the limit's number follows it.
	SkipPerTickLimit = "clean, per-tick limit"
	// SkipStartupBackoff starts the reason a bead stays unslung after its last
	// sling failed at session start: the dispatcher is holding it, not a
	// person, so the dispatch.idle check counts it as waiting work and it is
	// deliberately not in that check's standoff list.
	SkipStartupBackoff = "startup backoff:"
)
