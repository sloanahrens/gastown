package taskmap

// Placements for batch P6 piece B (gt-1y0yf5): the formula create, list and
// overlay commands, all five in dispatch because each one shapes the work
// instructions a town hands its agents (writing a formula, finding one,
// customizing a step at prime time). Overlay list and edit read files a human
// may look for, but what they change is what an agent is told to do.
func init() {
	Register(
		Entry{Path: "formula create", Group: "dispatch", Use: "Use this when the town has no formula for work you will repeat, to start a new formula file from a template.", Check: "the run prints Created formula with the new file's path"},
		Entry{Path: "formula list", Group: "dispatch", Use: "Use this to see which formulas this town can run before you pick one to show, run or sling.", Check: "the printed list names the town's formulas"},
		Entry{Path: "formula overlay", Group: "dispatch", Use: "Use this to change one step of a formula without editing the formula that gastown source ships to every town."},
		Entry{Path: "formula overlay edit", Group: "dispatch", Use: "Use this when a formula step needs different instructions in this town, to open that formula's overlay in your editor.", Check: "the run prints Overlay updated and the next `gt prime` shows the changed step"},
		Entry{Path: "formula overlay list", Group: "dispatch", Use: "Use this to see which formulas this town has customized with an overlay.", Check: "the printed table names each formula's overlay file with its path"},
	)
}
