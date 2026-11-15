package taskmap

// Placements for batch P12, half B part 1 (gt-j669b4.23): the five gt plugin
// commands that read, run and record a plugin by hand. The sibling piece
// (gt-ijo6p4) places plugin sync, which copies plugin files between the source
// checkout and the runtime directories.
//
// Group judgement: all five sit in dispatch. A plugin is periodic work the
// daemon heartbeat dispatches on a gate, the dispatcher beside the scheduler,
// and these commands read that work and hand one plugin's instructions to the
// seat that does it. gt plugin run is the closest call: it executes nothing, so
// its entry says the work becomes yours, and it refuses a script-type plugin
// outright because only the daemon's cooldown gate ever runs a run.sh. internal
// was rejected because a person starts every one of these by hand; review was
// rejected because history reports the work already dispatched, not a check of
// work before or after landing.
func init() {
	Register(
		Entry{Path: "plugin history", Group: "dispatch", Use: "Use this when you need to know whether a plugin has been running and how its last runs ended, before you trust what it reports or chase a report it never sent."},
		Entry{Path: "plugin list", Group: "dispatch", Use: "Use this to see which plugins the town and each rig define, and whether a rig has its own copy of one, before you run or edit a plugin."},
		Entry{Path: "plugin record-run", Group: "dispatch", Use: "Use this when the work a plugin asked for is done, failed or skipped, so its history carries the real result instead of the receipt for printing the instructions.", Check: "the command prints the new bead id and gt plugin history <plugin> shows the run with that result's glyph"},
		Entry{Path: "plugin run", Group: "dispatch", Use: "Use this when a plugin's work is due but nothing will dispatch it, so you take its instructions and do the work yourself.", Check: "gt plugin history <name> shows the new run with the printed marker, not the success one"},
		Entry{Path: "plugin show", Group: "dispatch", Use: "Use this when you are about to run a plugin and need its gate and the start of its instructions, or when you need to know why the daemon never runs it."},
	)
}
