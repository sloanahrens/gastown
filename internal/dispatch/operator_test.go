package dispatch

import (
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// TestOperatorReservation pins the two records that reserve a bead for the
// human operator (gt-21pl0): the operator label, and an assignee that is not an
// agent address. The human assignees are the handles the town's own beads carry
// today, and the agent addresses are the forms every dispatch path writes.
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

		{name: "bare handle is a person", assignee: "ada", want: "assignee ada is not an agent address"},
		{name: "a name with a space is a person", assignee: "Test Operator", want: "assignee Test Operator is not an agent address"},
		{name: "overseer is a person", assignee: "overseer", want: "assignee overseer is not an agent address"},

		{name: "polecat address", assignee: "gastown/polecats/onyx"},
		{name: "crew address", assignee: "gastown/crew/operator"},
		{name: "witness address", assignee: "gastown/witness"},
		{name: "dog address", assignee: "deacon/dogs/rex"},
		// The mayor's seat was retired (gt-rwp7z.11). Its slash-qualified
		// address still parses as an agent address, but the bare older
		// spelling no longer names one, so a bead left on it reads as the
		// operator's and stays held until someone re-addresses it.
		{name: "retired seat's address", assignee: "mayor/"},
		{name: "retired seat's older spelling", assignee: "mayor", want: "assignee mayor is not an agent address"},
		{name: "deacon", assignee: "deacon/"},
		{name: "deacon, older spelling", assignee: "deacon"},

		// The label wins over an agent assignee: the operator's own mark is the
		// stronger record, and it is the one they can put on a bead an agent
		// already holds.
		{name: "label on an agent-held bead", labels: []string{"operator"}, assignee: "gastown/polecats/onyx", want: "label operator"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := OperatorReservation(tt.labels, tt.assignee, nil); got != tt.want {
				t.Errorf("OperatorReservation(%v, %q) = %q, want %q", tt.labels, tt.assignee, got, tt.want)
			}
		})
	}
}

// TestOperatorReservationUsesSettingsNames: an assignee equal to a name the
// town settings list in operator.names is the operator's, whether the name is
// bare or carries a slash, while an unlisted bare name follows the
// bare-address rule (gt-ywuip).
func TestOperatorReservationUsesSettingsNames(t *testing.T) {
	t.Parallel()
	op := &config.OperatorConfig{Names: []string{"Test Operator", "operator/handle"}}
	tests := []struct {
		name     string
		assignee string
		want     string
	}{
		{name: "a listed bare name", assignee: "Test Operator", want: "assignee Test Operator is the operator"},
		{name: "a listed name padded with spaces", assignee: "  Test Operator  ", want: "assignee Test Operator is the operator"},
		// A listed name with a slash is the added rule: without the setting it
		// would parse as an agent address and the bead would be dispatchable.
		{name: "a listed name carrying a slash", assignee: "operator/handle", want: "assignee operator/handle is the operator"},
		{name: "an unlisted bare name follows today's rule", assignee: "someone", want: "assignee someone is not an agent address"},
		{name: "an unlisted agent address is still an agent", assignee: "gastown/polecats/onyx", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := OperatorReservation(nil, tt.assignee, op); got != tt.want {
				t.Errorf("OperatorReservation(nil, %q) = %q, want %q", tt.assignee, got, tt.want)
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
