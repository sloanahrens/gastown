package taskmap

// The placement for gt update images prune, the read-only registry retention
// plan. It sits in roll beside gt update images, whose recorded pairs it keeps.
func init() {
	Register(
		Entry{Path: "update images prune", Group: "roll", Use: "Use this when the registry's gastown image tags need pruning and you must see which tags the retention plan would keep and which it would delete, before anything is deleted; it deletes nothing.", Check: "the run prints one keep or delete line per tag with its reason, then each image's counts; exit 0 is a plan, and a read or planner failure exits non-zero with no line printed"},
	)
}
