package land

import "slices"

// ExpectedReviewFlags is the sorted flag names a complete om review run carries
// for in and bar, as flagNames gives them for reviewOnce's argv: -C, --base,
// --head and --out on every run, --bead when the review names its work bead,
// --threshold when bar > 0, --round when Round > 0, and --prior-findings when
// the input has the previous round's findings. The other conditional flags
// arrive with the next beads. Whether the installed om takes a flag is a
// run-time question its help probe answers, so a run whose probe refuses
// --threshold or --round carries fewer flags, and Verdict.Flags records the set
// it ran with. Any mismatch against the list a stored review ran with is a
// reuse miss, not a near match: the key's InputDigest covers these flags
// (gt-0lys4u), and ReuseMiss compares every key field for equality, so a flag
// over-claimed here costs a review and never reuses a stale verdict.
func ExpectedReviewFlags(in ReviewInput, bar float64) []string {
	flags := []string{"-C", "--base", "--head", "--out"}
	if in.BeadID != "" {
		flags = append(flags, "--bead")
	}
	if bar > 0 {
		flags = append(flags, "--threshold")
	}
	if in.Round > 0 {
		flags = append(flags, "--round")
	}
	if len(in.PriorFindings) > 0 {
		flags = append(flags, "--prior-findings")
	}
	slices.Sort(flags)
	return flags
}
