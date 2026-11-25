package land

import "slices"

// ExpectedReviewFlags is the sorted flag names a complete om review run carries
// for in and bar, as flagNames gives them for reviewOnce's argv. This part names
// the flags every run carries: -C, --base, --head and --out, and --bead when the
// review names its work bead. The flags that depend on the input or on bar
// follow in the next change.
func ExpectedReviewFlags(in ReviewInput, bar float64) []string {
	flags := []string{"-C", "--base", "--head", "--out"}
	if in.BeadID != "" {
		flags = append(flags, "--bead")
	}
	slices.Sort(flags)
	return flags
}
