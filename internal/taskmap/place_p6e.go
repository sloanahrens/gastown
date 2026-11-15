package taskmap

// Placements for batch P6 piece E (gt-m6ramj): the molecule commands that
// show where a molecule has got to and what comes next. Group judgement:
// current, progress and dag sit in orient because each reads a molecule's
// state without changing it, and an agent reaches for them to find its place;
// detach sits in dispatch beside attach and attachment, because it clears the
// same attachment those commands set and read.
func init() {
	Register(
		Entry{Path: "mol current", Group: "orient", Use: "Use this to see what an agent should be working on next, whether that is your own hook or a named agent's, when you have lost track of the current step.", Check: "the output prints Identity, Handoff, Molecule and Status lines, and a Current line names the step to work on when one is ready"},
		Entry{Path: "mol dag", Group: "orient", Use: "Use this when you need the order of a molecule's steps and which of them can run in parallel, before you pick the next one to work on.", Check: "the output names the root and node count, and each step line carries one mark: ✓ done, ⧖ in_progress, ○ ready or ◌ blocked"},
		Entry{Path: "mol detach", Group: "dispatch", Use: "Use this when a pinned bead should stop carrying its molecule, to clear the attachment from its hook while the molecule's steps stay as they are.", Check: "the run prints Detached with the molecule and bead IDs, or says no molecule was attached"},
		Entry{Path: "mol progress", Group: "orient", Use: "Use this when you need to know how far a molecule has got, which steps are done, running, ready or blocked, and whether it is finished.", Check: "the output prints the Done, In Progress and Ready counts and a percentage bar"},
	)
}
