package attention

// riskPathLoudPriority is the bd priority at and above which a failed review of
// a risk-path landing is an incident rather than review debt: P1, the lane the
// town lands its own machinery in. P0 sits above it, so a P0 landing is loud
// too.
const riskPathLoudPriority = 1

// RiskPathEvidence is what a risk-path item's landing says about it: the
// verdict of the review on its head and the landing work bead's own priority.
type RiskPathEvidence struct {
	// Failed is whether the latest review naming the landing's head found a
	// defect. An item that reaches the queue is failed or unreviewed; a PASS,
	// WAIVED or AUDITED review answers the head and clears it.
	Failed bool
	// Priority is the landing work bead's bd priority, 0 the loudest.
	Priority int
}

// RiskPathSeverity returns the severity of a risk-path item, from the evidence
// its landing records rather than from its kind alone: a failed review of a P1
// landing is high, and every other risk-path item is review debt, which is low
// (gt-zevjx).
//
// A risk-path item is the town's own review queue — a landing that touched a
// path on the risk list, whose post-landing review is still owed — and the
// queue holds dozens of them at once. The item is not an incident, and drawn as
// loud as one it buries the escalation, the red main and the daemon-down item
// beside it. What earns the loudness is a defect a review found in the
// machinery the town runs on (P1: the daemon, the guards, git, dolt), where it
// can stop everything else.
func RiskPathSeverity(ev RiskPathEvidence) Severity {
	if ev.Failed && ev.Priority <= riskPathLoudPriority {
		return SeverityHigh
	}
	return SeverityLow
}
