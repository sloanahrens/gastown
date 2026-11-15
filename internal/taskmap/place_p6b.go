package taskmap

// Placements for batch P6 piece B (gt-1y0yf5): the formula namespace's
// remaining commands. Group judgement: every one of them shapes the work
// instructions a town hands its agents (writing a formula, finding one,
// customising a step at prime time), so all five sit in dispatch with the
// `formula` parent piece A placed; if overlays were only ever read by a human
// looking for them, list and edit could lean toward orient, but they change
// what an agent is told to do.
func init() {
	Register(
		Entry{Path: "formula create", Group: "dispatch", Use: "Use this when the town has no formula for work you will repeat, to start a new formula file from a template.", Check: "the run prints Created formula with the new file's path"},
		Entry{Path: "formula list", Group: "dispatch", Use: "Use this to see which formulas this town can run before you pick one to show, run or sling.", Check: "the printed list names the town's formulas"},
		Entry{Path: "formula overlay", Group: "dispatch", Use: "Use this to change one step of a formula without editing the formula that gastown source ships to every town."},
		Entry{Path: "formula overlay edit", Group: "dispatch", Use: "Use this when a formula step needs different instructions in this town, to open that formula's overlay in your editor.", Check: "the run prints Overlay updated and the next `gt prime` shows the changed step"},
		Entry{Path: "formula overlay list", Group: "dispatch", Use: "Use this to see which formulas this town has customised with an overlay.", Check: "the printed table names each formula's overlay file with its path"},
	)
}
