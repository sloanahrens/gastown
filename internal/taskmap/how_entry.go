package taskmap

// Placement for gt how, the orientation command a fresh overseer runs first:
// it lists the task groups, or one group's commands (gt-j669b4.6). It is its
// own file, so the batch files keep their own commands.
func init() {
	Register(
		Entry{
			Path:  "how",
			Group: "orient",
			Use:   "Use this to find the task group a job belongs to, or the commands in one group, before you go looking through the command tree.",
			Check: "gt how <group> lists that group's commands",
		},
	)
}
