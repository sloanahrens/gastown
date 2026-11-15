package taskmap

// Placements for batch P6 piece C (gt-8pefhr): reading, running and syncing
// formulas, the overlay that shows one formula's changes, and the mol
// namespace. Group judgement: formula show, run and sync sit in dispatch with
// the rest of the formula namespace, because a formula defines the work a town
// hands its agents; formula sync changes those on-disk formulas, so it is the
// least obvious of the three. mol sits in orient, not dispatch, because its
// help opens with the commands that show an agent its own hook and progress;
// the attach and step subcommands are placed in their own batch pieces.
func init() {
	Register(
		Entry{Path: "formula show", Group: "dispatch", Use: "Use this to read a formula's variables and steps before you run it or write an overlay for it, as gt prime will render them.", Check: "the output lists the formula's steps with their dependencies"},
		Entry{Path: "formula run", Group: "dispatch", Use: "Use this when a workflow formula should start now, to pour it into a molecule and dispatch its steps to the rig's available workers."},
		Entry{Path: "formula sync", Group: "dispatch", Use: "Use this after gt has been rebuilt with a formula fix, to bring the town's on-disk formulas up to the ones this binary embeds.", Check: "the report names the commit the synced formulas came from"},
		Entry{Path: "formula overlay show", Group: "dispatch", Use: "Use this to see the step changes this town has made to one formula, before you edit its overlay or when a formula step reads differently from the source.", Check: "the output's header names the overlay file under Source"},
		Entry{Path: "mol", Group: "orient", Use: "Use this to find the molecule command for your own hook, whether you need to see what you should work on now, how far along it is, or how to attach, step through or end it."},
	)
}
