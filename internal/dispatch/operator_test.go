package dispatch

import "testing"

// TestOperatorReservation pins the two records that reserve a bead for the
// human operator (gt-21pl0). The human assignees are the ones the town's own
// beads carry today — `sloan` on gt-nj23.9, `Sloan Ahrens` and `overseer`
// elsewhere — and the agent addresses are the forms every dispatch path writes.
func TestOperatorReservation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		labels   []string
		assignee string
		want     string
	}{
		{name: "unassigned and unlabelled", want: ""},
		{name: "label", labels: []string{"operator"}, want: "label operator"},
		{name: "label, however typed", labels: []string{"  Operator "}, want: "label operator"},
		{name: "another label", labels: []string{"run-blocker"}, want: ""},

		{name: "bare handle is a person", assignee: "sloan", want: "assignee sloan is not an agent address"},
		{name: "a name with a space is a person", assignee: "Sloan Ahrens", want: "assignee Sloan Ahrens is not an agent address"},
		{name: "overseer is a person", assignee: "overseer", want: "assignee overseer is not an agent address"},

		{name: "polecat address", assignee: "gastown/polecats/onyx"},
		{name: "crew address", assignee: "gastown/crew/sloan"},
		{name: "witness address", assignee: "gastown/witness"},
		{name: "dog address", assignee: "deacon/dogs/boot"},
		{name: "mayor", assignee: "mayor/"},
		{name: "mayor, older spelling", assignee: "mayor"},
		{name: "deacon", assignee: "deacon/"},
		{name: "deacon, older spelling", assignee: "deacon"},

		// The label wins over an agent assignee: the operator's own mark is the
		// stronger record, and it is the one they can put on a bead an agent
		// already holds.
		{name: "label on an agent-held bead", labels: []string{"operator"}, assignee: "gastown/polecats/onyx", want: "label operator"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := OperatorReservation(tt.labels, tt.assignee); got != tt.want {
				t.Errorf("OperatorReservation(%v, %q) = %q, want %q", tt.labels, tt.assignee, got, tt.want)
			}
		})
	}
}

// TestOperatorLabelIsTheWrittenSpelling pins the label the operator types onto
// a bead. The rule matches it case-insensitively, so only the constant's value
// is contractual — beads written by hand carry this spelling.
func TestOperatorLabelIsTheWrittenSpelling(t *testing.T) {
	t.Parallel()
	if OperatorLabel != "operator" {
		t.Fatalf("OperatorLabel = %q", OperatorLabel)
	}
}
