package taskmap

// Placements for batch P5, part 4 (gt-3jvzja): gt polecat pool-init through
// gt polecat status. Pool-init fills a rig with idle polecats, stale finds the
// ones that are candidates for cleanup, remove gives one up, status shows one
// polecat's state, and prune deletes merged polecat branches. All five sit
// beside the other polecat seat commands.
//
// Group judgement: gt polecat prune deletes branches from the local clone, and
// from origin with --remote, so it is host, the machine-level cleanup group. It
// is not land, because it does not move finished work onto main. gt polecat
// stale is seats, not shutdown, because its default run only reports; the
// destructive step is the --cleanup flag, which is named in its Use line.
func init() {
	Register(
		Entry{Path: "polecat pool-init", Group: "seats", Use: "Use this when a rig needs a standing pool of idle polecats for gt sling to hand work to, and the pool is short of its target size.", Check: "the run ends with Pool initialized: <n> created, <total> total (target: <size>)"},
		Entry{Path: "polecat prune", Group: "host", Use: "Use this when merged polecat branches have piled up in a rig, locally or on origin, and only the branches still in use should stay.", Check: "gt polecat prune <rig> --dry-run lists each branch it would delete with its reason"},
		Entry{Path: "polecat remove", Group: "seats", Use: "Use this when a finished polecat should leave its rig, once its session is stopped and its worktree holds no uncommitted work that would be lost.", Check: "the run ends with Removed <n> polecat(s)."},
		Entry{Path: "polecat stale", Group: "seats", Use: "Use this to find the polecats in a rig that are candidates for cleanup, before you run --cleanup and nuke any of them.", Check: "the run ends with Summary: <n> stale, <m> active"},
		Entry{Path: "polecat status", Group: "seats", Use: "Use this to see one polecat's lifecycle state, the issue it holds and whether its session is running, before you stop, remove or nuke it.", Check: "the run prints Polecat: <rig>/<name>, then its State and Session status"},
	)
}
