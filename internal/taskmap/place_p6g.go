package taskmap

// Placements for batch P6 piece G (gt-2c3q4r): the molecule step commands that
// wait on the activity feed, close a finished step and move the molecule on,
// and leave an event on a channel, and the promote command that ships a green
// commit to a rig's GitHub target. Group judgement: await-signal sits in
// internal beside its sibling gt mol await-signal, because it is the patrol
// loop's wait primitive rather than a task an agent picks from the map; step
// done sits in orient, because finishing a step is how a polecat finds what
// comes next; emit-event sits in dispatch, because an agent runs it by hand to
// hand a signal to another agent, so the internal rule does not fit; promote
// sits in roll, which covers moving a landing on to its next target.
func init() {
	Register(
		Entry{Path: "mol step await-signal", Group: "internal", Use: "Use this when a patrol loop is idle and should sleep until activity for its rig reaches the events feed, instead of polling.", Check: "the run prints Signal received after the wait, or Timeout after the wait with the idle cycle number when an agent bead was given"},
		Entry{Path: "mol step done", Group: "orient", Use: "Use this when the step you are working on is finished, to close it and move the molecule on to the next ready step.", Check: "the run prints Next step assigned with the next step ID, or says the molecule is complete when no step is left, and gt mol progress shows one more step closed"},
		Entry{Path: "mol step emit-event", Group: "dispatch", Use: "Use this when another agent or a channel's consumer, such as the refinery, must learn that something just happened, by leaving an event file it picks up on its next await-event.", Check: "the run prints the path of the event file it wrote under ~/gt/events/<channel>/, and a per-rig channel with no rig context is an error naming --rig"},
		Entry{Path: "promote", Group: "roll", Use: "Use this when a commit on a rig's Forgejo main is green and the rig's GitHub promote target must advance to it.", Check: "the printed line is rig <rig>: GitHub main holds <short-sha> and the exit code is 0; exit 1 means the commit is not promotable and exit 2 means the promotion failed"},
	)
}
