package taskmap

// Placement for gt review-preview, the polecat's local om round on its branch
// head before gt done submits it (gt-jx66mp). It is its own file, so the batch
// files keep their own commands.
func init() {
	Register(
		Entry{
			Path:  "review-preview",
			Group: "review",
			Use:   "Use this to see what the local review round finds on your branch head before you submit it, so you can answer each finding.",
			Check: "bd show <id>",
		},
	)
}
