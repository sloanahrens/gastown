package done

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestLineLimitVerdict(t *testing.T) {
	t.Parallel()
	refusal := []string{
		"Change exceeds the 70-added-line constraint: 141 added lines against 70 declared (the refusal is at twice the limit).",
		splitLine,
	}
	tests := []struct {
		name   string
		limits []int
		added  int
		refuse bool
		want   []string
	}{
		{"no limit", nil, 500, false, []string{noLimitLine}},
		{"twice the limit passes", []int{70}, 140, false, []string{
			"Change exceeds the 70-added-line constraint: 140 added lines against 70 declared (the refusal is at twice the limit)."}},
		{"past twice refuses", []int{70}, 141, true, refusal},
		{"two numbers read at the larger", []int{45, 60}, 121, true, []string{
			"Change exceeds the 60-added-line constraint: 121 added lines against 60 declared (the bead states 45 and 60; the refusal is at twice the limit).", splitLine}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if refuse, got := lineLimitVerdict(tc.limits, tc.added); refuse != tc.refuse || !reflect.DeepEqual(got, tc.want) {
				t.Errorf("lineLimitVerdict(%v, %d): refuse %v, lines %q; want refuse %v, lines %q",
					tc.limits, tc.added, refuse, got, tc.refuse, tc.want)
			}
		})
	}
}

// TestSplitLineTellsTheSeatToPushFirst: the refusal past twice the limit sends
// the seat to push, check ls-remote and comment both shas before the split.
func TestSplitLineTellsTheSeatToPushFirst(t *testing.T) {
	t.Parallel()
	err := reportLineLimit(new(bytes.Buffer), new(bytes.Buffer), func() (int, error) { return 141, nil }, "Change: at most 70 added lines in 2 files.", true)
	if err == nil {
		t.Fatal("reportLineLimit accepted 141 added lines against a declared limit of 70")
	}
	_, second, _ := strings.Cut(err.Error(), "\n")
	push, lsRemote := strings.Index(second, "git push origin HEAD:refs/heads/"), strings.Index(second, "git ls-remote origin")
	split := strings.Index(second, "then comment that the bead needs a split")
	if push < 0 || lsRemote < 0 || split < 0 || push > lsRemote || lsRemote > split ||
		!strings.Contains(second, "never --force") || !strings.Contains(second, "check the tip is your HEAD") ||
		!strings.Contains(second, "comment both shas") {
		t.Fatalf("the split line does not push, check ls-remote, comment both shas, then ask for a split: %s", second)
	}
}

// TestLineLimitReportRefusesPolecatOnly: a polecat past twice the declared
// limit is refused on out, a crew branch gets prefixed warnings on errOut and
// no error, and no declared limit means the count is never read (gt-zqg4m2).
func TestLineLimitReportRefusesPolecatOnly(t *testing.T) {
	t.Parallel()
	const desc = "Change: at most 70 added lines in 2 files."
	refusal := "Change exceeds the 70-added-line constraint: 141 added lines against 70 declared"
	var polecatOut, polecatErrOut, crewOut, crewErrOut, noLimitOut bytes.Buffer
	countCalled := false
	polecatErr := reportLineLimit(&polecatOut, &polecatErrOut, func() (int, error) { return 141, nil }, desc, true)
	crewErr := reportLineLimit(&crewOut, &crewErrOut, func() (int, error) { return 141, nil }, desc, false)
	noLimitErr := reportLineLimit(&noLimitOut, new(bytes.Buffer), func() (int, error) { countCalled = true; return 0, nil }, "Change: at most 2 files.", false)
	if polecatErr == nil || !strings.Contains(polecatErr.Error(), refusal) || !strings.Contains(polecatErr.Error(), splitLine) {
		t.Fatalf("reportLineLimit(polecat) = %v, want the refusal text", polecatErr)
	}
	if !strings.Contains(polecatOut.String(), refusal) || polecatErrOut.Len() != 0 {
		t.Errorf("polecat out %q, errOut %q; want the refusal on out and nothing on errOut", polecatOut.String(), polecatErrOut.String())
	}
	if crewErr != nil || crewOut.Len() != 0 || !strings.Contains(crewErrOut.String(), crewWarningPrefix+refusal) {
		t.Errorf("crew err %v, out %q, errOut %q; want no error, nothing on out and the prefixed warning on errOut", crewErr, crewOut.String(), crewErrOut.String())
	}
	if noLimitErr != nil || countCalled || !strings.Contains(noLimitOut.String(), noLimitLine) {
		t.Errorf("no-limit err %v, count called %v, out %q; want no error, no count and the no-limit line", noLimitErr, countCalled, noLimitOut.String())
	}
}

// TestLineLimitReportCountError: a count that cannot be read is returned
// wrapped as added-line check, never ignored and never printed (gt-zqg4m2).
func TestLineLimitReportCountError(t *testing.T) {
	t.Parallel()
	want := errors.New("numstat failed")
	var out, errOut bytes.Buffer
	err := reportLineLimit(&out, &errOut, func() (int, error) { return 0, want }, "Change: at most 70 added lines in 2 files.", false)
	if !errors.Is(err, want) || !strings.HasPrefix(err.Error(), "added-line check: ") {
		t.Errorf("count error = %v, want %q", err, "added-line check: numstat failed")
	}
	if out.Len() != 0 || errOut.Len() != 0 {
		t.Errorf("count error printed out %q, errOut %q; want nothing printed", out.String(), errOut.String())
	}
}
