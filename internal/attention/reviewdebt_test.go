package attention

import "testing"

// The one rule that sets a risk-path item's severity (gt-zevjx): review debt is
// low, and a failed review of a P1 landing — the lane the town's own machinery
// lands in — is the one risk-path item that stays high. An unreadable bead is
// never the P1 case.
func TestRiskPathSeverity(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		ev   RiskPathEvidence
		want Severity
	}{
		{"an unreviewed landing is review debt", RiskPathEvidence{Priority: 1}, SeverityLow},
		{"an unreviewed P1 landing is still review debt", RiskPathEvidence{Priority: 0}, SeverityLow},
		{"a failed review of a P2 landing is review debt", RiskPathEvidence{Failed: true, Priority: 2}, SeverityLow},
		{"a failed review of the slowest landing is review debt", RiskPathEvidence{Failed: true, Priority: 4}, SeverityLow},
		{"a failed review of an unreadable bead is review debt", RiskPathEvidence{Failed: true, Priority: -1}, SeverityLow},
		{"a failed review of a P1 landing is an incident", RiskPathEvidence{Failed: true, Priority: 1}, SeverityHigh},
		{"a failed review of a P0 landing is an incident", RiskPathEvidence{Failed: true, Priority: 0}, SeverityHigh},
	}
	for _, tc := range cases {
		if got := RiskPathSeverity(tc.ev); got != tc.want {
			t.Errorf("%s: RiskPathSeverity(%+v) = %q, want %q", tc.name, tc.ev, got, tc.want)
		}
	}
}

// The rule picks between the queue's two severities, never a third: `gt tail`'s
// watch source reads events.jsonl and drops a line whose severity is not low or
// high, so a risk-path line with a severity of its own would vanish from the
// feed.
func TestRiskPathSeverityIsOneOfTwoSeverities(t *testing.T) {
	t.Parallel()

	for _, ev := range []RiskPathEvidence{
		{}, {Failed: true}, {Priority: 0}, {Failed: true, Priority: 0},
	} {
		if got := RiskPathSeverity(ev); got != SeverityLow && got != SeverityHigh {
			t.Errorf("RiskPathSeverity(%+v) = %q, want low or high", ev, got)
		}
	}
}
