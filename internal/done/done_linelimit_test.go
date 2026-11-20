package done

import (
	"bytes"
	"errors"
	"os"
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

// TestLineLimitReportRefusesPolecatOnly: a polecat past twice the declared
// limit is refused with the verdict text, a crew branch is given that text and
// no error, and no declared limit means the count is never read (gt-zqg4m2).
// parallel-global-ok: the only test that swaps lineLimitOut, and no peer test in package done calls reportLineLimit.
func TestLineLimitReportRefusesPolecatOnly(t *testing.T) {
	t.Parallel()
	const desc = "Change: at most 70 added lines in 2 files."
	refusal := "Change exceeds the 70-added-line constraint: 141 added lines against 70 declared"
	var printed bytes.Buffer
	//testpolicy:allow no-global-swap — lineLimitOut is the seam reportLineLimit prints through, and this test reads it
	lineLimitOut = &printed
	t.Cleanup(func() {
		//testpolicy:allow no-global-swap — restores the seam swapped above
		lineLimitOut = os.Stdout
	})
	var polecatErr, crewErr, noLimitErr error
	countCalled := false
	polecatErr = reportLineLimit(func() (int, error) { return 141, nil }, desc, true)
	crewErr = reportLineLimit(func() (int, error) { return 141, nil }, desc, false)
	noLimitErr = reportLineLimit(func() (int, error) { countCalled = true; return 0, nil }, "Change: at most 2 files.", false)
	if polecatErr == nil || !strings.Contains(polecatErr.Error(), refusal) || !strings.Contains(polecatErr.Error(), splitLine) {
		t.Fatalf("reportLineLimit(polecat) = %v, want the refusal text", polecatErr)
	}
	if crewErr != nil || noLimitErr != nil || countCalled || strings.Count(printed.String(), refusal) != 2 || !strings.Contains(printed.String(), noLimitLine) {
		t.Errorf("crew %v, no-limit %v, count called %v, printed %q; want no errors, no count, the refusal twice and the no-limit line", crewErr, noLimitErr, countCalled, printed.String())
	}
}

// TestLineLimitReportCountError: a count that cannot be read is returned
// wrapped as added-line check, never ignored (gt-zqg4m2).
func TestLineLimitReportCountError(t *testing.T) {
	t.Parallel()
	want := errors.New("numstat failed")
	err := reportLineLimit(func() (int, error) { return 0, want }, "Change: at most 70 added lines in 2 files.", false)
	if !errors.Is(err, want) || !strings.HasPrefix(err.Error(), "added-line check: ") {
		t.Errorf("count error = %v, want %q", err, "added-line check: numstat failed")
	}
}
