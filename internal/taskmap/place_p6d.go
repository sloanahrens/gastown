package taskmap

// Placements for batch P6 piece D (gt-gdmv5k): the commands that put a
// molecule on an agent's hook, take it off again, and wait for the next event
// in a patrol loop. Group judgement: attach, attach-from-mail, attachment and
// burn sit in dispatch beside the hook commands, because they read and change
// the same attachment the hook family shows and clears; await-signal sits there
// too, not in internal, because an agent runs it by hand inside a patrol loop
// rather than gt invoking it as plumbing (gt-f9qwcc).
func init() {
	Register(
		Entry{Path: "mol attach", Group: "dispatch", Use: "Use this when a molecule needs to sit on an agent's hook, naming the pinned bead to attach it to another agent or giving only the molecule ID to attach it to your own.", Check: "the run prints Attached with the molecule and the time it was attached"},
		Entry{Path: "mol attach-from-mail", Group: "dispatch", Use: "Use this when work reached you as a mail message whose body carries attached_molecule, to put that molecule on your own hook without copying its ID.", Check: "the run prints the mail, hook and molecule IDs and the mail is marked read"},
		Entry{Path: "mol attachment", Group: "dispatch", Use: "Use this to see which molecule a pinned bead's hook already holds, before you attach another or take this one off.", Check: "the output names the attached molecule's ID and when it was attached"},
		Entry{Path: "mol await-signal", Group: "dispatch", Use: "Use this inside a patrol loop to wait until activity for your rig reaches the events feed, so the agent sleeps instead of polling.", Check: "the run prints Signal received after the wait, or Timeout after the wait with the idle cycle number when an agent bead was given, or with (no activity) when none was"},
		Entry{Path: "mol burn", Group: "dispatch", Use: "Use this when you are abandoning a molecule and no audit record is wanted, to clear it from the hook without leaving a digest; use squash when the run should stay auditable.", Check: "the run prints Burned molecule with its ID and the agent it came from, or nothing to burn when no molecule is attached"},
	)
}
