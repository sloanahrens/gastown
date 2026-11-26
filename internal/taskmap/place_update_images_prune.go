package taskmap

// The placement for gt update images prune, the registry retention plan. It
// sits in roll beside gt update images, whose recorded pairs it keeps. Without
// --apply it prints the plan and changes nothing; with --apply it deletes the
// tags the plan names, at most --max of them.
func init() {
	Register(
		Entry{Path: "update images prune", Group: "roll", Use: "Use this when the registry's gastown image tags need pruning: without --apply it prints which tags the retention plan keeps and which it would delete and changes nothing, and with --apply it deletes the tags the plan names, at most --max of them.", Check: "the plan run prints one keep or delete line per tag with its reason, then each image's counts; the --apply run prints one deleted line per tag and then the deleted and skipped counts; a read, planner, credential or delete failure exits non-zero"},
	)
}
