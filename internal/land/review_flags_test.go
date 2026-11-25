package land

import (
	"context"
	"slices"
	"testing"
)

// reviewFlagsInput is a review that passes the bead, the brief, the policy,
// the rounds ledger and the round, so the argv carries the flags a complete om
// review is run with.
func reviewFlagsInput(t *testing.T, round int) ReviewInput {
	t.Helper()
	return ReviewInput{Repo: baseRepo{},
		Dir: t.TempDir(), Base: "aaa", Head: "bbb", BeadID: "gt-abc",
		BriefText: "# Work item\n", PolicyText: "policy", RoundsText: "[]", Round: round,
	}
}

// argvFlagNames is the flag words of argv in the order argv gives them.
func argvFlagNames(argv []string) []string {
	var flags []string
	for _, a := range argv {
		if len(a) > 0 && a[0] == '-' {
			flags = append(flags, a)
		}
	}
	return flags
}

// TestVerdictReuseFlagsNameTheArgv runs a review through the fake om and checks
// that the verdict names the flags of its argv, sorted. The argv's own order is
// checked first so the row cannot pass on an argv that already sorts.
func TestVerdictReuseFlagsNameTheArgv(t *testing.T) {
	t.Parallel()
	stub := &contextOM{t: t, help: omHelpWithRoundFlags}
	r := OMReviewer{OutDir: t.TempDir(), run: stub.run}
	got, err := r.ReviewBead(context.Background(), reviewFlagsInput(t, 2))
	if err != nil {
		t.Fatalf("ReviewBead: %v", err)
	}
	natural := argvFlagNames(stub.argv)
	if slices.IsSorted(natural) {
		t.Fatalf("argv flags %v are already sorted; the row does not test the sort", natural)
	}
	want := slices.Clone(natural)
	slices.Sort(want)
	if !slices.Equal(got.Flags, want) {
		t.Fatalf("Flags = %v, want the argv flags sorted %v", got.Flags, want)
	}
}

// TestVerdictReuseDroppedFlagIsVisible runs a review against an om whose help
// does not list --round. The landing drops the flag with a warning, and the
// verdict must show that it was dropped.
func TestVerdictReuseDroppedFlagIsVisible(t *testing.T) {
	t.Parallel()
	stub := &contextOM{t: t, help: omHelpWithBead}
	r := OMReviewer{OutDir: t.TempDir(), run: stub.run}
	got, err := r.ReviewBead(context.Background(), reviewFlagsInput(t, 2))
	if err != nil {
		t.Fatalf("ReviewBead: %v", err)
	}
	if slices.Contains(stub.argv, "--round") {
		t.Fatalf("argv carries --round, which this om's help does not list: %v", stub.argv)
	}
	if slices.Contains(got.Flags, "--round") {
		t.Fatalf("Flags = %v names --round, which the argv does not carry", got.Flags)
	}
	want := argvFlagNames(stub.argv)
	slices.Sort(want)
	if !slices.Equal(got.Flags, want) {
		t.Fatalf("Flags = %v, want the argv flags sorted %v", got.Flags, want)
	}
}
