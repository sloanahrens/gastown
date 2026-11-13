package taskmap

// Placement for gt review-preview reply, which answers one finding of the last
// local om round on the work bead (gt-16x50l). It is its own file, as the
// parent's placement is, so the batch files keep their own commands.
func init() {
	Register(
		Entry{
			Path:  "review-preview reply",
			Group: "review",
			Use:   "Use this to answer one finding of the last local review round on your branch, as fixed once the branch has changed or as disputed with a reason, so the answer is on the work bead.",
			Check: "bd show <id> lists a LOCAL ROUND REPLY line for the finding",
		},
	)
}
