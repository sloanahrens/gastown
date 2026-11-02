package events

import (
	"strings"
	"testing"
)

// TestDoneEventIsSubmission pins which done events are submissions: one written
// before gt done recorded the exit type, and one whose exit type is the
// completed exit. An escalated or deferred exit submits nothing, and a status
// the package cannot read or does not know is not read as a submission and is
// not known either, so a reader that must rule out a duplicate can refuse
// (gt-qqmae).
func TestDoneEventIsSubmission(t *testing.T) {
	t.Parallel()
	unknown := strings.ToLower(DoneStatusEscalated)
	tests := []struct {
		name      string
		payload   map[string]interface{}
		wantSub   bool
		wantKnown bool
	}{
		{"no status: written before the field existed", map[string]interface{}{"bead": "gt-a"}, true, true},
		{"an empty status", map[string]interface{}{"status": ""}, true, true},
		{"the completed exit", map[string]interface{}{"status": DoneStatusCompleted}, true, true},
		{"the escalated exit", map[string]interface{}{"status": DoneStatusEscalated}, false, true},
		{"the deferred exit", map[string]interface{}{"status": DoneStatusDeferred}, false, true},
		{"a status the package does not know", map[string]interface{}{"status": unknown}, false, false},
		{"a status that is not a string", map[string]interface{}{"status": 3}, false, false},
		{"a null status", map[string]interface{}{"status": nil}, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ev := Event{Type: TypeDone, Payload: tc.payload}
			if got := DoneEventIsSubmission(ev); got != tc.wantSub {
				t.Errorf("DoneEventIsSubmission(%v) = %v, want %v", tc.payload, got, tc.wantSub)
			}
			if got := DoneEventStatusKnown(ev); got != tc.wantKnown {
				t.Errorf("DoneEventStatusKnown(%v) = %v, want %v", tc.payload, got, tc.wantKnown)
			}
		})
	}
}
