package taskmap

// Placements for batch P6 piece F (gt-rj0k05): the molecule commands that
// show what is on a hook, close a molecule out as a digest, and walk its
// steps. Group judgement: status sits in orient beside current and progress,
// because it reads the hook and says what to do next; squash sits in dispatch
// beside burn, because both clear the molecule from the hook and differ only
// in whether a digest is kept; step is a parent that only lists the step
// workflow, so it sits in orient as a pointer to where a molecule goes next;
// await-event sits in internal beside await-signal, because it is the patrol
// loop's wait primitive rather than a task an agent picks from the map.
func init() {
	Register(
		Entry{Path: "mol status", Group: "orient", Use: "Use this when you need to see what is on your hook, or a named agent's hook, and what the next action is, before you start or resume work.", Check: "the output prints the Hooked line with the bead ID and title, or says Nothing on hook - no work slung, and a Next line names the action"},
		Entry{Path: "mol squash", Group: "dispatch", Use: "Use this when a molecule has finished and its run should stay on record as a short digest, such as a patrol cycle; use burn when no record is wanted.", Check: "the run prints Squashed molecule with its ID, and gt hook shows nothing attached"},
		Entry{Path: "mol step", Group: "orient", Use: "Use this when you need to know how a molecule moves on from one step to the next, before you complete the step in front of you.", Check: "the output lists the step subcommands, and gt mol progress shows the step count moving after gt mol step done"},
		Entry{Path: "mol step await-event", Group: "internal", Use: "Use this when a patrol loop has nothing to do until a file-based event arrives on a named channel, so the agent sleeps instead of polling.", Check: "the run prints the event count received with the event paths, or Timeout after the wait with the idle cycle count"},
	)
}
