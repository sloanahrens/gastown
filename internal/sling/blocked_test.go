package sling

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/dispatch"
)

// blockedTestRig is the rig name every stub below answers for.
const blockedTestRig = "dag"

// estopAnswer is an estopOn stub that reports one state for every rig.
func estopAnswer(on bool, err error) func(townRoot, rigName string) (bool, error) {
	return func(string, string) (bool, error) { return on, err }
}

// parkedAnswer is a parked stub that reports reason for every rig; an empty
// reason is a rig that is neither parked nor docked.
func parkedAnswer(reason string) func(townRoot, rigName string) (bool, string) {
	return func(string, string) (bool, string) { return reason != "", reason }
}

// TestBlockedRefusalsRoundTripThroughSlingRefusalLine pins the contract between
// Blocked's refusal text and dispatch.SlingRefusalLine (gt-vufiq). An automatic
// dispatcher reads a subprocess's output, so it cannot see ErrRigUnavailable:
// its only evidence that a rig-parked sling is a deferral rather than a failure
// is the sentence Blocked prints. Nothing else ties Blocked's format strings to
// dispatch's rigUnavailableLine, so a reword on either side would turn every
// dispatch to a parked rig into a counted failure. Feeding each refusal back
// through the predicate makes that reword fail here instead.
//
// The predicate returns the refusal line, not the whole message: Blocked appends
// the command that clears the state on a following line, which belongs to the
// operator and not to the deferral.
func TestBlockedRefusalsRoundTripThroughSlingRefusalLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		estop  func(townRoot, rigName string) (bool, error)
		parked func(townRoot, rigName string) (bool, string)
		label  string
	}{
		{
			name:   "e-stop active",
			estop:  estopAnswer(true, nil),
			parked: parkedAnswer(""),
			label:  "e-stop",
		},
		{
			// estop.On reports the state and the read's error separately, and
			// Blocked prints the error as the reason.
			name:   "e-stop whose state could not be read",
			estop:  estopAnswer(true, errors.New("estop state unreadable")),
			parked: parkedAnswer(""),
			label:  "e-stop",
		},
		{
			name:   "parked rig",
			estop:  estopAnswer(false, nil),
			parked: parkedAnswer("parked"),
			label:  "rig parked",
		},
		{
			name:   "docked rig",
			estop:  estopAnswer(false, nil),
			parked: parkedAnswer("docked"),
			label:  "rig docked",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			label, err := Blocked("/town", blockedTestRig, tt.estop, tt.parked)
			if err == nil {
				t.Fatal("Blocked returned no refusal")
			}
			if label != tt.label {
				t.Errorf("label = %q; want %q", label, tt.label)
			}
			if !errors.Is(err, ErrRigUnavailable) {
				t.Errorf("error %q does not wrap ErrRigUnavailable", err)
			}

			refusal, _, _ := strings.Cut(err.Error(), "\n")
			// "Error: " is cobra's prefix on the message a caller's stderr
			// carries, which is the form the dispatcher reads.
			for _, text := range []string{err.Error(), "Error: " + err.Error()} {
				got, ok := dispatch.SlingRefusalLine(text)
				if !ok {
					t.Fatalf("SlingRefusalLine(%q) found no refusal; Blocked and the dispatch pattern have drifted", text)
				}
				if got != refusal {
					t.Errorf("SlingRefusalLine(%q) = %q; want Blocked's refusal line %q", text, got, refusal)
				}
			}
		})
	}
}
