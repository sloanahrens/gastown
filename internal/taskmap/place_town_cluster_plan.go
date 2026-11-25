package taskmap

// Placement for gt town cluster plan, which renders the desired set from the
// recorded flags and prints its plan id, so a shape change is reviewable before
// it is applied.
func init() {
	Register(
		Entry{Path: "town cluster plan", Group: "host", Use: "Use this when a control-cluster shape change must be reviewed before it is applied, to render the desired set into a directory and print the plan id.", Check: "prints the 12-hex plan id after rendering the files into --out"},
	)
}
